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
		// excluded by default in the demo/live config -- must not appear.
		upSample(model.Metric{"job": "prometheus", "instance": "localhost:9090"}, 1),
	}

	proxies := map[string]TrafficProxy{
		KeyFor("shop", "checkout", "checkout"): {Value: 0.6, Basis: BasisRequests},
		KeyFor("", "", "search"):               {Value: 2.9, Basis: BasisRequests},
		KeyFor("", "", "batchworker"):          {Value: 1, Basis: BasisSamples},
	}

	got := DiscoverServices(up, []string{"legacy-cron"}, proxies, []string{"prometheus"})

	byName := map[string]Service{}
	for _, s := range got.Services {
		byName[s.Name] = s
	}

	if len(got.Services) != 4 {
		t.Fatalf("got %d services, want 4: %+v", len(got.Services), got.Services)
	}
	if _, ok := byName["prometheus"]; ok {
		t.Error("prometheus: expected to be excluded")
	}
	if len(got.ExcludedJobs) != 1 || got.ExcludedJobs[0] != "prometheus" {
		t.Errorf("ExcludedJobs = %v, want [prometheus]", got.ExcludedJobs)
	}

	checkout, ok := byName["shop/checkout"]
	if !ok {
		t.Fatal("shop/checkout not discovered")
	}
	if checkout.Source != "k8s" || !checkout.Up || checkout.Traffic != 0.6 || checkout.TrafficBasis != BasisRequests {
		t.Errorf("checkout = %+v, want source=k8s up=true traffic=0.6 basis=%s", checkout, BasisRequests)
	}

	search, ok := byName["search"]
	if !ok {
		t.Fatal("search not discovered")
	}
	if search.Source != "job" || !search.Up || search.Traffic != 2.9 || search.TrafficBasis != BasisRequests {
		t.Errorf("search = %+v, want source=job up=true traffic=2.9 basis=%s", search, BasisRequests)
	}

	batch, ok := byName["batchworker"]
	if !ok {
		t.Fatal("batchworker not discovered")
	}
	if batch.Up {
		t.Errorf("batchworker.Up = true, want false (its only sample read 0)")
	}
	if batch.TrafficBasis != BasisSamples {
		t.Errorf("batchworker TrafficBasis = %q, want %q", batch.TrafficBasis, BasisSamples)
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
	got := DiscoverServices(up, []string{"search"}, nil, nil)
	if len(got.Services) != 1 {
		t.Fatalf("got %d services, want 1 (explicit name already discovered via job): %+v", len(got.Services), got.Services)
	}
}

func TestDiscoverServicesNoExclusionsMatchedIsEmpty(t *testing.T) {
	up := model.Vector{upSample(model.Metric{"job": "search"}, 1)}
	got := DiscoverServices(up, nil, nil, []string{"prometheus"})
	if len(got.ExcludedJobs) != 0 {
		t.Errorf("ExcludedJobs = %v, want empty (nothing matched \"prometheus\")", got.ExcludedJobs)
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

func TestBuildTrafficProxiesPrefersRequestsOverSamples(t *testing.T) {
	sampleVolume := model.Matrix{
		&model.SampleStream{
			Metric: model.Metric{"job": "checkout"},
			Values: []model.SamplePair{{Value: 20}},
		},
		&model.SampleStream{
			Metric: model.Metric{"job": "batchworker"},
			Values: []model.SamplePair{{Value: 1}},
		},
	}
	reqRate := model.Matrix{
		&model.SampleStream{
			Metric: model.Metric{"job": "checkout"},
			Values: []model.SamplePair{{Value: 8}, {Value: 12}}, // avg 10
		},
	}

	got := BuildTrafficProxies(reqRate, sampleVolume)

	checkout := got[KeyFor("", "", "checkout")]
	if checkout.Basis != BasisRequests || checkout.Value != 10 {
		t.Errorf("checkout proxy = %+v, want {10 %s}", checkout, BasisRequests)
	}
	batch := got[KeyFor("", "", "batchworker")]
	if batch.Basis != BasisSamples || batch.Value != 1 {
		t.Errorf("batchworker proxy = %+v, want {1 %s} (no request counter, falls back)", batch, BasisSamples)
	}
}
