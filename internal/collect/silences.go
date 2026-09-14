package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

// FetchSilences reads every silence Alertmanager still knows about, expired
// ones included. An expired silence is the most useful kind here: it is a
// human on the record saying an alert was not worth hearing.
func FetchSilences(ctx context.Context, amURL string, hc *http.Client) ([]store.Silence, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	url := strings.TrimSuffix(amURL, "/") + "/api/v2/silences"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build silences request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch silences: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch silences: alertmanager returned %s", resp.Status)
	}

	var payload []struct {
		ID        string          `json:"id"`
		CreatedBy string          `json:"createdBy"`
		Comment   string          `json:"comment"`
		StartsAt  time.Time       `json:"startsAt"`
		EndsAt    time.Time       `json:"endsAt"`
		Matchers  []store.Matcher `json:"matchers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode silences: %w", err)
	}

	out := make([]store.Silence, 0, len(payload))
	for _, p := range payload {
		out = append(out, store.Silence{
			AMID:      p.ID,
			Matchers:  p.Matchers,
			CreatedBy: p.CreatedBy,
			Comment:   p.Comment,
			StartsAt:  p.StartsAt.UTC(),
			EndsAt:    p.EndsAt.UTC(),
		})
	}
	return out, nil
}

// matcherRegexes caches compiled matcher patterns. SilencedSeconds runs once
// per episode and evaluates every matcher of every silence, so a compile per
// call is a compile per (episode x silence x matcher): on a plausible install
// -- tens of thousands of episodes against a few hundred silences -- that is
// millions of identical compilations inside the scan's timeout budget.
//
// Keyed by the raw pattern, and the compile failure is cached too, so an
// invalid pattern is not re-attempted for every episode either.
var matcherRegexes sync.Map // string -> compiledMatcher

type compiledMatcher struct {
	re  *regexp.Regexp
	err error
}

func matcherRegex(pattern string) (*regexp.Regexp, error) {
	if v, ok := matcherRegexes.Load(pattern); ok {
		c := v.(compiledMatcher)
		return c.re, c.err
	}
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	if err != nil {
		err = fmt.Errorf("invalid matcher regex %q: %w", pattern, err)
	}
	matcherRegexes.Store(pattern, compiledMatcher{re: re, err: err})
	return re, err
}

// MatcherMatches applies Alertmanager matcher semantics. A missing label is
// the empty string, and regexes are fully anchored.
func MatcherMatches(m store.Matcher, labels map[string]string) (bool, error) {
	value := labels[m.Name]

	if !m.IsRegex {
		if m.IsEqual {
			return value == m.Value, nil
		}
		return value != m.Value, nil
	}

	re, err := matcherRegex(m.Value)
	if err != nil {
		// Fail closed: an unparseable matcher cannot be shown to cover
		// anything, and must never be read as a match.
		return false, err
	}
	matched := re.MatchString(value)
	if m.IsEqual {
		return matched, nil
	}
	return !matched, nil
}

// SilenceMatches reports whether a silence covers an alert. Every matcher must
// match, which is Alertmanager's own rule. alertname is supplied separately
// because episode labels have it stripped.
func SilenceMatches(s store.Silence, alertName string, labels map[string]string) bool {
	full := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		full[k] = v
	}
	full["alertname"] = alertName

	for _, m := range s.Matchers {
		ok, err := MatcherMatches(m, full)
		if err != nil || !ok {
			// A silence with an unparseable matcher cannot be shown to cover
			// anything, so treat it as non-matching rather than guess.
			return false
		}
	}
	return len(s.Matchers) > 0
}

// SilencedSeconds returns how much of an episode was covered by matching
// silences. Overlapping silences are unioned, never summed, so coverage can
// never exceed the episode.
func SilencedSeconds(start, end time.Time, sils []store.Silence, alertName string, labels map[string]string) float64 {
	type span struct{ from, to time.Time }

	var spans []span
	for _, s := range sils {
		if !SilenceMatches(s, alertName, labels) {
			continue
		}
		from, to := s.StartsAt, s.EndsAt
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		if !to.After(from) {
			continue
		}
		spans = append(spans, span{from, to})
	}
	if len(spans) == 0 {
		return 0
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].from.Before(spans[j].from) })

	var total float64
	cur := spans[0]
	for _, s := range spans[1:] {
		if s.from.After(cur.to) {
			total += cur.to.Sub(cur.from).Seconds()
			cur = s
			continue
		}
		if s.to.After(cur.to) {
			cur.to = s.to
		}
	}
	total += cur.to.Sub(cur.from).Seconds()
	return total
}
