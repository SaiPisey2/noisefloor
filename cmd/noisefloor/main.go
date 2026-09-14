// Command noisefloor scores Prometheus alert rules against their own firing
// history, so noisy rules can be found and fixed at the source.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/collect"
	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/report"
	"github.com/SaiPisey2/noisefloor/internal/score"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

const sampleConfig = `prometheus:
  url: http://localhost:9090
  step: 1m
  chunk: 6h
  # A single chunk query can fail transiently (a 503, a timeout, a connection
  # reset) without the rest of the scan being at fault. retry_attempts is the
  # max tries per chunk, including the first (1 = no retry); each retryable
  # failure backs off retry_base_delay * 2^(attempt-1) -- 1s, 2s, 4s at the
  # defaults below. A 400/422 (a bad query) or other non-transient failure is
  # never retried, and Ctrl-C or the scan timeout is never waited out.
  retry_attempts: 3
  retry_base_delay: 1s
  # Prometheus and Alertmanager often sit behind different gateways, so each
  # takes its own auth block. Uncomment and fill in what your setup needs;
  # bearer, basic and tls can all be left out for an unauthenticated demo.
  # auth:
  #   bearer_token_file: /etc/noisefloor/prometheus-token   # re-read per request
  #   # bearer_token: inline-token-for-quick-testing
  #   # username: alice
  #   # password_file: /etc/noisefloor/prometheus-password
  #   # tls:
  #   #   ca_file: /etc/noisefloor/ca.pem
  #   #   cert_file: /etc/noisefloor/client.pem
  #   #   key_file: /etc/noisefloor/client-key.pem
  #   #   insecure_skip_verify: false   # only true against a host you control

alertmanager:
  url: http://localhost:9093
  # auth:
  #   bearer_token_file: /etc/noisefloor/alertmanager-token

database: noisefloor.db
window: 30d
timeout: 10m

# Which fires count as off-hours, and off-hours is a scored signal. Keep this
# explicit: "Local" would score the same database differently on a CET laptop
# and in a UTC CI container. Set your team's working timezone if it is not UTC.
timezone: UTC

# Weights must sum to 1.0. Only these five signals move the noise score.
weights:
  short_lived_rate: 0.30
  silenced_rate: 0.25
  flap_rate: 0.20
  cofire_ratio: 0.15
  offhours_rate: 0.10

# How soon a re-fire on the same series counts as flapping (feeds flap_rate,
# weighted above). 1h is the long-standing default; lower it for rules that
# should never legitimately re-fire within the hour, or raise it if hourly
# paging is normal for your environment.
flap_window: 1h

confidence:
  min_episodes: 10
  min_window: 14d

# Guards against a broken rule file being mistaken for a deliberate cleanup.
# Above this fraction of previously active rules disappearing from Prometheus
# in one run, the scan refuses to deactivate them and names the missing rules
# instead of silently shrinking the report.
rules:
  max_deactivated_fraction: 0.2
`

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "scan":
		if err := runScan(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `noisefloor scores Prometheus alert rules against their own history.

usage:
  noisefloor scan [-config noisefloor.yaml]
  noisefloor init [-config noisefloor.yaml]
`)
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	path := fs.String("config", "noisefloor.yaml", "path to write")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// O_EXCL rather than Stat-then-Write: same length, no window between the
	// check and the write, and a Stat error other than "not exists" cannot be
	// misread as "absent".
	f, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", *path, err)
	}
	defer f.Close()

	if _, err := f.WriteString(sampleConfig); err != nil {
		return fmt.Errorf("write %s: %w", *path, err)
	}
	fmt.Printf("wrote %s\n", *path)
	return nil
}

// scanWindow returns the query window for a scan started at now: `to` rounded
// DOWN to a multiple of step, and `from` derived from that rounded value.
//
// The truncation is load-bearing for deduplication, not tidiness. Prometheus
// aligns the sample grid of a range query to the query start, so a window
// beginning one second later comes back with every sample timestamp shifted by
// that second. Shifted samples reconstruct into episodes with shifted
// started_at values, and the store's
// UNIQUE (rule_id, fingerprint, started_at, state) then sees new rows instead
// of upserting the ones it already holds: the same real firings get stored
// twice, adjacent in time. flap_rate reads adjacent episodes as re-fires, so a
// rule scanned repeatedly -- on a cron, which is how this is meant to be used
// -- drifts from `retire` to `tune` with no change in its actual behaviour.
// Anchoring `to` to the step grid makes every scan within the same step ask
// for exactly the same window, so the grid, the episodes and the rows are
// identical and the upsert deduplicates as intended.
//
// time.Time.Truncate rounds down against absolute time since the zero instant,
// which is the fixed grid this needs; it is not a local-midnight or
// wall-clock-relative operation.
func scanWindow(now time.Time, window, step time.Duration) (from, to time.Time) {
	if step <= 0 {
		// config.Validate rejects it, but do not silently return a zero
		// window if it is ever reached.
		return now.Add(-window), now
	}
	to = now.Truncate(step)
	return to.Add(-window), to
}

func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfgPath := fs.String("config", "noisefloor.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// A scan issues one range query per chunk -- 120 of them over a 30-day
	// window at the default 6h chunk. Without a deadline a hung Prometheus
	// hangs the command forever, and this is a thing people run in CI.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Std())
	defer cancel()

	db, err := store.Open(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	api, err := prom.New(cfg.Prometheus)
	if err != nil {
		return err
	}

	// Rules first, so episodes attach to rules that know their group.
	groups, err := api.Rules(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	ruleSync, err := collect.SyncRules(ctx, groups, db, now, cfg.Rules.MaxDeactivatedFraction)
	if err != nil {
		return err
	}

	// A bare deactivation count cannot tell an operator "rules I deleted on
	// purpose" from "a rule file that stopped parsing this morning" -- name
	// them, grouped by rule group, so a real emergency is recognisable at a
	// glance instead of looking like routine cleanup. (Already sorted by group
	// then name.)
	if len(ruleSync.DeactivatedRules) > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d rule(s) deactivated this run (active before, no longer "+
				"reported by prometheus):\n", len(ruleSync.DeactivatedRules))
		var curGroup string
		for _, d := range ruleSync.DeactivatedRules {
			if d.GroupName != curGroup {
				fmt.Fprintf(os.Stderr, "  %s:\n", d.GroupName)
				curGroup = d.GroupName
			}
			fmt.Fprintf(os.Stderr, "    - %s\n", d.AlertName)
		}
	}

	// An alert name defined by two groups cannot be attributed: the ALERTS
	// series carries an alertname and no group, so both rules' episodes land on
	// whichever row the lookup returns, the other rule is silently skipped for
	// having none, and the survivor is judged -- with its own for: -- on a
	// history that is partly someone else's. That produces a confident retire
	// justified by a different rule. Refuse to score them and say so.
	ambiguous := map[string]bool{}
	if len(ruleSync.AmbiguousNames) > 0 {
		for _, n := range ruleSync.AmbiguousNames {
			ambiguous[n] = true
		}
		fmt.Fprintf(os.Stderr,
			"warning: %d alert name(s) defined in more than one rule group: %s\n"+
				"         the ALERTS series records no group, so their episodes cannot be\n"+
				"         attributed to one rule; they will NOT be scored. Rename them or\n"+
				"         remove the duplicate definition.\n",
			len(ruleSync.AmbiguousNames),
			strings.Join(ruleSync.AmbiguousNames, ", "))
	}

	from, to := scanWindow(now, cfg.Window.Std(), cfg.Prometheus.Step.Std())
	backfill, err := collect.New(api, db, cfg).Run(ctx, from, to)
	if err != nil {
		return err
	}

	// silenced_rate carries a quarter of the score, so a failure here is not a
	// detail: every rule scores up to 25 points lower and a `retire` can become
	// a `keep`. Record whether the signal was actually available so the report
	// can say "unavailable" rather than print a 0 that looks like a measurement.
	silencesAvailable := true
	switch {
	case cfg.Alertmanager.URL == "":
		silencesAvailable = false
		fmt.Fprintln(os.Stderr,
			"warning: no alertmanager.url configured; silenced_rate reads 0 for every rule")
	default:
		amRT, rtErr := cfg.Alertmanager.Auth.Transport()
		if rtErr != nil {
			return fmt.Errorf("alertmanager client: %w", rtErr)
		}
		amClient := &http.Client{Transport: amRT, Timeout: 30 * time.Second}
		fetched, ferr := collect.FetchSilences(ctx, cfg.Alertmanager.URL, amClient)
		if ferr != nil {
			silencesAvailable = false
			fmt.Fprintf(os.Stderr,
				"warning: alertmanager unreachable, silenced_rate reads 0 for every rule: %v\n", ferr)
		} else if err := db.UpsertSilences(ctx, fetched); err != nil {
			return err
		}
	}

	// Score against everything ever observed, not just this fetch. Alertmanager
	// garbage-collects expired silences (120h by default), so a 30-day window
	// can only ever see the older ones from our own store -- which is the whole
	// reason we persist them.
	silences, err := db.ListSilences(ctx, backfill.WindowStart, backfill.WindowEnd)
	if err != nil {
		return err
	}

	rules, err := db.ListRules(ctx)
	if err != nil {
		return err
	}

	// Count the store's inactive total, not how many this run deactivated --
	// the run after a rule disappears would otherwise report 0 inactive while
	// the store holds one.
	inactive := 0
	for _, r := range rules {
		if !r.Active {
			inactive++
		}
	}
	allEpisodes, err := db.ListEpisodesInWindow(ctx, backfill.WindowStart, backfill.WindowEnd)
	if err != nil {
		return err
	}

	byRule := map[int64][]store.Episode{}
	for _, e := range allEpisodes {
		byRule[e.RuleID] = append(byRule[e.RuleID], e)
	}

	// Confidence is earned against time actually observed, not time requested.
	// A fresh Prometheus holding three days of history must not score a rule as
	// though it had been watched for thirty; equally, a full window must not be
	// shortened by a probe's guess. EarliestData is measured from the episodes
	// themselves, so this is exact either way.
	window := backfill.WindowEnd.Sub(backfill.WindowStart)
	if backfill.Truncated && !backfill.EarliestData.IsZero() {
		window = backfill.WindowEnd.Sub(backfill.EarliestData)
	}
	var rows []report.Row

	for _, r := range rules {
		if !r.Active {
			continue
		}
		if ambiguous[r.AlertName] {
			continue
		}
		eps := byRule[r.ID]
		if len(eps) == 0 {
			continue
		}

		signals := score.Compute(score.Input{
			Rule:        r,
			Episodes:    eps,
			AllEpisodes: allEpisodes,
			Silences:    silences,
			Location:    cfg.Location(),
			FlapWindow:  cfg.FlapWindow.Std(),
		})
		noise, confidence, verdict := score.Evaluate(signals, r, window, now, cfg)

		// A rule retuned inside the window earned these episodes under an
		// expression that no longer exists. Report the numbers, withhold the
		// judgement.
		if collect.RetunedDuring(r, backfill.WindowStart) {
			verdict = score.VerdictKeep
			confidence = 0
		}

		if err := db.UpsertScore(ctx, &store.Score{
			RuleID:      r.ID,
			WindowStart: backfill.WindowStart,
			WindowEnd:   backfill.WindowEnd,
			Signals:     signals.Map(),
			NoiseScore:  noise,
			Verdict:     verdict,
			Confidence:  confidence,
			ComputedAt:  now,
		}); err != nil {
			return err
		}

		rows = append(rows, report.Row{
			AlertName: r.AlertName, GroupName: r.GroupName,
			Verdict: verdict, Noise: noise, Confidence: confidence,
			Signals: signals,
		})
	}

	return report.Render(os.Stdout, rows, report.Meta{
		WindowStart:       backfill.WindowStart,
		WindowEnd:         backfill.WindowEnd,
		EarliestData:      backfill.EarliestData,
		Truncated:         backfill.Truncated,
		RulesActive:       ruleSync.Active,
		RulesInactive:     inactive,
		Episodes:          backfill.Episodes,
		Silences:          len(silences),
		SilencesAvailable: silencesAvailable,
		Ambiguous:         len(ruleSync.AmbiguousNames),
	})
}
