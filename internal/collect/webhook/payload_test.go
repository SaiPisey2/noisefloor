package webhook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestValidateGroupedMultiAlertFixture(t *testing.T) {
	var p Payload
	if err := json.Unmarshal(loadFixture(t, "grouped_multialert.json"), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(p.Alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(p.Alerts))
	}
	if p.Receiver != "team-pager" {
		t.Errorf("Receiver = %q, want team-pager", p.Receiver)
	}
	if p.Alerts[0].Labels["instance"] == p.Alerts[1].Labels["instance"] {
		t.Errorf("fixture alerts must differ by instance to exercise grouping")
	}
}

func TestValidateResolvedFixture(t *testing.T) {
	var p Payload
	if err := json.Unmarshal(loadFixture(t, "resolved.json"), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p.Status != statusResolved {
		t.Errorf("Status = %q, want resolved", p.Status)
	}
	a := p.Alerts[0]
	if a.EndsAt.Before(a.StartsAt) || a.EndsAt.Equal(a.StartsAt) {
		t.Errorf("resolved fixture must have endsAt strictly after startsAt")
	}
}

func TestValidateMalformedMissingAlertnameFixture(t *testing.T) {
	var p Payload
	if err := json.Unmarshal(loadFixture(t, "malformed_missing_alertname.json"), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded on a payload with no alertname label, want error")
	}
}

func TestValidateTruncatedJSONFailsToDecode(t *testing.T) {
	var p Payload
	err := json.Unmarshal(loadFixture(t, "malformed_truncated.json"), &p)
	if err == nil {
		t.Fatal("json.Unmarshal succeeded on truncated JSON, want error")
	}
}

func TestValidateRejectsEmptyAlerts(t *testing.T) {
	p := Payload{Status: statusFiring, Receiver: "r"}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded with no alerts, want error")
	}
}

func TestValidateRejectsUnknownStatus(t *testing.T) {
	p := Payload{
		Status: "pending", Receiver: "r",
		Alerts: []Alert{{Status: statusFiring, Labels: map[string]string{"alertname": "X"}, StartsAt: time.Now()}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded with an unknown top-level status, want error")
	}
}

func TestValidateAlertRejectsResolvedWithoutEndsAt(t *testing.T) {
	p := Payload{
		Status: statusResolved, Receiver: "r",
		Alerts: []Alert{{
			Status: statusResolved, Labels: map[string]string{"alertname": "X"},
			StartsAt: time.Now(),
		}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded on a resolved alert with no endsAt, want error")
	}
}

func TestValidateAlertRejectsEndsAtBeforeStartsAt(t *testing.T) {
	now := time.Now()
	p := Payload{
		Status: statusResolved, Receiver: "r",
		Alerts: []Alert{{
			Status: statusResolved, Labels: map[string]string{"alertname": "X"},
			StartsAt: now, EndsAt: now.Add(-time.Minute),
		}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded with endsAt before startsAt, want error")
	}
}

func TestValidateRejectsTooManyAlerts(t *testing.T) {
	alerts := make([]Alert, MaxAlertsPerPayload+1)
	for i := range alerts {
		alerts[i] = Alert{Status: statusFiring, Labels: map[string]string{"alertname": "X"}, StartsAt: time.Now()}
	}
	p := Payload{Status: statusFiring, Receiver: "r", Alerts: alerts}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded with too many alerts, want error")
	}
}

func TestValidateRejectsOversizedAnnotation(t *testing.T) {
	huge := make([]byte, MaxAnnotationLen+1)
	for i := range huge {
		huge[i] = 'x'
	}
	p := Payload{
		Status: statusFiring, Receiver: "r",
		Alerts: []Alert{{
			Status: statusFiring, Labels: map[string]string{"alertname": "X"},
			Annotations: map[string]string{"summary": string(huge)},
			StartsAt:    time.Now(),
		}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded with an oversized annotation, want error")
	}
}

func TestValidateRejectsTooManyLabels(t *testing.T) {
	labels := map[string]string{"alertname": "X"}
	for i := 0; i < MaxLabelsPerAlert+1; i++ {
		labels[string(rune('a'+i%26))+string(rune(i))] = "v"
	}
	p := Payload{
		Status: statusFiring, Receiver: "r",
		Alerts: []Alert{{Status: statusFiring, Labels: labels, StartsAt: time.Now()}},
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate succeeded with too many labels, want error")
	}
}
