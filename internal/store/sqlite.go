package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type SQLite struct{ db *sql.DB }

func Open(path string) (*SQLite, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)",
		path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
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
