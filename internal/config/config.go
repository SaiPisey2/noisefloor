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
	Coverage   Coverage   `yaml:"coverage"`
}

// Coverage configures `noisefloor coverage` only; scan and remediate never
// read it.
type Coverage struct {
	// Services is an explicit list of service names to include in coverage
	// discovery even where `up` and its Kubernetes SD labels don't name
	// them -- a target scraped by a different Prometheus, or one whose job
	// name doesn't match what the team calls it. Optional: discovery from
	// `up` plus namespace/service SD labels covers the common case with no
	// configuration at all.
	Services []string `yaml:"services"`

	// ExcludeJobs names job values to drop from coverage discovery
	// entirely -- not shown in the grid or the blind-spot list at all.
	// Defaults to ["prometheus"]: Prometheus's own self-scrape job is
	// present in essentially every install, and would otherwise be the
	// top line of nearly every coverage report purely because it exposes a
	// lot of internal metrics -- crowding out the application-level gaps
	// this command exists to surface, in the position that should carry
	// the most signal. "Nobody alerts on our own Prometheus" is still a
	// legitimate finding some teams want; set this to [] to see it.
	ExcludeJobs []string `yaml:"exclude_jobs"`

	// RuleTargets nominates, per service (keyed by coverage.Service.Name,
	// e.g. "search" or "shop/checkout"), the existing rule group a starter
	// rule proposed by `noisefloor propose` should be appended to.
	//
	// A blind spot has, by construction, no alert on it at all -- so unlike
	// remediate (which edits a rule Prometheus already evaluates, giving it
	// an unambiguous home), propose has no existing rule of its own to
	// locate. It falls back to a rule group that already covers this exact
	// service via some OTHER signal (found automatically -- see
	// internal/pr's target-resolution doc comment), and only asks the
	// operator when that fails, which it always will for a service that has
	// never had any alerting at all.
	//
	// Deliberately narrow: this names an EXISTING group in an EXISTING
	// file, never a new one. Fabricating a rule file's path or a brand new
	// group name is a guess about a team's own layout conventions
	// (directory structure, how rule_files: globs are wired, naming
	// scheme) that noisefloor has no way to verify, and a wrong guess is
	// exactly the "PR that does not apply cleanly" the propose feature was
	// warned against. Naming an existing group here costs the operator one
	// line of config and removes the guess entirely -- remediate.LocateRules
	// finds it by (file, group name) exactly the way it finds a rule to
	// edit.
	RuleTargets map[string]RuleTarget `yaml:"rule_targets"`
}

// RuleTarget is one nomination in Coverage.RuleTargets: an existing rule
// group, identified the same way remediate.RuleLocation is (by file path
// and group name), to append a starter rule to.
type RuleTarget struct {
	File  string `yaml:"file"`
	Group string `yaml:"group"`
}

type Prometheus struct {
	URL   string   `yaml:"url"`
	Step  Duration `yaml:"step"`
	Chunk Duration `yaml:"chunk"`
	Auth  Auth     `yaml:"auth"`

	// RetryAttempts is the maximum number of tries per chunk query,
	// including the first: 1 means no retry. A transient error on chunk 87
	// of 120 used to abort the whole scan and discard the other 119 chunks'
	// worth of work; retrying here means one bad response no longer costs
	// the rest of the scan.
	RetryAttempts int `yaml:"retry_attempts"`
	// RetryBaseDelay is the backoff base: attempt N (1-indexed) after a
	// retryable failure waits RetryBaseDelay * 2^(N-1), so the default 1s
	// backs off 1s, 2s, 4s, ... Only retryable failures wait at all -- see
	// prom.IsRetryable -- and a canceled context is never waited out.
	RetryBaseDelay Duration `yaml:"retry_base_delay"`
}

type Alertmanager struct {
	URL  string `yaml:"url"`
	Auth Auth   `yaml:"auth"`
}

type Rules struct {
	// Path is a git checkout of Prometheus rule files, walked by
	// internal/remediate.LocateRules to find the file and line a rule
	// lives at -- what issue #8 (PR-per-rule bot) needs to open a change
	// against the right place. Live as of issue #9. Left empty, no
	// location lookup runs and store.Rule.Line stays 0 for every rule.
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
			Step:           Duration(time.Minute),
			Chunk:          Duration(6 * time.Hour),
			RetryAttempts:  3,
			RetryBaseDelay: Duration(time.Second),
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
		Coverage: Coverage{
			ExcludeJobs: []string{"prometheus"},
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
	if c.Prometheus.RetryAttempts < 1 {
		return fmt.Errorf("prometheus.retry_attempts must be >= 1, got %d", c.Prometheus.RetryAttempts)
	}
	if c.Prometheus.RetryBaseDelay.Std() <= 0 {
		return fmt.Errorf("prometheus.retry_base_delay must be positive")
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
	for svc, target := range c.Coverage.RuleTargets {
		if target.File == "" || target.Group == "" {
			return fmt.Errorf("coverage.rule_targets[%q]: both file and group are required", svc)
		}
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
