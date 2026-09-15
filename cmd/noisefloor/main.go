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

	"github.com/SaiPisey2/noisefloor/internal/collect/prom"
	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/coverage"
	"github.com/SaiPisey2/noisefloor/internal/pr"
	"github.com/SaiPisey2/noisefloor/internal/report"
	"github.com/SaiPisey2/noisefloor/internal/scanner"
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
  # A git checkout of the rule files Prometheus loads. Set this to get
  # file+line locations for each rule (store.Rule.Line) and a warning for
  # any mismatch between what's in these files and what Prometheus actually
  # evaluates. Left blank, rules are still scored -- just without a location
  # to point a remediation PR at.
  # path: ./rules
  max_deactivated_fraction: 0.2

# noisefloor coverage only; scan and remediate never read this. Discovery
# from up{} plus Kubernetes namespace/service SD labels covers most setups
# without any of this being set.
# coverage:
#   services: [checkout, billing]   # include even if up{} doesn't name them
#   exclude_jobs: [prometheus]      # default; set to [] to see Prometheus's own gap
#   # noisefloor propose only. Names the existing rule group a starter rule
#   # for this service should be appended to, when propose cannot find one
#   # covering the service already. Never a new file or a new group -- see
#   # the README's Coverage remediation section for why.
#   rule_targets:
#     search: {file: ./rules/services.yml, group: services}
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
	case "remediate":
		if err := runRemediate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "coverage":
		if err := runCoverage(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "propose":
		if err := runPropose(os.Args[2:]); err != nil {
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
  noisefloor remediate [-config noisefloor.yaml] [-apply] [-owner OWNER -repo REPO]
  noisefloor coverage [-config noisefloor.yaml] [-detail]
  noisefloor propose [-config noisefloor.yaml] [-apply] [-owner OWNER -repo REPO]

Opening pull requests (-apply) needs a GitHub token in $GITHUB_TOKEN.
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

	now := time.Now().UTC()
	result, err := scanner.RunFull(ctx, cfg, db, api, now)
	if err != nil {
		return err
	}

	return report.Render(os.Stdout, result.Rows, result.Meta)
}

// runRemediate scores rules exactly as `scan` does (via scanner.RunFull),
// then builds a pull-request proposal for every rule that qualifies:
// GitHub first, one PR per rule, dry-run unless -apply is passed.
func runRemediate(args []string) error {
	fs := flag.NewFlagSet("remediate", flag.ExitOnError)
	cfgPath := fs.String("config", "noisefloor.yaml", "config file")
	apply := fs.Bool("apply", false, "open PRs for real (default: dry run, prints what would be opened)")
	owner := fs.String("owner", "", "GitHub repository owner (required with -apply)")
	repoName := fs.String("repo", "", "GitHub repository name (required with -apply)")
	base := fs.String("base", "main", "base branch to open PRs against")
	// Supply the token in $GITHUB_TOKEN. A value passed on the command line
	// is in this process's argv, which every user on the machine can read
	// out of `ps`, and which the shell writes to its history file --
	// neither of which a token survives being in. The flag stays for the
	// rare case where an environment variable is not available.
	tokenFlag := fs.String("token", "",
		"GitHub token; prefer $GITHUB_TOKEN, since argv is visible in ps and recorded in shell history")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Rules.Path == "" {
		return fmt.Errorf("rules.path is not configured; remediate needs a git checkout of the " +
			"rule files to locate each rule in and edit (see README's Remediation section)")
	}

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

	now := time.Now().UTC()
	result, err := scanner.RunFull(ctx, cfg, db, api, now)
	if err != nil {
		return err
	}

	// The idempotence check (FindOpenPR) is a read-only GitHub call, so it
	// runs in dry run too whenever -owner/-repo are given -- a dry run
	// against a repo that already has an open PR for a rule should say so,
	// not just repeat "would open" forever. A token is only REQUIRED with
	// -apply (EnsureBranch/CommitFiles/OpenPR need write access); a public
	// repo's PRs can be listed unauthenticated. With no -owner/-repo at
	// all, there is no repository to check against, so a fake provider
	// stands in and the idempotence check is simply skipped.
	var provider pr.Provider
	switch {
	case *owner != "" && *repoName != "":
		// $GITHUB_TOKEN first: it is the one that does not leak. -token is
		// the fallback, and it is checked second so an environment already
		// carrying a token is what gets used by default.
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			token = *tokenFlag
		}
		if *apply && token == "" {
			return fmt.Errorf("-apply requires a GitHub token: set $GITHUB_TOKEN " +
				"(avoid -token, which puts the token in argv, visible in ps and shell history)")
		}
		provider = pr.NewGitHubProvider(token)
	case *apply:
		return fmt.Errorf("-apply requires -owner and -repo (which GitHub repository to open PRs against)")
	default:
		provider = pr.NewFakeProvider()
	}

	runResult, err := pr.Run(ctx, provider, result.Evals, pr.RunOptions{
		Owner: *owner, Repo: *repoName, Base: *base, RepoRoot: cfg.Rules.Path, Apply: *apply,
	})
	if err != nil {
		return err
	}

	pr.WriteResult(os.Stdout, runResult, *apply)
	return nil
}

// runCoverage finds services with no alert coverage at all -- the inverse
// of scan, which finds rules that alert for nothing. It is a separate
// subcommand rather than a `scan` flag: it answers a different question
// (which services exist and what signals alert on them, not how a rule's
// own history behaves), against different Prometheus queries (`up` and a
// traffic proxy, not ALERTS), and scan's report table is already fourteen
// columns wide -- a coverage grid bolted onto it would serve neither
// reading well.
func runCoverage(args []string) error {
	fs := flag.NewFlagSet("coverage", flag.ExitOnError)
	cfgPath := fs.String("config", "noisefloor.yaml", "config file")
	detail := fs.Bool("detail", false, "also print the rule and reason behind every classification")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Std())
	defer cancel()

	api, err := prom.New(cfg.Prometheus)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	result, err := coverage.Run(ctx, api, cfg, now)
	if err != nil {
		return err
	}

	if err := coverage.Render(os.Stdout, result); err != nil {
		return err
	}
	if *detail {
		fmt.Println()
		return coverage.RenderDetail(os.Stdout, result.Grid)
	}
	return nil
}

// runPropose closes the loop coverage opened: for every blind-spot service
// coverage.Run finds (no alerting at all on some signal), it proposes ONE
// starter rule as a pull request, reusing the exact same PR machinery
// runRemediate does -- FindPR-then-open, dry-run by default, -apply opt-in,
// $GITHUB_TOKEN over -token. See internal/pr/starter.go's package doc
// comment for why a starter proposal is a fundamentally different, more
// conservative kind of claim than a retire or a tune, and why only one
// rule is proposed per service per run.
func runPropose(args []string) error {
	fs := flag.NewFlagSet("propose", flag.ExitOnError)
	cfgPath := fs.String("config", "noisefloor.yaml", "config file")
	apply := fs.Bool("apply", false, "open PRs for real (default: dry run, prints what would be opened)")
	owner := fs.String("owner", "", "GitHub repository owner (required with -apply)")
	repoName := fs.String("repo", "", "GitHub repository name (required with -apply)")
	base := fs.String("base", "main", "base branch to open PRs against")
	tokenFlag := fs.String("token", "",
		"GitHub token; prefer $GITHUB_TOKEN, since argv is visible in ps and recorded in shell history")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Rules.Path == "" {
		return fmt.Errorf("rules.path is not configured; propose needs a git checkout of the " +
			"rule files to append a starter rule to (see README's Coverage remediation section)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout.Std())
	defer cancel()

	api, err := prom.New(cfg.Prometheus)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	result, err := coverage.Run(ctx, api, cfg, now)
	if err != nil {
		return err
	}

	var provider pr.Provider
	switch {
	case *owner != "" && *repoName != "":
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			token = *tokenFlag
		}
		if *apply && token == "" {
			return fmt.Errorf("-apply requires a GitHub token: set $GITHUB_TOKEN " +
				"(avoid -token, which puts the token in argv, visible in ps and shell history)")
		}
		provider = pr.NewGitHubProvider(token)
	case *apply:
		return fmt.Errorf("-apply requires -owner and -repo (which GitHub repository to open PRs against)")
	default:
		provider = pr.NewFakeProvider()
	}

	runResult, err := pr.RunStarters(ctx, provider, api, cfg, result, now, pr.RunOptions{
		Owner: *owner, Repo: *repoName, Base: *base, RepoRoot: cfg.Rules.Path, Apply: *apply,
	})
	if err != nil {
		return err
	}

	pr.WriteResult(os.Stdout, runResult, *apply)
	return nil
}
