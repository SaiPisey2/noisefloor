package store

import "time"

const (
	SourceBackfill = "backfill"
	SourceWebhook  = "webhook"

	StateFiring  = "firing"
	StatePending = "pending"
)

type Rule struct {
	ID          int64
	AlertName   string
	GroupName   string
	File        string
	Line        int
	Expr        string
	ExprHash    string
	For         time.Duration
	Labels      map[string]string
	Annotations map[string]string
	FirstSeen   time.Time
	LastSeen    time.Time
	// ExprChangedAt is when this rule's expression last changed. A retuned
	// rule must not inherit the verdict earned by its old expression.
	ExprChangedAt time.Time
	Active        bool
}

type Episode struct {
	ID          int64
	RuleID      int64
	Fingerprint string
	Labels      map[string]string
	StartedAt   time.Time
	EndedAt     time.Time
	Resolution  time.Duration
	Source      string
	State       string
}

func (e Episode) Duration() time.Duration { return e.EndedAt.Sub(e.StartedAt) }

type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type Silence struct {
	ID        int64
	AMID      string
	Matchers  []Matcher
	CreatedBy string
	Comment   string
	StartsAt  time.Time
	EndsAt    time.Time
}

type Score struct {
	RuleID      int64
	WindowStart time.Time
	WindowEnd   time.Time
	Signals     map[string]float64
	NoiseScore  float64
	Verdict     string
	Confidence  float64
	ComputedAt  time.Time
}
