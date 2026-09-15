# noisefloor

Scores Prometheus alert rules against their own firing history, so you can find
the rules that page people for nothing.

Most alert tooling suppresses noise after it is generated. noisefloor looks at
the rule that generated it.

## How it works

Alertmanager keeps no history, which is why most tools ask you to install a
webhook and wait a month. Prometheus, however, already stores `ALERTS` as an
ordinary time series. noisefloor range-queries it and reconstructs every firing
episode for every rule, back to your retention limit.

Point it at a Prometheus URL and you get a scored leaderboard immediately.

## Install

```
go install github.com/SaiPisey2/noisefloor/cmd/noisefloor@latest
```

## Use

```
noisefloor init
noisefloor scan
```

This is real output from the demo stack (`make demo-up && make demo-seed`),
30 days of seeded history:

```
Window     2026-08-15 to 2026-09-14  (30d)
Rules      8 active, 0 inactive
Episodes   4337
Silences   0

NOISE  CONF  VERDICT   GROUP  RULE           FIRES  P50  SHORT  SILENCED  FLAP  COFIRE  CONC  CHURN  NIGHT
49     0.8   tune      demo   DemoFlapping   1436   4m   91%    0%        100%  7%      0%    0%     2%
45     0.8   retire    demo   DemoCauseA     607    3m   100%   0%        0%    100%    0%    0%     5%
45     0.8   retire    demo   DemoCauseB     607    3m   100%   0%        0%    100%    0%    0%     5%
45     0.8   retire    demo   DemoCauseC     607    3m   100%   0%        0%    100%    0%    0%     5%
45     0.8   retire    demo   DemoCauseD     607    3m   100%   0%        0%    100%    0%    0%     5%
31     0.8   retire    demo   DemoSpiky      444    4m   100%   0%        0%    7%      0%    0%     2%
4      0.8   automate  demo   DemoSustained  28     1h   0%     0%        0%    7%      0%    0%     33%
```

This is the output of `make demo-up && make demo-seed && noisefloor scan`;
exact counts shift slightly between runs as the seeded window slides, and
per-episode durations now carry deliberate, bimodal jitter (see Limits) so
the exact NOISE numbers above will not reproduce bit-for-bit either.

`DemoFlapping` re-fires constantly on the same series (`tune`) -- its
`for:` counterfactual is demonstrated below, in Remediation. `DemoCauseA`
through `DemoCauseD` always fire together, alongside whatever they are a
symptom of (`retire`, high cofire). `DemoSpiky` resolves itself before
anyone could act (`retire`, on short_lived_rate alone -- no silence
required, though the e2e test adds one anyway to exercise that path too).
`DemoSustained` fires rarely but for real, sustained periods, and stays
`automate` -- never `retire` -- because it is the one rule in the set worth
a runbook, not a deletion. `DemoQuiet` never fires and does not appear at
all.

NIGHT reads at or near zero for every demo rule because the seeded fires are
spread evenly around the clock. That is the correct answer: the column
measures *disproportionately* nocturnal firing, so a rule that pages as often
at 3pm as at 3am scores 0 on it; a low-fire rule like `DemoSustained` (29
fires) shows more sampling variance around that zero than the high-volume
rules. See the column notes below.

## What the columns mean

- **NOISE** -- 0 to 100, weighted from the five scored signals below.
- **CONF** -- how much the evidence is worth, separately from how bad it looks. A
  rule that fired three times can score terribly and mean nothing. Three things
  have to hold before a rule gets any verdict but `keep`:
  - at least `confidence.min_episodes` fires (default 10) -- the configured
    number is a hard floor, not a ratio to be halved;
  - `CONF` at or above 0.5, where `CONF` is
    `min(fires / min_episodes, observed_window / min_window)`;
  - an observed window long enough to matter. That window is bounded by how
    long the rule has demonstrably existed -- the earlier of when noisefloor
    first saw it and its own first episode -- so a rule added yesterday cannot
    claim a month of observation however hard it fires today.
- **VERDICT** -- see Verdicts below.
- **GROUP** / **RULE** -- the Prometheus rule group and alert name this row
  scores.
- **FIRES** -- episodes counted in the window.
- **P50** -- median firing episode duration, formatted compactly (`3m`, `45s`,
  `1h2m`). Not part of the weighted NOISE sum, but one of three conditions for
  `automate` (alongside FIRES and SHORT): without it on screen, an `automate`
  verdict cites a number nowhere else in the row.
- **SHORT** -- episodes that resolved themselves before anyone could act.
- **SILENCED** -- firing time a human had explicitly silenced.
- **FLAP** -- re-fires on the same series within `flap_window` (default 1h,
  configurable -- see Configuration below).
- **COFIRE** -- episodes that started alongside three or more other rules, which
  is what a cause-based alert riding someone else's incident looks like.
- **CONC** -- concentration: how much of the firing time sits on a small
  number of series. Not part of the weighted NOISE sum, but shown because it
  can route a noisy rule to `tune` instead of `retire`; the table would
  otherwise show a verdict it can't justify.
- **CHURN** -- pending churn: the share of pending periods that never became a
  real alert. Not part of the weighted NOISE sum, but at 50% or higher it
  routes a noisy rule to `tune` instead of `retire` -- the threshold sits too
  close to normal operation, so it needs tuning rather than deletion. Without
  this column that verdict flip had no visible cause.
- **NIGHT** -- how much *more* of this rule's firing lands outside weekday
  09:00-18:00 than you would expect by chance. This is the fifth weighted
  signal behind NOISE (`offhours_rate`).

  The raw share is not useful on its own: 123 of the week's 168 hours are
  outside those working hours, so any rule firing round the clock sits at 73%
  and the number says nothing about the rule. NIGHT is that raw share rescaled
  against the 73% baseline, so a uniformly-firing rule reads 0% and a rule that
  only ever pages at night reads 100%. Hours are counted in the configured
  `timezone`.

## Verdicts

- `retire` -- the weighted noise score crosses the threshold (30), most often
  because the rule self-resolves before anyone can act. Silence is one of the
  five contributing signals, not a precondition -- DemoCauseA-D above reach
  `retire` at 0% silenced, driven by SHORT and COFIRE instead. 30 is where a
  rule that ONLY ever self-resolves becomes actionable: `short_lived_rate`
  alone carries 0.30 of the weight, so nothing else has to be wrong for that
  to be worth saying.
- `tune` -- flapping, concentrated on a few series, or sitting so close to its
  threshold that it keeps almost firing (pending churn). The threshold or
  `for:` is wrong, not the rule.
- `automate` -- real, frequent and long-running. A runbook candidate.
- `keep` -- fine, or not enough evidence to say otherwise.

## Configuration

`noisefloor init` writes a starter `noisefloor.yaml` with the scoring weights
spelled out. They are meant to be edited; there is no model, just a weighted
sum you can read.

`timezone` defaults to `UTC`, and deliberately not to `Local`: it decides which
fires count as off-hours, which is a scored signal, so `Local` would let the
same database produce different verdicts on a CET laptop and in a UTC CI
container. Set your team's working timezone if it is not UTC.

`prometheus.retry_attempts` (default 3) and `prometheus.retry_base_delay`
(default `1s`) control retry on a chunked `ALERTS` query. A 30-day scan at
the default 6h chunk issues 120 of these; a single transient failure --
a 503, a timeout, a connection reset -- used to abort the whole scan and
discard every chunk already reconstructed. Only failures worth retrying
actually wait: a 400 or 422 from Prometheus (a bad query) fails immediately
without spending an attempt, since it will fail identically on retry, and
neither Ctrl-C nor the scan timeout sits through a back-off delay. Episodes
are written to the store as each chunk settles, not only at the end, so a
failure that exhausts every attempt still leaves everything reconstructed
before it in the database.

`flap_window` decides how soon a re-fire on the same series counts as
flapping (feeds `flap_rate`, weighted above). It defaults to `1h`, unchanged
from before this was configurable. It is a fixed, absolute duration, not
scaled by episode length: a rule whose episodes are typically 30 minutes long
and a rule whose episodes are typically 30 seconds long use the same 1h
window unless you set this per-deployment. Scaling the window by episode
duration was considered -- a 30-minute episode recurring hourly arguably
isn't flapping, while a 30-second one clearly is -- but was judged too risky
to default: the natural formula (scale off the rule's own P50 duration)
creates a feedback loop, since P50 is itself computed over episodes whose
boundaries already depend on the gap-merging tolerance in `BuildIntervals`,
and it would silently invalidate the archetype calibration in
`internal/score/verdict.go` (`flapTuneThreshold` was tuned against an
absolute 1h). `flap_window` is therefore a single global knob, not scaled per
rule: widen it if your rules' normal episodes commonly run well over an hour,
narrow it if they normally run in seconds.

## Authentication

Prometheus and Alertmanager each take their own `auth` block, since they
frequently sit behind different gateways. Supported per endpoint: a bearer
token (inline or from a file, re-read on every request so a rotated token
takes effect without a restart), basic auth (username plus an inline or
file-based password), and TLS (a private CA, a client certificate for mutual
TLS, or skipping verification). Bearer and basic auth are mutually exclusive
per endpoint.

Bearer token from a file, the usual shape for a managed Prometheus behind an
oauth2-proxy or similar:

```yaml
prometheus:
  url: https://prometheus.example.com
  auth:
    bearer_token_file: /etc/noisefloor/prometheus-token
```

Mutual TLS against a Prometheus that presents a private CA and expects a
client certificate:

```yaml
prometheus:
  url: https://prometheus.internal:9090
  auth:
    tls:
      ca_file: /etc/noisefloor/ca.pem
      cert_file: /etc/noisefloor/client.pem
      key_file: /etc/noisefloor/client-key.pem
```

`tls.insecure_skip_verify` disables certificate verification entirely. Only
set it against a host you control, such as a self-signed instance on your
own machine -- never a shared or production endpoint.

## Demo

```
make demo-up
make demo-seed
make build && ./noisefloor scan
```

Brings up Prometheus, Alertmanager and a service that emits deliberately noisy
and deliberately healthy alerts, then seeds 30 days of history.

## Remediation

Scoring a rule is not fixing it. `noisefloor remediate` closes that gap: it
opens one pull request per rule against a git checkout of your Prometheus
rule files, proposing exactly what the evidence supports.

```
noisefloor remediate -config noisefloor.yaml
```

**Dry run by default.** The command above opens nothing -- it prints, to
stdout, every PR it would open: the branch name, a unified diff of the
exact lines that would change, and the full PR body, for every rule that
qualifies. Opening PRs for real is explicit opt-in:

```
noisefloor remediate -config noisefloor.yaml -apply -owner myorg -repo alert-rules
```

`-apply` requires `-owner`/`-repo` and a GitHub token:

```
export GITHUB_TOKEN=ghp_...
noisefloor remediate -config noisefloor.yaml -apply -owner myorg -repo alert-rules
```

**Supply the token in `$GITHUB_TOKEN`.** There is also a `-token` flag, but
anything on the command line lands in the process's argv, where every other
user on the machine can read it out of `ps`, and in your shell's history
file. A token does not survive either. Use the flag only where an
environment variable genuinely is not available, and rotate anything you
have already passed that way.

Without `-apply`, `-owner`/`-repo` are still useful -- given, they make the
dry run check GitHub (read-only, no token required for a public repo) for a
PR already open on a rule's branch, so the printed output says "already
open" instead of "would open" for a rule that already has one.

`rules.path` must be configured (see Configuration above): remediate needs
a file and line span for a rule before it can propose an edit to it, and
refuses to run without one.

Two shapes of proposal:

- **retire** -- deletes the rule. The PR body carries the evidence table:
  fires, short-lived % (with the threshold it was measured against, and the
  `for:` that threshold was derived from), silenced % (with who silenced it
  and when), confidence, and the observation window -- everything a
  reviewer needs to check the claim against their own Prometheus, stated as
  a query to run and a count to expect, not just asserted. If Alertmanager
  was unreachable during the scan, the silence row says so rather than
  printing a `0%` nobody measured.
- **tune** -- raises `for:`. The PR body states the counterfactual
  explicitly: *"p90 episode is 4m, current `for: 30s`; `for: 4m30s` would
  have suppressed 46% of past fires and retained 128 of 128 episodes longer
  than 9m."* -- a specific, checkable claim about what the proposed value
  would have done to the rule's own history (see `internal/remediate`'s
  counterfactual code).

Every diff is minimal and surgical: a retire deletes exactly the rule's own
lines -- stopping at its last line of content, so the blank line and doc
comments that introduce the *next* rule stay where they are; a tune changes
only the `for:` line's value, preserving its indentation and any trailing
comment, or inserts a new `for:` line when the rule has none. A written or
inserted line takes the file's own line ending, so a CRLF rule file does
not come back mixed.
Never a PR touching more than one rule -- a PR touching twenty rules gets
closed wholesale, and the tool would be dead on arrival.

**What it refuses to touch**, and why -- these are all already computed by
`scan`, wired in rather than re-derived:

- a rule defined in more than one rule group (ambiguous: its episodes
  cannot be attributed to either definition);
- an inactive rule (Prometheus no longer evaluates it);
- a rule retuned inside the observation window (its episodes belong to an
  expression that no longer exists);
- a rule below the confidence floor (not enough evidence to propose
  anything);
- a rule noisefloor cannot locate in the configured checkout;
- a rule whose `(group, alertname)` the checkout defines more than once --
  including twice inside a single group, which is legal Prometheus and
  which the cross-group ambiguity check above cannot see. There is no way
  to tell which definition the scored history belongs to;
- a rule whose file no longer matches what Prometheus evaluates. Every
  number in the PR body is measured against the live rule and every line of
  the diff is computed against the file, so a checkout that has fallen
  behind produces a PR arguing one case and performing a different edit.
  The refusal names both values;
- any rule in a file holding more than one YAML document. Rule positions
  are line numbers within one document, so a span bounded by the end of the
  file would run straight through every document after it.

Two more refusals happen at the forge rather than at the proposal:

- **a PR a maintainer closed is not reopened.** Branch names are
  deterministic per proposal, so a closed PR is an answer to this exact
  proposal. noisefloor reports it and moves on; it proposes again only when
  the proposal itself changes, which changes the branch.
- **a commit is refused if the remote has moved.** Committing writes a
  whole file, so before writing, noisefloor reads the file on both the base
  and the bot's branch and requires them to match the checkout the edit was
  computed from, byte for byte. Anything else would silently revert work
  the run never saw -- under a PR titled "retire X". Re-run against a fresh
  checkout.

**Idempotent.** Running the bot twice does not open a second PR for the
same rule and the same proposal: each proposal's branch name is
deterministic (derived from the rule and, for a tune, the specific
candidate value proposed), and remediate checks for an already-open PR on
that branch before opening a new one. A materially different proposal
(the candidate `for:` value changed since the last run) gets a new branch
and a new PR, rather than silently rewriting one a human may already be
reviewing.

**GitHub first.** The provider that actually talks to a forge sits behind
a small interface (`internal/pr.Provider`); a GitLab implementation is a
new type behind that same interface, not a restructuring.

## Coverage

`scan` finds rules that page for nothing. `noisefloor coverage` finds the
opposite problem: services with no alerting on some signal at all.

```
noisefloor coverage -config noisefloor.yaml
```

It discovers services from Prometheus's own `up` series (grouped by
Kubernetes namespace/service SD labels where present, otherwise by `job`),
classifies every alerting rule Prometheus evaluates by which of five
signals it covers (rate, errors, latency, saturation, burn_rate), and
prints a grid plus a blind-spot list ranked by traffic -- a service
carrying real load with zero alerting sorts at the top. `-detail` shows the
rule and reasoning behind every cell, including which classifications are
a confident read versus a guess (`v?`).

## Propose starter rules for blind spots

Finding a blind spot is not fixing it. `noisefloor propose` closes that
gap for a service with **no alert coverage at all**: it opens one pull
request per service, adding a single starter rule for the most
fundamental missing signal, reusing the exact same PR machinery
`remediate` does (branch, diff, body, dry run, idempotence).

```
noisefloor propose -config noisefloor.yaml
```

**This is a fundamentally riskier kind of proposal than retire/tune**, and
the PR body says so up front. A retire or tune proposal is backed by
thirty days of a real rule's own firing history; a starter rule has never
existed, so there is no history to derive a threshold from. Every starter
body carries an explicit warning that its threshold is a conservative
starting point requiring tuning, never a recommendation -- the one thing
in the body that is NOT evidence-backed the way everything else noisefloor
emits is. Where the service's own current metrics allow it (a p99 latency,
an error ratio, a resident-memory reading), the threshold is scaled from
that real number and the body says so and shows the arithmetic; otherwise
it falls back to a fixed, deliberately loose default and says that too.

Four templates, one per signal noisefloor can hand-author a rule for
(`rate`, `errors`, `latency`, `saturation` -- `burn_rate` rules are
generator output, not something to propose): "traffic disappeared"
(`absent()`, no threshold to guess at all), error ratio, p99 latency, and
resident memory. Each only ever names a metric the service was confirmed,
by an instant Prometheus query, to actually expose -- verified again after
the fact by running the generated expression back through coverage's own
classifier and checking it lands on the intended signal. Priority order is
rate, errors, latency, saturation: at most one starter rule is proposed
per blind-spot service per run, the same way remediate never touches more
than one rule per PR, scaled up one level -- a service that has never had
any alerting does not get four simultaneous pull requests the first time
noisefloor looks at it.

**Placement policy.** A retire or tune proposal edits a rule that already
has a home; a blind spot's whole problem is that no rule exists for it at
all. `propose` will not guess a rule file's path or invent a new group --
either is a guess about a team's own layout conventions that a wrong
answer turns into a PR that does not apply cleanly. Instead:

- if some OTHER signal already has a certain, specifically-matched rule
  covering this exact service, the new rule is appended to that rule's own
  group;
- otherwise, `coverage.rule_targets` in config names an existing group
  explicitly (`rule_targets: {search: {file: ./rules/services.yml, group:
  services}}`), verified to actually exist before being trusted;
- otherwise, the service is refused (reported alongside every other
  refusal) and the operator is asked to nominate one.

**Refusals**, reported the same way remediate's are: an idle service (see
coverage's own idle threshold), a signal the service has no recognised
metric for, existing coverage on the signal that is a guess rather than
certain (propose will not stack a second rule on an unverified reading),
and an unresolvable target.

**Dry run by default**, exactly like `remediate`.

## Limits

- The demo waveforms (`demo/seed`, `demo/faultgen`) carry deterministic,
  bimodal jitter on `DemoSpiky` and `DemoFlapping`'s on-periods -- most
  episodes short (3m or 4m), `DemoFlapping` additionally drawing a long
  episode (15m-17m) about 1 cycle in 10 -- seeded from the cycle index so
  re-seeding reproduces byte-identically. Real alerts do not fire for
  exactly the same duration every time, and a fixed duration made the
  `tune` counterfactual above demonstrate nothing -- every candidate `for:`
  landed exactly on the retain/suppress boundary and suppressed 0%. The
  long band exists so that counterfactual has something real to retain,
  not just suppress; `DemoSpiky` has no long band, since its noise score
  is calibrated with only one point of margin above the retire threshold
  and any real fraction of long episodes would cost it that verdict.
- Episode precision is bounded by the query step; alerts shorter than one step
  are undercounted.
- The scan window ends on a `prometheus.step` boundary, so it can lag the
  moment you ran the scan by up to one step. That is deliberate: Prometheus
  aligns a range query's samples to the query start, so an unanchored window
  returns the same firings at shifted timestamps, and the store records them
  as additional episodes rather than the ones it already holds. Anchoring the
  window keeps repeated scans -- the intended usage -- returning the same
  verdicts.
- An episode that overlaps one already stored for the same rule, labelset and
  state is discarded rather than added: one series cannot be firing twice at
  once, so an overlap is the same firing observed again at a different
  resolution or window offset. What was stored first is kept, at the step it
  was first recorded with, so changing `prometheus.step` between scans does
  not double-count history. Delete the database to re-derive it at the new
  step.
- A chunk that still fails after every retry attempt aborts the scan, same as
  before retries existed -- retry absorbs transient failures, it does not
  make Prometheus unavailability invisible. Episodes settled in earlier
  chunks are already written to the store by then, but each series' most
  recently observed episode is deliberately held back until either the next
  chunk confirms it did not continue, or the whole scan finishes -- so it may
  not be written until a later, successful scan re-observes it.
- History is bounded by Prometheus retention. The whole requested window is
  always queried -- Prometheus simply returns nothing outside retention -- and
  when the earliest episode found is well inside the window the report says
  where data actually begins rather than guessing at the cause. Confidence is
  then measured against the span that exists, not the span requested.
- An alert name defined in more than one rule group is not scored, and the
  report says how many were skipped. The `ALERTS` series records an alertname
  but no group, so there is no way to tell whose episodes are whose; scoring
  one rule on both histories would produce a confident verdict about the wrong
  rule.
- Rules that no longer exist keep their history but are never scored or
  proposed for change. If a new rule is later created with the same alert name
  as a deleted one, it inherits the deleted rule's episodes -- the `ALERTS`
  series cannot distinguish them -- and will be scored on that history as
  though it were its own.
- Confidence is measured from the episodes reconstructed in the current run.
  A database that has outlived Prometheus retention still scores rules on
  everything it has stored, but caps their confidence at the span Prometheus
  can still answer for. On a quiet install this shortens confidence windows
  slightly. Both effects err toward `keep`, never toward proposing a deletion.
- A rule whose expression changed inside the window is shown but not given a
  verdict. Its history belongs to the old expression.
- Tolerating a one-sample gap slightly inflates episode duration and slightly
  deflates episode count, which biases against calling a rule flappy.
- If Alertmanager is unreachable, `silenced_rate` falls back to whatever the
  store already holds from an earlier scan -- it does not read as zero unless
  the store has nothing either.
- If more than `rules.max_deactivated_fraction` (default 0.2) of previously
  active rules are missing from a run, the scan refuses rather than
  deactivating them, and names which ones -- a rule file that stopped parsing
  should stop the scan, not quietly empty the report. A smaller drop proceeds
  normally, with the missing rules printed to stderr so routine cleanup is
  still visible.

## License

Apache 2.0

## Contributing

Run `git config core.hooksPath .githooks` after cloning. The hooks reject
commit messages carrying tool attribution, and CI enforces the same rule
across the whole history.
