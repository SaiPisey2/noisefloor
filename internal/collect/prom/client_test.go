package prom

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

func newTestAPI(t *testing.T, handler http.HandlerFunc) *API {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	api, err := New(config.Prometheus{
		URL:   srv.URL,
		Step:  config.Duration(time.Minute),
		Chunk: config.Duration(6 * time.Hour),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return api
}

func TestQueryRangeParsesMatrix(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"__name__":"ALERTS","alertname":"A","alertstate":"firing"},
	   "values":[[1700000000,"1"],[1700000060,"1"]]}]}}`

	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	m, err := api.QueryRange(context.Background(), "ALERTS",
		time.Unix(1_700_000_000, 0), time.Unix(1_700_000_120, 0), time.Minute)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(m) != 1 {
		t.Fatalf("got %d series, want 1", len(m))
	}
	if got := string(m[0].Metric["alertname"]); got != "A" {
		t.Errorf("alertname = %q, want A", got)
	}
	if len(m[0].Values) != 2 {
		t.Errorf("got %d samples, want 2", len(m[0].Values))
	}
}

func TestRulesParsesAlertingRules(t *testing.T) {
	body := `{"status":"success","data":{"groups":[
	  {"name":"demo","file":"/etc/prometheus/rules/demo.yml","rules":[
	    {"type":"alerting","name":"DemoSpiky","query":"demo_spiky_gauge > 0",
	     "duration":0,"labels":{"severity":"warning"},
	     "annotations":{"summary":"s"},"health":"ok"},
	    {"type":"recording","name":"job:x","query":"sum(x)"}
	  ]}]}}`

	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	groups, err := api.Rules(context.Background())
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if len(groups[0].Alerting) != 1 {
		t.Fatalf("got %d alerting rules, want 1 (recording rules must be dropped)",
			len(groups[0].Alerting))
	}
	r := groups[0].Alerting[0]
	if r.Name != "DemoSpiky" {
		t.Errorf("name = %q, want DemoSpiky", r.Name)
	}
	if r.Labels["severity"] != "warning" {
		t.Errorf("labels = %v", r.Labels)
	}
	if groups[0].File != "/etc/prometheus/rules/demo.yml" {
		t.Errorf("file = %q", groups[0].File)
	}
}

func TestRetentionFloorUsesEarliestSampleInRange(t *testing.T) {
	// Data begins partway into the requested window: the floor is where the
	// data starts, not where the request started.
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{},"values":[[1699000000,"2"],[1699003600,"2"],[1699007200,"2"]]}]}}`

	var gotPath string
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	from := time.Unix(1_698_000_000, 0).UTC()
	to := time.Unix(1_700_000_000, 0).UTC()

	floor, err := api.RetentionFloor(context.Background(), from, to)
	if err != nil {
		t.Fatalf("RetentionFloor: %v", err)
	}
	want := time.Unix(1_699_000_000, 0).UTC()
	if !floor.Equal(want) {
		t.Errorf("floor = %v, want %v", floor, want)
	}
	// The bug this replaces used an instant query, which always reports
	// roughly now regardless of how much history exists.
	if !strings.HasSuffix(gotPath, "query_range") {
		t.Errorf("RetentionFloor hit %q; it must use query_range, never an "+
			"instant query, or it reports now and collapses every window", gotPath)
	}
}

func TestRetentionFloorReportsNoData(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	from := time.Unix(1_698_000_000, 0).UTC()
	to := time.Unix(1_700_000_000, 0).UTC()

	if _, err := api.RetentionFloor(context.Background(), from, to); !errors.Is(err, ErrEmptyResult) {
		t.Errorf("err = %v, want ErrEmptyResult when no ALERTS data exists", err)
	}
}

func TestRetentionFloorRejectsEmptyWindow(t *testing.T) {
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("RetentionFloor queried Prometheus for an empty window")
	})
	now := time.Unix(1_700_000_000, 0).UTC()
	if _, err := api.RetentionFloor(context.Background(), now, now); err == nil {
		t.Error("RetentionFloor accepted an empty window, want error")
	}
}

func TestQueryRangeSurfacesHTTPErrors(t *testing.T) {
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := api.QueryRange(context.Background(), "ALERTS",
		time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if err == nil {
		t.Fatal("QueryRange succeeded against a 500, want error")
	}
}
