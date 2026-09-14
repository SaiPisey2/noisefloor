package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so YAML can carry a "d" (day) suffix, which
// time.ParseDuration does not understand.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// ParseDuration extends time.ParseDuration with a day unit, so "30d" works.
func ParseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if rest, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: %w", s, err)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// unmarshalYAMLForTest exists so the duration tests do not need to import
// yaml directly.
func unmarshalYAMLForTest(data []byte, out any) error {
	return yaml.Unmarshal(data, out)
}
