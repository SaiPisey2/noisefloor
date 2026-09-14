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
Episodes   6763
Silences   0

NOISE  CONF  VERDICT   GROUP  RULE           FIRES  P50  SHORT  SILENCED  FLAP  COFIRE  CONC  CHURN  NIGHT
51     0.8   tune      demo   DemoFlapping   3885   4m   100%   0%        100%  7%      0%    0%     1%
45     0.8   retire    demo   DemoCauseA     602    3m   100%   0%        0%    100%    0%    0%     0%
45     0.8   retire    demo   DemoCauseB     602    3m   100%   0%        0%    100%    0%    0%     0%
45     0.8   retire    demo   DemoCauseC     602    3m   100%   0%        0%    100%    0%    0%     0%
45     0.8   retire    demo   DemoCauseD     602    3m   100%   0%        0%    100%    0%    0%     0%
31     0.8   retire    demo   DemoSpiky      440    3m   100%   0%        0%    7%      0%    0%     0%
3      0.8   automate  demo   DemoSustained  29     1h   3%     0%        0%    7%      0%    0%     10%
```

This is the output of `make demo-up && make demo-seed && noisefloor scan`;
exact counts shift slightly between runs as the seeded window slides.

`DemoFlapping` re-fires constantly on the same series (`tune`). `DemoCauseA`
through `DemoCauseD` always fire together, alongside whatever they are a
symptom of (`retire`, high cofire). `DemoSpiky` resolves itself before anyone
could act (`retire`). `DemoSustained` fires rarely but for real, sustained
periods, and stays `automate` -- never `retire` -- because it is the one rule
in the set worth a runbook, not a deletion. `DemoQuiet` never fires and does
not appear at all.

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

## Limits

- Episode precision is bounded by the query step; alerts shorter than one step
  are undercounted.
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
