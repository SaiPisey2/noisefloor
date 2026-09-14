package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "noisefloor.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeTemp(t, "prometheus:\n  url: http://localhost:9090\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Window.Std() != 30*24*time.Hour {
		t.Errorf("window = %v, want 720h", cfg.Window.Std())
	}
	if cfg.Prometheus.Step.Std() != time.Minute {
		t.Errorf("step = %v, want 1m", cfg.Prometheus.Step.Std())
	}
	if cfg.Database != "noisefloor.db" {
		t.Errorf("database = %q, want noisefloor.db", cfg.Database)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	path := writeTemp(t, "prometheus:\n  url: http://prom:9090\n  step: 30s\nwindow: 7d\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Prometheus.Step.Std() != 30*time.Second {
		t.Errorf("step = %v, want 30s", cfg.Prometheus.Step.Std())
	}
	if cfg.Window.Std() != 7*24*time.Hour {
		t.Errorf("window = %v, want 168h", cfg.Window.Std())
	}
}

func TestLoadRequiresPrometheusURL(t *testing.T) {
	path := writeTemp(t, "window: 7d\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded without a prometheus url, want error")
	}
}

func TestLoadRejectsWeightsNotSummingToOne(t *testing.T) {
	body := `
prometheus:
  url: http://localhost:9090
weights:
  short_lived_rate: 0.5
  silenced_rate: 0.1
  flap_rate: 0.1
  cofire_ratio: 0.1
  offhours_rate: 0.1
`
	path := writeTemp(t, body)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with weights summing to 0.9, want error")
	}
}

func TestLoadRejectsChunkSmallerThanStep(t *testing.T) {
	body := "prometheus:\n  url: http://localhost:9090\n  step: 5m\n  chunk: 1m\n"
	path := writeTemp(t, body)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with chunk < step, want error")
	}
}

func TestDefaultWeightsSumToOne(t *testing.T) {
	if sum := Default().Weights.Sum(); sum < 0.999 || sum > 1.001 {
		t.Errorf("default weights sum = %v, want 1.0", sum)
	}
}

func TestLoadDefaultsTimeoutToTenMinutes(t *testing.T) {
	path := writeTemp(t, "prometheus:\n  url: http://localhost:9090\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Timeout.Std() != 10*time.Minute {
		t.Errorf("timeout = %v, want 10m", cfg.Timeout.Std())
	}
}

func TestLoadRejectsNonPositiveTimeout(t *testing.T) {
	body := "prometheus:\n  url: http://localhost:9090\ntimeout: 0s\n"
	path := writeTemp(t, body)
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with timeout: 0s, want error")
	}
}
