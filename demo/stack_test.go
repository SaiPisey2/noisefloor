//go:build integration

package demo

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

const promURL = "http://localhost:9090"

func TestDemoRulesAreLoaded(t *testing.T) {
	resp, err := http.Get(promURL + "/api/v1/rules")
	if err != nil {
		t.Fatalf("prometheus unreachable, run `make demo-up`: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Data struct {
			Groups []struct {
				Name  string `json:"name"`
				Rules []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"rules"`
			} `json:"groups"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode rules: %v", err)
	}

	found := map[string]bool{}
	for _, g := range body.Data.Groups {
		for _, r := range g.Rules {
			found[r.Name] = true
		}
	}
	for _, want := range []string{
		"DemoSpiky", "DemoFlapping", "DemoSustained",
		"DemoCauseA", "DemoCauseB", "DemoCauseC", "DemoCauseD", "DemoQuiet",
	} {
		if !found[want] {
			t.Errorf("rule %s not loaded by prometheus", want)
		}
	}
}

func TestAlertsSeriesAppears(t *testing.T) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := http.Get(promURL + `/api/v1/query?query=ALERTS`)
		if err == nil {
			var body struct {
				Data struct {
					Result []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if len(body.Data.Result) > 0 {
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatal("no ALERTS series after 3 minutes; faultgen or rules are wrong")
}
