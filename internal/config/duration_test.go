package config

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0.5d", 12 * time.Hour},
		{"1h", time.Hour},
		{"90m", 90 * time.Minute},
		{"45s", 45 * time.Second},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if err != nil {
			t.Fatalf("ParseDuration(%q) returned error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDurationRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "abc", "d", "30x", "--1d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded, want error", in)
		}
	}
}

func TestDurationUnmarshalYAML(t *testing.T) {
	var got struct {
		Window Duration `yaml:"window"`
	}
	if err := unmarshalYAMLForTest([]byte("window: 14d\n"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Window.Std() != 14*24*time.Hour {
		t.Errorf("window = %v, want 336h", got.Window.Std())
	}
}
