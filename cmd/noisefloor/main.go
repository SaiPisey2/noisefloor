// Command noisefloor scores Prometheus alert rules against their own firing
// history, so noisy rules can be found and fixed at the source.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
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

	if _, err := os.Stat(*path); err == nil {
		return fmt.Errorf("%s already exists", *path)
	}
	if err := os.WriteFile(*path, []byte(sampleConfig), 0o644); err != nil {
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

	ctx := context.Background()
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

	var silences []store.Silence
	if cfg.Alertmanager.URL != "" {
		silences, err = collect.FetchSilences(ctx, cfg.Alertmanager.URL, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: silences unavailable, silenced_rate will read 0: %v\n", err)
		} else if err := db.UpsertSilences(ctx, silences); err != nil {
			return err
		}
	}

	rules, err := db.ListRules(ctx)
	if err != nil {
		return err
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
		WindowStart:   backfill.WindowStart,
		WindowEnd:     backfill.WindowEnd,
		Truncated:     backfill.Truncated,
		RulesActive:   ruleSync.Active,
		RulesInactive: ruleSync.Deactivated,
		Episodes:      backfill.Episodes,
		Silences:      len(silences),
	})
}
