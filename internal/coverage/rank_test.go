package coverage

import "testing"

// TestRankBlindSpots is the blind-spot ranking table called for by issue
// #10: a service with substantial traffic and zero alerts must outrank an
// idle one, and the idle one must be flagged, not merely sorted last.
func TestRankBlindSpots(t *testing.T) {
	grid := []ServiceCoverage{
		{Service: Service{Name: "shop/checkout", Traffic: 500}, Covered: map[Signal][]RuleMatch{
			SignalRate: {{AlertName: "x", Certain: true}},
		}}, // fully covered on at least one signal -> not a blind spot
		{Service: Service{Name: "search", Traffic: 9000}, Covered: map[Signal][]RuleMatch{}},       // headline blind spot
		{Service: Service{Name: "batchworker", Traffic: 2}, Covered: map[Signal][]RuleMatch{}},     // idle, no alerts
		{Service: Service{Name: "shop/billing", Traffic: 1000}, Covered: map[Signal][]RuleMatch{}}, // some traffic, no alerts
	}

	got := RankBlindSpots(grid)

	var names []string
	for _, b := range got {
		names = append(names, b.Service.Name)
	}

	if len(got) != 3 {
		t.Fatalf("got %d blind spots, want 3 (checkout is covered): %v", len(got), names)
	}
	if got[0].Service.Name != "search" {
		t.Errorf("top blind spot = %q, want search (highest traffic among the uncovered)", got[0].Service.Name)
	}
	if got[len(got)-1].Service.Name != "batchworker" {
		t.Errorf("last blind spot = %q, want batchworker (idle)", got[len(got)-1].Service.Name)
	}
	if !got[len(got)-1].Idle {
		t.Error("batchworker: expected Idle == true")
	}
	for _, b := range got {
		if b.Service.Name != "batchworker" && b.Idle {
			t.Errorf("%s: unexpectedly flagged Idle", b.Service.Name)
		}
	}
}

func TestRankBlindSpotsAllIdleWhenNoTraffic(t *testing.T) {
	grid := []ServiceCoverage{
		{Service: Service{Name: "a", Traffic: 0}},
		{Service: Service{Name: "b", Traffic: 0}},
	}
	got := RankBlindSpots(grid)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	for _, b := range got {
		if !b.Idle {
			t.Errorf("%s: expected Idle == true when every service has zero traffic", b.Service.Name)
		}
	}
}
