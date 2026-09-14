package config

import (
	"fmt"
	"math"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Prometheus   Prometheus   `yaml:"prometheus"`
	Alertmanager Alertmanager `yaml:"alertmanager"`
	Rules        Rules        `yaml:"rules"`
	Database     string       `yaml:"database"`
	Window       Duration     `yaml:"window"`
	Timeout      Duration     `yaml:"timeout"`
	Timezone     string       `yaml:"timezone"`
	Weights      Weights      `yaml:"weights"`
	// FlapWindow is how soon a re-fire on the same series counts as flapping,
	// which feeds flap_rate. It sits alongside the weights because it shapes
	// the same signal they score: an operator tuning flap_rate's weight needs
	// to see, and be able to change, what "flapping" itself means. Defaults to
	// 1h, unchanged from before this was configurable.
	FlapWindow Duration   `yaml:"flap_window"`
	Confidence Confidence `yaml:"confidence"`
}

type Prometheus struct {
	URL   string   `yaml:"url"`
	Step  Duration `yaml:"step"`
	Chunk Duration `yaml:"chunk"`
	Auth  Auth     `yaml:"auth"`
}

type Alertmanager struct {
	URL  string `yaml:"url"`
	Auth Auth   `yaml:"auth"`
}

type Rules struct {
	// Path is reserved for issue #8 (PR-per-rule bot), which needs to locate
	// the rule file a proposed change targets in order to open a PR against
	// it. Unused today.
	Path string `yaml:"path"`

	// MaxDeactivatedFraction guards against a broken rule file being mistaken
	// for a deliberate cleanup. Prometheus reporting fewer alerting rules than
	// the store remembers as active is ambiguous on its own: it is what BOTH
	// "someone deleted some rules" and "a rule file stopped parsing this
	// morning" look like from here. Above this fraction of previously active
	// rules disappearing in a single run, SyncRules refuses to deactivate them
	// rather than silently emptying the report.
	//
	// 0.2 is chosen to catch the reported failure mode directly: one rule file
	// out of a handful failing to load is a sudden, meaningfully large slice
	// of the active set, while an operator pruning one or two noisy rules out
	// of a normal-sized rule set stays comfortably under it. A team that does
	// large, deliberate rule-set prunings in one sitting should raise this.
	MaxDeactivatedFraction float64 `yaml:"max_deactivated_fraction"`
}

// Weights are the scored signals only. Verdict-only and confidence-only
// signals deliberately carry no weight; see docs/noisefloor-design.md.
type Weights struct {
	ShortLivedRate float64 `yaml:"short_lived_rate"`
	SilencedRate   float64 `yaml:"silenced_rate"`
	FlapRate       float64 `yaml:"flap_rate"`
	CofireRatio    float64 `yaml:"cofire_ratio"`
	OffhoursRate   float64 `yaml:"offhours_rate"`
}

func (w Weights) Sum() float64 {
	return w.ShortLivedRate + w.SilencedRate + w.FlapRate + w.CofireRatio + w.OffhoursRate
}

type Confidence struct {
	MinEpisodes int      `yaml:"min_episodes"`
	MinWindow   Duration `yaml:"min_window"`
}

func Default() Config {
	return Config{
		Prometheus: Prometheus{
			Step:  Duration(time.Minute),
			Chunk: Duration(6 * time.Hour),
		},
		Database: "noisefloor.db",
		Window:   Duration(30 * 24 * time.Hour),
		Timeout:  Duration(10 * time.Minute),
		// UTC, not Local. Timezone decides which fires count as off-hours, and
		// off-hours is a scored signal: with "Local" the same database scored on
		// a CET laptop and in a UTC CI container produces different noise scores
		// and can produce different verdicts. A scan must be reproducible.
		// Teams whose working day is not UTC set this explicitly, which is a
		// deliberate, recorded choice rather than an accident of where it ran.
		Timezone: "UTC",
		Weights: Weights{
			ShortLivedRate: 0.30,
			SilencedRate:   0.25,
			FlapRate:       0.20,
			CofireRatio:    0.15,
			OffhoursRate:   0.10,
		},
		FlapWindow: Duration(time.Hour),
		Confidence: Confidence{
			MinEpisodes: 10,
			MinWindow:   Duration(14 * 24 * time.Hour),
		},
		Rules: Rules{
			MaxDeactivatedFraction: 0.2,
		},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Prometheus.URL == "" {
		return fmt.Errorf("prometheus.url is required")
	}
	if c.Prometheus.Step.Std() <= 0 {
		return fmt.Errorf("prometheus.step must be positive")
	}
	if c.Prometheus.Chunk.Std() < c.Prometheus.Step.Std() {
		return fmt.Errorf("prometheus.chunk (%v) must be >= prometheus.step (%v)",
			c.Prometheus.Chunk, c.Prometheus.Step)
	}
	if c.Window.Std() <= 0 {
		return fmt.Errorf("window must be positive")
	}
	if c.Timeout.Std() <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	if sum := c.Weights.Sum(); math.Abs(sum-1.0) > 0.001 {
		return fmt.Errorf("weights must sum to 1.0, got %.3f", sum)
	}
	if c.FlapWindow.Std() <= 0 {
		return fmt.Errorf("flap_window must be positive")
	}
	if c.Confidence.MinEpisodes < 1 {
		return fmt.Errorf("confidence.min_episodes must be >= 1")
	}
	if c.Rules.MaxDeactivatedFraction <= 0 || c.Rules.MaxDeactivatedFraction > 1 {
		return fmt.Errorf("rules.max_deactivated_fraction must be in (0, 1], got %v",
			c.Rules.MaxDeactivatedFraction)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	if err := c.Prometheus.Auth.Validate(); err != nil {
		return fmt.Errorf("prometheus.auth: %w", err)
	}
	if err := c.Alertmanager.Auth.Validate(); err != nil {
		return fmt.Errorf("alertmanager.auth: %w", err)
	}
	return nil
}

// Location resolves the configured timezone. Validate guarantees it parses.
func (c Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}
