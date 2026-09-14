package collect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/store"
)

func TestMatcherMatches(t *testing.T) {
	labels := map[string]string{"severity": "page", "instance": "web-1"}

	cases := []struct {
		name    string
		matcher store.Matcher
		want    bool
	}{
		{"equal hit", store.Matcher{Name: "severity", Value: "page", IsEqual: true}, true},
		{"equal miss", store.Matcher{Name: "severity", Value: "warn", IsEqual: true}, false},
		{"not-equal hit", store.Matcher{Name: "severity", Value: "warn", IsEqual: false}, true},
		{"not-equal miss", store.Matcher{Name: "severity", Value: "page", IsEqual: false}, false},
		{"regex hit", store.Matcher{Name: "instance", Value: "web-.*", IsRegex: true, IsEqual: true}, true},
		{"regex miss", store.Matcher{Name: "instance", Value: "db-.*", IsRegex: true, IsEqual: true}, false},
		{"regex anchored", store.Matcher{Name: "instance", Value: "web", IsRegex: true, IsEqual: true}, false},
		{"negative regex", store.Matcher{Name: "instance", Value: "db-.*", IsRegex: true, IsEqual: false}, true},
		{"missing label is empty", store.Matcher{Name: "team", Value: "", IsEqual: true}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := MatcherMatches(c.matcher, labels)
			if err != nil {
				t.Fatalf("MatcherMatches: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestMatcherRejectsBadRegex(t *testing.T) {
	_, err := MatcherMatches(
		store.Matcher{Name: "a", Value: "[", IsRegex: true, IsEqual: true},
		map[string]string{"a": "x"})
	if err == nil {
		t.Fatal("bad regex accepted, want error")
	}
}

func TestSilenceMatchesRequiresAllMatchers(t *testing.T) {
	sil := store.Silence{Matchers: []store.Matcher{
		{Name: "alertname", Value: "DemoSpiky", IsEqual: true},
		{Name: "severity", Value: "warning", IsEqual: true},
	}}

	if !SilenceMatches(sil, "DemoSpiky", map[string]string{"severity": "warning"}) {
		t.Error("all matchers satisfied, should match")
	}
	if SilenceMatches(sil, "DemoSpiky", map[string]string{"severity": "page"}) {
		t.Error("one matcher failed, should not match")
	}
	if SilenceMatches(sil, "Other", map[string]string{"severity": "warning"}) {
		t.Error("alertname differs, should not match")
	}
}

func TestSilencedSecondsCountsOverlapOnly(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	sil := store.Silence{
		Matchers: []store.Matcher{{Name: "alertname", Value: "A", IsEqual: true}},
		StartsAt: base.Add(30 * time.Minute),
		EndsAt:   base.Add(90 * time.Minute),
	}

	// Episode runs 0..60m, silence covers 30..90m, so 30m overlap.
	got := SilencedSeconds(base, base.Add(time.Hour), []store.Silence{sil}, "A", nil)
	if want := (30 * time.Minute).Seconds(); got != want {
		t.Errorf("silenced seconds = %v, want %v", got, want)
	}
}

func TestSilencedSecondsIgnoresNonMatchingSilence(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	sil := store.Silence{
		Matchers: []store.Matcher{{Name: "alertname", Value: "B", IsEqual: true}},
		StartsAt: base,
		EndsAt:   base.Add(time.Hour),
	}
	if got := SilencedSeconds(base, base.Add(time.Hour), []store.Silence{sil}, "A", nil); got != 0 {
		t.Errorf("silenced seconds = %v, want 0 for a non-matching silence", got)
	}
}

func TestSilencedSecondsDoesNotDoubleCountOverlappingSilences(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	mk := func(from, to time.Duration) store.Silence {
		return store.Silence{
			Matchers: []store.Matcher{{Name: "alertname", Value: "A", IsEqual: true}},
			StartsAt: base.Add(from), EndsAt: base.Add(to),
		}
	}
	sils := []store.Silence{mk(0, 40*time.Minute), mk(20*time.Minute, time.Hour)}

	got := SilencedSeconds(base, base.Add(time.Hour), sils, "A", nil)
	if want := time.Hour.Seconds(); got != want {
		t.Errorf("silenced seconds = %v, want %v (union, not sum)", got, want)
	}
}

func TestFetchSilencesParsesResponse(t *testing.T) {
	body := `[{"id":"sil-1","createdBy":"sai","comment":"noisy",
	  "startsAt":"2023-11-14T22:13:20Z","endsAt":"2023-11-15T22:13:20Z",
	  "matchers":[{"name":"alertname","value":"DemoSpiky","isRegex":false,"isEqual":true}],
	  "status":{"state":"expired"}}]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/silences" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := FetchSilences(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("FetchSilences: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d silences, want 1", len(got))
	}
	if got[0].AMID != "sil-1" || got[0].CreatedBy != "sai" {
		t.Errorf("bad silence: %+v", got[0])
	}
	if len(got[0].Matchers) != 1 || !got[0].Matchers[0].IsEqual {
		t.Errorf("matchers not parsed: %+v", got[0].Matchers)
	}
}
