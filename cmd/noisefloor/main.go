// Command noisefloor scores Prometheus alert rules against their own firing
// history, so noisy rules can be found and fixed at the source.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
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

alertmanager:
  url: http://localhost:9093

database: noisefloor.db
window: 30d
timeout: 10m
timezone: Local

# Weights must sum to 1.0. Only these five signals move the noise score.
weights:
  short_lived_rate: 0.30
  silenced_rate: 0.25
  flap_rate: 0.20
  cofire_ratio: 0.15
  offhours_rate: 0.10

confidence:
  min_episodes: 10
  min_window: 14d
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
	ruleSync, err := collect.SyncRules(ctx, groups, db, now)
	if err != nil {
		return err
	}

	backfill, err := collect.New(api, db, cfg).Run(ctx, now.Add(-cfg.Window.Std()), now)
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
		fetched, ferr := collect.FetchSilences(ctx, cfg.Alertmanager.URL, nil)
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

	window := backfill.WindowEnd.Sub(backfill.WindowStart)
	var rows []report.Row

	for _, r := range rules {
		if !r.Active {
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
		})
		noise, confidence, verdict := score.Evaluate(signals, window, cfg)

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
		Truncated:         backfill.Truncated,
		RulesActive:       ruleSync.Active,
		RulesInactive:     inactive,
		Episodes:          backfill.Episodes,
		Silences:          len(silences),
		SilencesAvailable: silencesAvailable,
	})
}
