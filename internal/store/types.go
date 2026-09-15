package store

import "time"

const (
	SourceBackfill = "backfill"
	// SourceWebhook is reserved for issue #13 (webhook-based episode
	// ingestion, as an alternative to range-querying ALERTS). Unused today.
	SourceWebhook = "webhook"

	StateFiring  = "firing"
	StatePending = "pending"
)

type Rule struct {
	ID        int64
	AlertName string
	GroupName string
	File      string
	// Line is where this rule starts in File, 1-indexed. It is populated
	// from internal/remediate.LocateRules, which walks a git checkout at
	// config.Rules.Path and parses each rule file's YAML positions --
	// see that package for how. Live as of issue #9; zero when rules.path
	// is unset, when the rule could not be found under it (see
	// remediate.MissingInFiles), or for any rule synced before that lookup
	// ran.
	Line        int
	Expr        string
	ExprHash    string
	For         time.Duration
	Labels      map[string]string
	Annotations map[string]string
	FirstSeen   time.Time
	LastSeen    time.Time
	// ExprChangedAt is when this rule's expression was last OBSERVED to change.
	// Zero means never observed changing, which includes every rule on its
	// first sync -- meeting a rule is not the same as watching it change.
	//
	// Read-only from the caller's side: UpsertRule ignores whatever is passed
	// here and maintains the column itself, writing zero on insert and the
	// observation time only when the expression hash actually differs.
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

// WebhookMeta carries the fields an Alertmanager webhook notification
// provides that the ALERTS series backfill cannot see at all: which
// receiver routed the alert, the grouping Alertmanager applied,
// annotations as rendered at fire time (template expansion happens in
// Alertmanager, not here), and the generatorURL. See issue #13.
//
// It is 1:1 with one Episode (EpisodeID is both the foreign key and, in
// storage, the primary key) and lives in its own table rather than as
// columns on Episode: adding it this way required no change to the
// episodes table, its UNIQUE constraint, or the backfill write path that
// depends on both, and needs no migration beyond the same
// CREATE TABLE IF NOT EXISTS every other table already uses.
type WebhookMeta struct {
	EpisodeID   int64
	Receiver    string
	GroupKey    string
	GroupLabels map[string]string
	// Annotations are the per-alert annotations as Alertmanager rendered
	// them for this firing -- distinct from the rule's own annotation
	// templates, which internal/collect/rules already captures.
	Annotations  map[string]string
	GeneratorURL string
	ExternalURL  string

	// PreciseStartedAt / PreciseEndedAt are the webhook's own exact
	// boundaries for this firing, always recorded even when the episode
	// row itself keeps a coarser boundary a backfilled episode already
	// had -- see SQLite.UpsertWebhookEpisode's reconciliation rule. Nothing
	// observed is ever thrown away, even when it does not move the
	// episode's stored started_at/ended_at.
	PreciseStartedAt time.Time
	PreciseEndedAt   time.Time

	UpdatedAt time.Time
}

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
