// Package webhook is the Alertmanager webhook receiver (issue #13): the
// write path that captures what the ALERTS series backfill cannot see at
// all -- receiver, grouping, annotations as rendered at fire time,
// generatorURL, and sub-step fidelity for alerts that fire and resolve
// inside one query step.
//
// It is strictly additive. `noisefloor scan`'s backfill from ALERTS
// remains sufficient on its own for a useful first scan; nothing in this
// package is required for that to keep being true. See the package doc
// comment on reconciliation in internal/store.SQLite.UpsertWebhookEpisode
// for how a firing observed by both paths becomes one episode.
package webhook

import (
	"fmt"
	"time"
)

// Payload is Alertmanager's webhook notification body -- the JSON shape
// documented at https://prometheus.io/docs/alerting/latest/configuration/#webhook_config
// and stable across Alertmanager's webhook message version "4".
type Payload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	TruncatedAlerts   int               `json:"truncatedAlerts"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []Alert           `json:"alerts"`
}

// Alert is one alert within a Payload -- Alertmanager groups one or more
// of these into a single notification per Payload.GroupLabels.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Bounds on an untrusted payload. Alert names, labels and annotations
// arriving here are attacker-controlled if this endpoint is reachable, and
// they flow into the same store the UI renders from -- the UI escapes on
// output, which is the right place for that, but a malformed or hostile
// payload must not be able to corrupt the database or crash the collector
// by, say, claiming ten million labels. The request body itself is already
// bounded (see config.Webhook.MaxBodyBytes / http.MaxBytesReader), so these
// are defence in depth against a small body packed with many small fields
// rather than the primary control.
const (
	MaxAlertsPerPayload = 500
	MaxLabelsPerAlert   = 200
	MaxLabelKeyLen      = 256
	MaxLabelValueLen    = 4096
	MaxAnnotationLen    = 16384
	MaxReceiverLen      = 512
	MaxGroupKeyLen      = 2048
	MaxURLLen           = 2048
)

const (
	statusFiring   = "firing"
	statusResolved = "resolved"
)

// MaxFutureSkew bounds how far into the future startsAt/endsAt may sit
// relative to this process's own clock. Validate already catches
// end-before-start and zero timestamps, but neither catches a timestamp
// that is simply wrong in the other direction: a resolved alert with
// endsAt in year 9999 passes both and writes an eight-thousand-year
// episode that poisons P50Duration and can hand a rule automate. Generous
// enough to absorb ordinary clock drift between Alertmanager and the
// collector, far too small for anything a real notification would ever
// send.
const MaxFutureSkew = 24 * time.Hour

// Validate rejects a payload noisefloor should not act on at all: wrong
// shape, missing required fields, or sizes far outside anything a real
// Alertmanager notification would ever send. It does not mutate p.
//
// It deliberately does not require CommonLabels, CommonAnnotations or
// TruncatedAlerts to be present or sensible -- noisefloor never reads them
// -- and does not validate Version, since Alertmanager has shipped webhook
// message version "4" for years and a future minor addition to the schema
// should not start rejecting requests.
func (p Payload) Validate() error {
	if len(p.Receiver) > MaxReceiverLen {
		return fmt.Errorf("receiver exceeds %d bytes", MaxReceiverLen)
	}
	if len(p.GroupKey) > MaxGroupKeyLen {
		return fmt.Errorf("groupKey exceeds %d bytes", MaxGroupKeyLen)
	}
	if len(p.ExternalURL) > MaxURLLen {
		return fmt.Errorf("externalURL exceeds %d bytes", MaxURLLen)
	}
	if p.Status != statusFiring && p.Status != statusResolved {
		return fmt.Errorf("status must be %q or %q, got %q", statusFiring, statusResolved, p.Status)
	}
	if len(p.Alerts) == 0 {
		return fmt.Errorf("alerts is empty")
	}
	if len(p.Alerts) > MaxAlertsPerPayload {
		return fmt.Errorf("alerts has %d entries, exceeds max %d", len(p.Alerts), MaxAlertsPerPayload)
	}
	if err := validateLabelMap("groupLabels", p.GroupLabels); err != nil {
		return err
	}
	for i, a := range p.Alerts {
		if err := a.validate(); err != nil {
			return fmt.Errorf("alerts[%d]: %w", i, err)
		}
	}
	return nil
}

func (a Alert) validate() error {
	if a.Status != statusFiring && a.Status != statusResolved {
		return fmt.Errorf("status must be %q or %q, got %q", statusFiring, statusResolved, a.Status)
	}
	if a.Labels["alertname"] == "" {
		return fmt.Errorf("labels.alertname is required")
	}
	if len(a.GeneratorURL) > MaxURLLen {
		return fmt.Errorf("generatorURL exceeds %d bytes", MaxURLLen)
	}
	if a.StartsAt.IsZero() {
		return fmt.Errorf("startsAt is required")
	}
	maxFuture := time.Now().Add(MaxFutureSkew)
	if a.StartsAt.After(maxFuture) {
		return fmt.Errorf("startsAt (%s) is more than %s in the future",
			a.StartsAt.Format(time.RFC3339), MaxFutureSkew)
	}
	if a.Status == statusResolved {
		if a.EndsAt.IsZero() {
			return fmt.Errorf("endsAt is required for a resolved alert")
		}
		if a.EndsAt.Before(a.StartsAt) {
			return fmt.Errorf("endsAt (%s) is before startsAt (%s)",
				a.EndsAt.Format(time.RFC3339), a.StartsAt.Format(time.RFC3339))
		}
		if a.EndsAt.After(maxFuture) {
			return fmt.Errorf("endsAt (%s) is more than %s in the future",
				a.EndsAt.Format(time.RFC3339), MaxFutureSkew)
		}
	}
	if err := validateLabelMap("labels", a.Labels); err != nil {
		return err
	}
	if err := validateAnnotationMap("annotations", a.Annotations); err != nil {
		return err
	}
	return nil
}

func validateLabelMap(field string, m map[string]string) error {
	if len(m) > MaxLabelsPerAlert {
		return fmt.Errorf("%s has %d entries, exceeds max %d", field, len(m), MaxLabelsPerAlert)
	}
	for k, v := range m {
		if len(k) > MaxLabelKeyLen {
			return fmt.Errorf("%s: key exceeds %d bytes", field, MaxLabelKeyLen)
		}
		if len(v) > MaxLabelValueLen {
			return fmt.Errorf("%s[%q]: value exceeds %d bytes", field, k, MaxLabelValueLen)
		}
	}
	return nil
}

func validateAnnotationMap(field string, m map[string]string) error {
	if len(m) > MaxLabelsPerAlert {
		return fmt.Errorf("%s has %d entries, exceeds max %d", field, len(m), MaxLabelsPerAlert)
	}
	for k, v := range m {
		if len(k) > MaxLabelKeyLen {
			return fmt.Errorf("%s: key exceeds %d bytes", field, MaxLabelKeyLen)
		}
		if len(v) > MaxAnnotationLen {
			return fmt.Errorf("%s[%q]: value exceeds %d bytes", field, k, MaxAnnotationLen)
		}
	}
	return nil
}
