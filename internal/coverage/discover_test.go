package coverage

import (
	"testing"

	"github.com/prometheus/common/model"
)

func upSample(labels model.Metric, value float64) *model.Sample {
	return &model.Sample{Metric: labels, Value: model.SampleValue(value)}
}

func TestDiscoverServices(t *testing.T) {
	up := model.Vector{
		// k8s SD labels present -> discovered as namespace/service.
		upSample(model.Metric{"job": "checkout", "namespace": "shop", "service": "checkout", "instance": "10.0.0.1:8080"}, 1),
		// A second instance of the same k8s service must not become a
		// second row.
		upSample(model.Metric{"job": "checkout", "namespace": "shop", "service": "checkout", "instance": "10.0.0.2:8080"}, 1),
		// job only, no SD labels -> discovered as the bare job.
		upSample(model.Metric{"job": "search", "instance": "10.0.0.3:8080"}, 1),
		// down.
		upSample(model.Metric{"job": "batchworker", "instance": "10.0.0.4:8080"}, 0),
	}

	traffic := map[string]float64{
		KeyFor("shop", "checkout", "checkout"): 500,
		KeyFor("", "", "search"):               9000,
		KeyFor("", "", "batchworker"):          2,
	}

	got := DiscoverServices(up, []string{"legacy-cron"}, traffic)

	byName := map[string]Service{}
	for _, s := range got {
		byName[s.Name] = s
	}

	if len(got) != 4 {
		t.Fatalf("got %d services, want 4: %+v", len(got), got)
	}

	checkout, ok := byName["shop/checkout"]
	if !ok {
		t.Fatal("shop/checkout not discovered")
	}
	if checkout.Source != "k8s" || !checkout.Up || checkout.Traffic != 500 {
		t.Errorf("checkout = %+v, want source=k8s up=true traffic=500", checkout)
	}

	search, ok := byName["search"]
	if !ok {
		t.Fatal("search not discovered")
	}
	if search.Source != "job" || !search.Up || search.Traffic != 9000 {
		t.Errorf("search = %+v, want source=job up=true traffic=9000", search)
	}

	batch, ok := byName["batchworker"]
	if !ok {
		t.Fatal("batchworker not discovered")
	}
	if batch.Up {
		t.Errorf("batchworker.Up = true, want false (its only sample read 0)")
	}

	cron, ok := byName["legacy-cron"]
	if !ok {
		t.Fatal("legacy-cron (explicit config entry) not discovered")
	}
	if cron.Source != "config" {
		t.Errorf("legacy-cron.Source = %q, want config", cron.Source)
	}
}

func TestDiscoverServicesExplicitDoesNotDuplicate(t *testing.T) {
	up := model.Vector{upSample(model.Metric{"job": "search"}, 1)}
	got := DiscoverServices(up, []string{"search"}, nil)
	if len(got) != 1 {
		t.Fatalf("got %d services, want 1 (explicit name already discovered via job): %+v", len(got), got)
	}
}

func TestSumTraffic(t *testing.T) {
	m := model.Matrix{
		&model.SampleStream{
			Metric: model.Metric{"job": "search", "instance": "a"},
			Values: []model.SamplePair{{Value: 100}, {Value: 200}, {Value: 300}},
		},
		&model.SampleStream{
			Metric: model.Metric{"namespace": "shop", "service": "checkout", "job": "checkout", "instance": "a"},
			Values: []model.SamplePair{{Value: 10}, {Value: 20}},
		},
		&model.SampleStream{
			Metric: model.Metric{"namespace": "shop", "service": "checkout", "job": "checkout", "instance": "b"},
			Values: []model.SamplePair{{Value: 30}, {Value: 50}},
		},
	}

	got := SumTraffic(m)

	if want := 200.0; got[KeyFor("", "", "search")] != want {
		t.Errorf("search traffic = %v, want %v", got[KeyFor("", "", "search")], want)
	}
	// Two instances of the same k8s service: their per-series averages sum.
	if want := 15.0 + 40.0; got[KeyFor("shop", "checkout", "checkout")] != want {
		t.Errorf("checkout traffic = %v, want %v", got[KeyFor("shop", "checkout", "checkout")], want)
	}
}
