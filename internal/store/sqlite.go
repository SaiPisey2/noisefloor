package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type SQLite struct{ db *sql.DB }

// maxOpenConns bounds how many concurrent connections one SQLite handle
// hands out. This is a read-only reporting server behind no queue of its
// own: without a cap, a burst of concurrent requests (twenty browser tabs
// hitting a flapping rule's detail page, say) opens a connection each,
// and modernc.org/sqlite serializes writers underneath -- readers pile up
// waiting on the OS file descriptor table long before they'd ever wait on
// SQLite itself. A small, fixed pool makes that queueing happen inside
// database/sql (bounded, fair) instead of as unbounded goroutines and file
// descriptors pinned on the far side of a slow query.
const maxOpenConns = 10

func Open(path string) (*SQLite, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().Unix()
}
func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// toJSON marshals v for a NOT NULL TEXT column. Every caller passes a plain
// map or slice of JSON-safe primitives -- rule labels/annotations, episode
// labels, silence matchers, or the scored signals map -- none of which
// contain cycles, channels, or anything else json.Marshal can fail on. The
// error is therefore tolerated rather than propagated, but "{}" is returned
// rather than "" so the column still holds valid JSON if that assumption is
// ever wrong: an empty string is not valid JSON, and every reader of this
// column assumes it is.
func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func fromJSONMap(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func (s *SQLite) UpsertRule(ctx context.Context, r *Rule) (int64, error) {
	const q = `
INSERT INTO rules (alert_name, group_name, file, line, expr, expr_hash, for_seconds,
                   labels, annotations, first_seen, last_seen, expr_changed_at, active)
VALUES (?,?,?,?,?,?,?,?,?,?,?,0,?)
ON CONFLICT (group_name, alert_name) DO UPDATE SET
  file        = excluded.file,
  line        = excluded.line,
  expr        = excluded.expr,
  expr_hash   = excluded.expr_hash,
  for_seconds = excluded.for_seconds,
  labels      = excluded.labels,
  annotations = excluded.annotations,
  last_seen   = excluded.last_seen,
  -- Only advance expr_changed_at when the expression actually changed, so a
  -- routine rescan does not look like a retune.
  --
  -- The INSERT above writes 0, never a timestamp: on first sight we have not
  -- OBSERVED this rule change, we have merely met it. Writing a timestamp
  -- there would mark every rule in a fresh database as retuned, and the caller
  -- withholds verdicts from retuned rules, so the very first scan would
  -- report nothing but keep at zero confidence for every rule.
  expr_changed_at = CASE
    WHEN rules.expr_hash != excluded.expr_hash THEN excluded.last_seen
    ELSE rules.expr_changed_at
  END,
  active      = excluded.active
RETURNING id`
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		r.AlertName, r.GroupName, r.File, r.Line, r.Expr, r.ExprHash,
		int64(r.For.Seconds()), toJSON(r.Labels), toJSON(r.Annotations),
		unix(r.FirstSeen), unix(r.LastSeen), r.Active,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert rule %s/%s: %w", r.GroupName, r.AlertName, err)
	}
	r.ID = id
	return id, nil
}

func (s *SQLite) ListRules(ctx context.Context) ([]Rule, error) {
	const q = `
SELECT id, alert_name, group_name, file, line, expr, expr_hash, for_seconds,
       labels, annotations, first_seen, last_seen, expr_changed_at, active
FROM rules ORDER BY group_name, alert_name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var out []Rule
	for rows.Next() {
		var r Rule
		var forSec, first, last, changed int64
		var labels, annotations string
		if err := rows.Scan(&r.ID, &r.AlertName, &r.GroupName, &r.File, &r.Line,
			&r.Expr, &r.ExprHash, &forSec, &labels, &annotations,
			&first, &last, &changed, &r.Active); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.For = time.Duration(forSec) * time.Second
		r.Labels = fromJSONMap(labels)
		r.Annotations = fromJSONMap(annotations)
		r.FirstSeen = fromUnix(first)
		r.LastSeen = fromUnix(last)
		r.ExprChangedAt = fromUnix(changed)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleIDByAlertName finds a rule by alert name alone. The backfiller needs it
// because the ALERTS series carries an alertname but no group, and inventing a
// group would create a second row for a rule that already exists.
//
// Active rules win over inactive ones, so history from a deleted rule never
// shadows the rule Prometheus currently evaluates.
func (s *SQLite) RuleIDByAlertName(ctx context.Context, alertName string) (int64, bool, error) {
	const q = `SELECT id FROM rules WHERE alert_name = ?
	           ORDER BY active DESC, id ASC LIMIT 1`
	var id int64
	err := s.db.QueryRowContext(ctx, q, alertName).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("lookup rule %q: %w", alertName, err)
	}
	return id, true, nil
}

func (s *SQLite) MarkRulesInactive(ctx context.Context, keep []int64) error {
	if len(keep) == 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE rules SET active = 0`)
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, len(keep))
	for i, id := range keep {
		args[i] = id
	}
	q := fmt.Sprintf(`UPDATE rules SET active = (id IN (%s))`, placeholders)
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("mark rules inactive: %w", err)
	}
	return nil
}

// InsertEpisodes writes episodes, upserting on
// (rule_id, fingerprint, started_at, state) and DISCARDING any episode that
// overlaps one already stored for the same series.
//
// The overlap rule is the store's own defence against re-storing the same
// firing at a shifted timestamp. One series is either firing or it is not, so
// two overlapping episodes with the same rule, fingerprint and state cannot
// both be real: an overlap is always the same firing observed twice, and the
// uniqueness constraint alone cannot see that, because a start that moved by
// a few seconds is a different key. The collectors keep the sample grid stable
// so this should not arise (see collect.Backfiller.Run), but the grid depends
// on prometheus.step, which an operator can change between runs, and on where
// the window starts, which slides forward every run and re-clips any episode
// straddling it. Neither is visible from here, and a duplicate that gets in is
// silently wrong rather than loudly wrong: adjacent duplicates read as
// re-fires and inflate flap_rate until the verdict flips.
//
// What is already stored wins. A re-observation carries no information the
// stored row does not already have, and refusing to rewrite history means a
// rescan can never shorten or move an episode it previously recorded.
func (s *SQLite) InsertEpisodes(ctx context.Context, eps []Episode) error {
	if len(eps) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// The NOT EXISTS is a half-open overlap test: an existing episode of the
	// same series whose span intersects this one, excluding the one that
	// shares this exact start -- that one is this same episode being
	// re-observed, and belongs on the ON CONFLICT path so a still-running
	// firing can have its ended_at extended.
	//
	// INSERT ... SELECT rather than VALUES because a row can be filtered out;
	// SQLite needs the WHERE for ON CONFLICT to parse unambiguously after a
	// SELECT, which this supplies anyway.
	const q = `
INSERT INTO episodes (rule_id, fingerprint, labels, started_at, ended_at,
                      resolution_seconds, source, state)
SELECT ?,?,?,?,?,?,?,?
WHERE NOT EXISTS (
  SELECT 1 FROM episodes e
  WHERE e.rule_id = ? AND e.fingerprint = ? AND e.state = ?
    AND e.started_at <> ?
    AND e.started_at < ? AND e.ended_at > ?
)
ON CONFLICT (rule_id, fingerprint, started_at, state) DO UPDATE SET
  ended_at = excluded.ended_at`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, e := range eps {
		start, end := unix(e.StartedAt), unix(e.EndedAt)
		if _, err := stmt.ExecContext(ctx,
			e.RuleID, e.Fingerprint, toJSON(e.Labels), start, end,
			int64(e.Resolution.Seconds()), e.Source, e.State,
			e.RuleID, e.Fingerprint, e.State, start, end, start,
		); err != nil {
			return fmt.Errorf("insert episode: %w", err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) scanEpisodes(rows *sql.Rows) ([]Episode, error) {
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		var labels string
		var start, end, res int64
		if err := rows.Scan(&e.ID, &e.RuleID, &e.Fingerprint, &labels,
			&start, &end, &res, &e.Source, &e.State); err != nil {
			return nil, fmt.Errorf("scan episode: %w", err)
		}
		e.Labels = fromJSONMap(labels)
		e.StartedAt = fromUnix(start)
		e.EndedAt = fromUnix(end)
		e.Resolution = time.Duration(res) * time.Second
		out = append(out, e)
	}
	return out, rows.Err()
}

const episodeCols = `id, rule_id, fingerprint, labels, started_at, ended_at,
                     resolution_seconds, source, state`

func (s *SQLite) ListEpisodesInWindow(ctx context.Context, from, to time.Time) ([]Episode, error) {
	q := `SELECT ` + episodeCols + ` FROM episodes
	      WHERE started_at >= ? AND started_at < ?
	      ORDER BY started_at`
	rows, err := s.db.QueryContext(ctx, q, unix(from), unix(to))
	if err != nil {
		return nil, fmt.Errorf("list episodes in window: %w", err)
	}
	return s.scanEpisodes(rows)
}

func (s *SQLite) UpsertSilences(ctx context.Context, sils []Silence) error {
	if len(sils) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	const q = `
INSERT INTO silences (am_id, matchers, created_by, comment, starts_at, ends_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT (am_id) DO UPDATE SET
  matchers = excluded.matchers,
  ends_at  = excluded.ends_at`
	for _, sil := range sils {
		if _, err := tx.ExecContext(ctx, q, sil.AMID, toJSON(sil.Matchers),
			sil.CreatedBy, sil.Comment, unix(sil.StartsAt), unix(sil.EndsAt)); err != nil {
			return fmt.Errorf("upsert silence %s: %w", sil.AMID, err)
		}
	}
	return tx.Commit()
}

func (s *SQLite) ListSilences(ctx context.Context, from, to time.Time) ([]Silence, error) {
	const q = `
SELECT id, am_id, matchers, created_by, comment, starts_at, ends_at
FROM silences WHERE ends_at >= ? AND starts_at < ? ORDER BY starts_at`
	rows, err := s.db.QueryContext(ctx, q, unix(from), unix(to))
	if err != nil {
		return nil, fmt.Errorf("list silences: %w", err)
	}
	defer rows.Close()

	var out []Silence
	for rows.Next() {
		var sil Silence
		var matchers string
		var start, end int64
		if err := rows.Scan(&sil.ID, &sil.AMID, &matchers, &sil.CreatedBy,
			&sil.Comment, &start, &end); err != nil {
			return nil, fmt.Errorf("scan silence: %w", err)
		}
		_ = json.Unmarshal([]byte(matchers), &sil.Matchers)
		sil.StartsAt = fromUnix(start)
		sil.EndsAt = fromUnix(end)
		out = append(out, sil)
	}
	return out, rows.Err()
}

func (s *SQLite) UpsertScore(ctx context.Context, sc *Score) error {
	const q = `
INSERT INTO scores (rule_id, window_start, window_end, signals, noise_score,
                    verdict, confidence, computed_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT (rule_id, window_start, window_end) DO UPDATE SET
  signals     = excluded.signals,
  noise_score = excluded.noise_score,
  verdict     = excluded.verdict,
  confidence  = excluded.confidence,
  computed_at = excluded.computed_at`
	_, err := s.db.ExecContext(ctx, q, sc.RuleID, unix(sc.WindowStart), unix(sc.WindowEnd),
		toJSON(sc.Signals), sc.NoiseScore, sc.Verdict, sc.Confidence, unix(sc.ComputedAt))
	if err != nil {
		return fmt.Errorf("upsert score for rule %d: %w", sc.RuleID, err)
	}
	return nil
}

// GetRule fetches one rule by ID. ok is false when no such rule exists,
// which the server's rule-detail handler turns into a 404 rather than a
// zero-value row that would render as an empty page.
func (s *SQLite) GetRule(ctx context.Context, id int64) (Rule, bool, error) {
	const q = `
SELECT id, alert_name, group_name, file, line, expr, expr_hash, for_seconds,
       labels, annotations, first_seen, last_seen, expr_changed_at, active
FROM rules WHERE id = ?`
	var r Rule
	var forSec, first, last, changed int64
	var labels, annotations string
	err := s.db.QueryRowContext(ctx, q, id).Scan(&r.ID, &r.AlertName, &r.GroupName,
		&r.File, &r.Line, &r.Expr, &r.ExprHash, &forSec, &labels, &annotations,
		&first, &last, &changed, &r.Active)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, false, nil
	}
	if err != nil {
		return Rule{}, false, fmt.Errorf("get rule %d: %w", id, err)
	}
	r.For = time.Duration(forSec) * time.Second
	r.Labels = fromJSONMap(labels)
	r.Annotations = fromJSONMap(annotations)
	r.FirstSeen = fromUnix(first)
	r.LastSeen = fromUnix(last)
	r.ExprChangedAt = fromUnix(changed)
	return r, true, nil
}

// MaxEpisodesPerPage is the hard ceiling on how many episode rows any
// single ListEpisodesForRule call returns, regardless of what limit a
// caller (ultimately, a `?limit=` query parameter on the JSON API) asks
// for. A rule flapping every ten minutes for a year is on the order of
// 50,000 episodes; without a ceiling here, a caller who mistypes or
// deliberately passes a huge limit still forces one query to materialize
// all of them.
const MaxEpisodesPerPage = 2000

// DefaultEpisodesPerPage is what ListEpisodesForRule uses when the caller
// asks for no limit at all (limit <= 0).
const DefaultEpisodesPerPage = 200

// CountEpisodesForRule returns exactly how many episodes exist for ruleID,
// independent of any page size -- what a paginated caller (the JSON API,
// the HTML page's "showing the most recent N of M" note) needs to state
// how much history a bounded page actually represents.
func (s *SQLite) CountEpisodesForRule(ctx context.Context, ruleID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM episodes WHERE rule_id = ?`, ruleID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count episodes for rule %d: %w", ruleID, err)
	}
	return n, nil
}

// ListEpisodesForRule returns one page of episodes for ruleID, most recent
// first, bounded by limit (clamped to (0, MaxEpisodesPerPage], defaulting
// to DefaultEpisodesPerPage when limit <= 0) and offset. Unlike the
// unbounded query this replaces, this always allocates O(limit), never
// O(total episodes) -- see MaxEpisodesPerPage's doc comment for why that
// matters. Use CountEpisodesForRule for the total a caller needs to page
// through or to report as "showing N of M".
func (s *SQLite) ListEpisodesForRule(ctx context.Context, ruleID int64, limit, offset int) ([]Episode, error) {
	if limit <= 0 {
		limit = DefaultEpisodesPerPage
	}
	if limit > MaxEpisodesPerPage {
		limit = MaxEpisodesPerPage
	}
	if offset < 0 {
		offset = 0
	}
	q := `SELECT ` + episodeCols + ` FROM episodes WHERE rule_id = ?
	      ORDER BY started_at DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, ruleID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list episodes for rule %d: %w", ruleID, err)
	}
	return s.scanEpisodes(rows)
}

// MaxWindowEpisodes bounds ListFiringEpisodesForRuleInWindow: a flapping
// rule can accumulate tens of thousands of episodes inside a single
// thirty-day scan window, not just over its whole history, so the window
// filter alone does not bound memory on its own. This one is deliberately
// much smaller than MaxWindowDurations (below): every row here comes back
// as a full Episode, labels map and all (collect.CoveringSilences needs
// the labels to match a silence's matchers against), and that
// per-row JSON decode is the expensive part -- a covering-silence check
// beyond a couple thousand episodes is already deep into "this rule needs
// fixing, not a more complete silence list" territory.
const MaxWindowEpisodes = 2000

// ListFiringEpisodesForRuleInWindow returns ruleID's FIRING episodes whose
// start falls in [from, to), oldest first, capped at MaxWindowEpisodes.
// This is the window a stored score was actually computed over -- the
// same set internal/scanner builds RuleEval.Durations from (see
// ListEpisodesInWindow) -- so a consumer matching silences against this
// call's result sees the same evidence a `noisefloor remediate` proposal
// for the same rule would have, not this rule's entire history the way
// ListEpisodesForRule does. For the counterfactual's duration list, use
// the cheaper ListFiringDurationsForRuleInWindow instead: it does not need
// labels, and its cap is much higher because of that.
func (s *SQLite) ListFiringEpisodesForRuleInWindow(ctx context.Context, ruleID int64, from, to time.Time) ([]Episode, error) {
	q := `SELECT ` + episodeCols + ` FROM episodes
	      WHERE rule_id = ? AND state = ? AND started_at >= ? AND started_at < ?
	      ORDER BY started_at LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, ruleID, StateFiring, unix(from), unix(to), MaxWindowEpisodes)
	if err != nil {
		return nil, fmt.Errorf("list firing episodes for rule %d in window: %w", ruleID, err)
	}
	return s.scanEpisodes(rows)
}

// MaxWindowDurations bounds ListFiringDurationsForRuleInWindow. Each row
// here costs two int64 scans and one subtraction -- no labels, no
// fingerprint, no per-row JSON decode -- so this affords a cap two orders
// of magnitude above MaxWindowEpisodes for roughly the same worst-case
// allocation.
const MaxWindowDurations = 50000

// ListFiringDurationsForRuleInWindow returns just the firing DURATIONS
// (ended_at - started_at) for ruleID's episodes whose start falls in
// [from, to), oldest first, capped at MaxWindowDurations. This is the
// cheap path for a consumer -- the rule-detail page's counterfactual --
// that only needs the numbers remediate.Compute wants, not a full
// Episode: on a window with tens of thousands of episodes, this allocates
// a small fraction of what ListFiringEpisodesForRuleInWindow would for
// the same rows, because there is no labels map to deserialize per row.
func (s *SQLite) ListFiringDurationsForRuleInWindow(ctx context.Context, ruleID int64, from, to time.Time) ([]time.Duration, error) {
	q := `SELECT started_at, ended_at FROM episodes
	      WHERE rule_id = ? AND state = ? AND started_at >= ? AND started_at < ?
	      ORDER BY started_at LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, ruleID, StateFiring, unix(from), unix(to), MaxWindowDurations)
	if err != nil {
		return nil, fmt.Errorf("list firing durations for rule %d in window: %w", ruleID, err)
	}
	defer rows.Close()

	var out []time.Duration
	for rows.Next() {
		var start, end int64
		if err := rows.Scan(&start, &end); err != nil {
			return nil, fmt.Errorf("scan firing duration for rule %d: %w", ruleID, err)
		}
		out = append(out, fromUnix(end).Sub(fromUnix(start)))
	}
	return out, rows.Err()
}

// EpisodeBucket is one fixed-width time bucket of a rule's episode
// history: how many firing and how many pending episodes started inside
// it. See EpisodeTimelineSummary.
type EpisodeBucket struct {
	Index   int
	Firing  int
	Pending int
}

// MaxTimelineEpisodes is the episode count above which the rule-detail
// page's timeline switches from drawing one bar per episode to the
// bucketed summary EpisodeTimelineSummary computes. Past this many
// episodes, one-rect-per-episode stops being readable anyway (it is well
// beyond the viewBox's pixel width) before it becomes a memory problem --
// bucketing is a readability fix that happens to also bound allocation.
const MaxTimelineEpisodes = 500

// TimelineBuckets is how many buckets EpisodeTimelineSummary divides a
// rule's history into. Comfortably above the viewBox's rendered width in
// typical browser zoom levels, so the bucketed view still reads as a
// texture rather than a bar chart.
const TimelineBuckets = 300

// EpisodeTimelineSummary reduces one rule's entire episode history to at
// most numBuckets fixed-width time buckets, computed inside SQL with two
// aggregate queries rather than by fetching every row: this allocates
// O(numBuckets), never O(episode count), so a rule with fifty thousand
// episodes costs the same as one with a dozen.
//
// windowStart is the earliest episode's start; windowEnd is the later of
// the last episode's end or now, so a still-firing episode's bucket
// reaches the present -- the same convention newTimeline uses for the
// exact (unbucketed) rendering. bucketWidth is (windowEnd-windowStart)
// divided across numBuckets, rounded up to a whole second. total is the
// exact episode count (a plain COUNT(*), not itself bounded), so a caller
// can state precisely how much history one bucketed picture summarizes.
// total == 0 (with every other return zero-valued) means the rule has no
// episodes at all.
func (s *SQLite) EpisodeTimelineSummary(ctx context.Context, ruleID int64, now time.Time, numBuckets int) (buckets []EpisodeBucket, bucketWidth time.Duration, windowStart, windowEnd time.Time, total int, err error) {
	var minStart, maxEnd sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT MIN(started_at), MAX(ended_at), COUNT(*) FROM episodes WHERE rule_id = ?`,
		ruleID).Scan(&minStart, &maxEnd, &total)
	if err != nil {
		return nil, 0, time.Time{}, time.Time{}, 0, fmt.Errorf("episode timeline bounds for rule %d: %w", ruleID, err)
	}
	if total == 0 {
		return nil, 0, time.Time{}, time.Time{}, 0, nil
	}

	windowStart = fromUnix(minStart.Int64)
	windowEnd = fromUnix(maxEnd.Int64)
	if now.After(windowEnd) {
		windowEnd = now
	}
	span := windowEnd.Sub(windowStart)
	if span <= 0 {
		span = time.Second
	}
	if numBuckets < 1 {
		numBuckets = 1
	}
	bucketWidth = time.Duration(math.Ceil(span.Seconds()/float64(numBuckets))) * time.Second
	if bucketWidth <= 0 {
		bucketWidth = time.Second
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT CAST((started_at - ?) / ? AS INTEGER) AS bucket, state, COUNT(*)
		 FROM episodes WHERE rule_id = ? GROUP BY bucket, state`,
		unix(windowStart), int64(bucketWidth.Seconds()), ruleID)
	if err != nil {
		return nil, 0, time.Time{}, time.Time{}, 0, fmt.Errorf("episode timeline buckets for rule %d: %w", ruleID, err)
	}
	defer rows.Close()

	byIdx := map[int]*EpisodeBucket{}
	for rows.Next() {
		var idx int
		var state string
		var n int
		if err := rows.Scan(&idx, &state, &n); err != nil {
			return nil, 0, time.Time{}, time.Time{}, 0, fmt.Errorf("scan episode timeline bucket: %w", err)
		}
		b, ok := byIdx[idx]
		if !ok {
			b = &EpisodeBucket{Index: idx}
			byIdx[idx] = b
		}
		if state == StateFiring {
			b.Firing += n
		} else {
			b.Pending += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, time.Time{}, time.Time{}, 0, err
	}

	buckets = make([]EpisodeBucket, 0, len(byIdx))
	for _, b := range byIdx {
		buckets = append(buckets, *b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Index < buckets[j].Index })
	return buckets, bucketWidth, windowStart, windowEnd, total, nil
}

func (s *SQLite) scanScores(rows *sql.Rows) ([]Score, error) {
	defer rows.Close()
	var out []Score
	for rows.Next() {
		var sc Score
		var start, end, computed int64
		var signals string
		if err := rows.Scan(&sc.RuleID, &start, &end, &signals,
			&sc.NoiseScore, &sc.Verdict, &sc.Confidence, &computed); err != nil {
			return nil, fmt.Errorf("scan score: %w", err)
		}
		sc.WindowStart = fromUnix(start)
		sc.WindowEnd = fromUnix(end)
		sc.ComputedAt = fromUnix(computed)
		sc.Signals = map[string]float64{}
		_ = json.Unmarshal([]byte(signals), &sc.Signals)
		out = append(out, sc)
	}
	return out, rows.Err()
}

const scoreCols = `rule_id, window_start, window_end, signals, noise_score, verdict, confidence, computed_at`

// ListLatestScores returns each scored rule's most recent score -- one row
// per rule_id, at that rule's own maximum window_end. This is what the
// leaderboard and /api/rules render: scan can run repeatedly, and only the
// newest verdict for a rule should ever be shown next to it.
func (s *SQLite) ListLatestScores(ctx context.Context) ([]Score, error) {
	q := `
SELECT s.rule_id, s.window_start, s.window_end, s.signals, s.noise_score, s.verdict, s.confidence, s.computed_at
FROM scores s
INNER JOIN (SELECT rule_id, MAX(window_end) AS mw FROM scores GROUP BY rule_id) latest
  ON latest.rule_id = s.rule_id AND latest.mw = s.window_end`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list latest scores: %w", err)
	}
	return s.scanScores(rows)
}

// GetLatestScore returns the most recent score for one rule. ok is false
// when the rule has never been scored (never fired, inactive, or
// ambiguous -- see scanner.Score, which only ever upserts a score for a
// rule that had at least one episode and was neither).
func (s *SQLite) GetLatestScore(ctx context.Context, ruleID int64) (Score, bool, error) {
	q := `SELECT ` + scoreCols + ` FROM scores WHERE rule_id = ? ORDER BY window_end DESC LIMIT 1`
	rows, err := s.db.QueryContext(ctx, q, ruleID)
	if err != nil {
		return Score{}, false, fmt.Errorf("get latest score for rule %d: %w", ruleID, err)
	}
	scores, err := s.scanScores(rows)
	if err != nil {
		return Score{}, false, err
	}
	if len(scores) == 0 {
		return Score{}, false, nil
	}
	return scores[0], true, nil
}

// SetMeta and GetMeta read and write the generic key/value meta table.
// Domain packages (internal/scanner, internal/coverage) use it to persist
// small, singleton pieces of state -- scan statistics for /metrics, the
// latest coverage snapshot for the coverage pages -- without this package
// needing to know what either one means. The value is caller-defined
// (typically a JSON blob); this layer only stores and returns bytes.
func (s *SQLite) SetMeta(ctx context.Context, key, value string) error {
	const q = `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`
	if _, err := s.db.ExecContext(ctx, q, key, value); err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}

// GetMeta returns the stored value for key. ok is false when the key has
// never been set -- e.g. no scan has run yet, or no coverage snapshot has
// ever been saved -- which callers turn into "no data yet" rather than an
// error.
func (s *SQLite) GetMeta(ctx context.Context, key string) (string, bool, error) {
	const q = `SELECT value FROM meta WHERE key = ?`
	var value string
	err := s.db.QueryRowContext(ctx, q, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get meta %q: %w", key, err)
	}
	return value, true, nil
}

// Ping verifies the database connection is alive -- what /healthz checks.
func (s *SQLite) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// UpsertWebhookEpisode records one episode observed by the webhook
// collector (issue #13), reconciled against whatever this series already
// has stored -- from an earlier webhook notification or from the backfill
// -- so a single firing observed by both paths becomes one episode, never
// two.
//
// Reconciliation rule, in order:
//
//  1. Exact match on (rule_id, fingerprint, state, started_at): this is a
//     repeat notification for an episode already known by either path
//     (Alertmanager re-sends a firing group every repeat_interval; a
//     resolved notification shares its firing's startsAt). ended_at is
//     extended, never shrunk, and source is set to webhook: this firing
//     now has webhook-fidelity data available, whichever path wrote the
//     row first.
//
//  2. Otherwise, any existing episode for the same (rule_id, fingerprint,
//     state) whose span OVERLAPS ep's, or is ADJACENT to it within the
//     wider of the two episodes' resolution_seconds: this is the same
//     firing, observed less precisely by one side. The existing row's
//     boundaries are left untouched -- the store's established rule
//     elsewhere is "what is already stored wins" (see InsertEpisodes), and
//     rewriting a backfilled episode's started_at would collide with its
//     own UNIQUE (rule_id, fingerprint, started_at, state) key. Only the
//     metadata (below) is attached.
//
//     The tolerance is the resolution, not a fixed constant, because
//     resolution IS the documented measure of how far off a backfilled
//     boundary can be (see prom.BuildIntervals): a webhook episode ending
//     up to one step after where a backfilled episode for the same series
//     ends is not a second firing, it is the query step's own imprecision.
//
//  3. Otherwise this is a firing neither path has recorded yet -- most
//     often one entirely invisible to the backfill because it started and
//     resolved inside a single query step. A new episode is inserted with
//     Source: SourceWebhook.
//
// In every case, meta is attached (upserted) to the resulting episode's
// id, so the webhook-only fields are never lost even when the episode row
// itself was not touched.
func (s *SQLite) UpsertWebhookEpisode(ctx context.Context, ep Episode, meta WebhookMeta) (id int64, reconciled bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	start, end := unix(ep.StartedAt), unix(ep.EndedAt)
	resSec := int64(ep.Resolution.Seconds())

	const exactQ = `SELECT id, ended_at FROM episodes
	                WHERE rule_id = ? AND fingerprint = ? AND state = ? AND started_at = ?`
	var existingEnd int64
	err = tx.QueryRowContext(ctx, exactQ, ep.RuleID, ep.Fingerprint, ep.State, start).Scan(&id, &existingEnd)
	switch {
	case err == nil:
		newEnd := end
		if existingEnd > newEnd {
			newEnd = existingEnd
		}
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE episodes SET ended_at = ?, source = ? WHERE id = ?`,
			newEnd, SourceWebhook, id); uerr != nil {
			return 0, false, fmt.Errorf("extend webhook episode: %w", uerr)
		}
		reconciled = true

	case errors.Is(err, sql.ErrNoRows):
		matchID, found, ferr := findReconcilableEpisode(ctx, tx, ep, resSec)
		if ferr != nil {
			return 0, false, ferr
		}
		if found {
			id, reconciled = matchID, true
			break
		}
		const insQ = `
INSERT INTO episodes (rule_id, fingerprint, labels, started_at, ended_at,
                      resolution_seconds, source, state)
VALUES (?,?,?,?,?,?,?,?) RETURNING id`
		if ierr := tx.QueryRowContext(ctx, insQ, ep.RuleID, ep.Fingerprint, toJSON(ep.Labels),
			start, end, resSec, SourceWebhook, ep.State).Scan(&id); ierr != nil {
			return 0, false, fmt.Errorf("insert webhook episode: %w", ierr)
		}
		reconciled = false

	default:
		return 0, false, fmt.Errorf("lookup existing episode: %w", err)
	}

	meta.EpisodeID = id
	if merr := upsertWebhookMeta(ctx, tx, meta); merr != nil {
		return 0, false, merr
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit: %w", err)
	}
	return id, reconciled, nil
}

// findReconcilableEpisode implements steps 2 of UpsertWebhookEpisode's
// reconciliation rule: any existing episode of the same series (rule_id,
// fingerprint, state), from either source, whose span overlaps ep's or
// sits within tolerance of it, where tolerance is the wider of the two
// episodes' own resolution_seconds.
//
// One (rule_id, fingerprint, state) series rarely accumulates more than a
// handful of episodes, so this scans them in Go rather than expressing the
// per-row tolerance comparison in SQL.
func findReconcilableEpisode(ctx context.Context, tx *sql.Tx, ep Episode, epResSec int64) (int64, bool, error) {
	const q = `SELECT id, started_at, ended_at, resolution_seconds FROM episodes
	           WHERE rule_id = ? AND fingerprint = ? AND state = ?
	           ORDER BY started_at`
	rows, err := tx.QueryContext(ctx, q, ep.RuleID, ep.Fingerprint, ep.State)
	if err != nil {
		return 0, false, fmt.Errorf("find reconcilable episode: %w", err)
	}
	defer rows.Close()

	newStart, newEnd := unix(ep.StartedAt), unix(ep.EndedAt)

	for rows.Next() {
		var id, start, end, res int64
		if err := rows.Scan(&id, &start, &end, &res); err != nil {
			return 0, false, fmt.Errorf("scan candidate: %w", err)
		}
		tol := res
		if epResSec > tol {
			tol = epResSec
		}

		if newStart < end && start < newEnd {
			return id, true, nil // overlap
		}
		var gap int64
		switch {
		case newStart >= end:
			gap = newStart - end
		case start >= newEnd:
			gap = start - newEnd
		}
		if gap > 0 && gap <= tol {
			return id, true, nil // adjacent, within tolerance
		}
	}
	return 0, false, rows.Err()
}

// upsertWebhookMeta attaches or updates the webhook-only fields for one
// episode. precise_ended_at only ever grows, matching the same
// never-rewrite-history-downward posture InsertEpisodes takes for
// ended_at: a later observation of the same still-firing alert always
// carries a later (or equal) true end, never an earlier one.
func upsertWebhookMeta(ctx context.Context, tx *sql.Tx, meta WebhookMeta) error {
	const q = `
INSERT INTO episode_webhook_meta (episode_id, receiver, group_key, group_labels,
                                  annotations, generator_url, external_url,
                                  precise_started_at, precise_ended_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (episode_id) DO UPDATE SET
  receiver           = excluded.receiver,
  group_key          = excluded.group_key,
  group_labels       = excluded.group_labels,
  annotations        = excluded.annotations,
  generator_url      = excluded.generator_url,
  external_url       = excluded.external_url,
  precise_started_at = excluded.precise_started_at,
  precise_ended_at   = CASE WHEN excluded.precise_ended_at > episode_webhook_meta.precise_ended_at
                            THEN excluded.precise_ended_at
                            ELSE episode_webhook_meta.precise_ended_at END,
  updated_at         = excluded.updated_at`
	_, err := tx.ExecContext(ctx, q, meta.EpisodeID, meta.Receiver, meta.GroupKey,
		toJSON(meta.GroupLabels), toJSON(meta.Annotations), meta.GeneratorURL, meta.ExternalURL,
		unix(meta.PreciseStartedAt), unix(meta.PreciseEndedAt), unix(meta.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert webhook meta for episode %d: %w", meta.EpisodeID, err)
	}
	return nil
}

// GetWebhookMeta fetches the webhook-only fields for one episode. ok is
// false when the episode has no webhook data at all -- the common case for
// a purely backfilled episode.
func (s *SQLite) GetWebhookMeta(ctx context.Context, episodeID int64) (WebhookMeta, bool, error) {
	const q = `
SELECT receiver, group_key, group_labels, annotations, generator_url, external_url,
       precise_started_at, precise_ended_at, updated_at
FROM episode_webhook_meta WHERE episode_id = ?`
	var m WebhookMeta
	m.EpisodeID = episodeID
	var groupLabels, annotations string
	var preciseStart, preciseEnd, updatedAt int64
	err := s.db.QueryRowContext(ctx, q, episodeID).Scan(&m.Receiver, &m.GroupKey, &groupLabels,
		&annotations, &m.GeneratorURL, &m.ExternalURL, &preciseStart, &preciseEnd, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookMeta{}, false, nil
	}
	if err != nil {
		return WebhookMeta{}, false, fmt.Errorf("get webhook meta for episode %d: %w", episodeID, err)
	}
	m.GroupLabels = fromJSONMap(groupLabels)
	m.Annotations = fromJSONMap(annotations)
	m.PreciseStartedAt = fromUnix(preciseStart)
	m.PreciseEndedAt = fromUnix(preciseEnd)
	m.UpdatedAt = fromUnix(updatedAt)
	return m, true, nil
}
