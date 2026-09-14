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
Episodes   6388
Silences   0

NOISE  CONF  VERDICT   GROUP  RULE           FIRES  SHORT  SILENCED  FLAP  COFIRE  CONC  NIGHT
51     1.0   tune      demo   DemoFlapping   3667   100%   0%        100%  7%      0%    1%
45     1.0   retire    demo   DemoCauseA     567    100%   0%        0%    100%    0%    0%
45     1.0   retire    demo   DemoCauseB     567    100%   0%        0%    100%    0%    0%
45     1.0   retire    demo   DemoCauseC     567    100%   0%        0%    100%    0%    0%
45     1.0   retire    demo   DemoCauseD     567    100%   0%        0%    100%    0%    0%
31     1.0   retire    demo   DemoSpiky      418    100%   0%        0%    7%      0%    0%
1      1.0   automate  demo   DemoSustained  28     0%     0%        0%    7%      0%    0%
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

NIGHT reads near zero for every demo rule because the seeded fires are spread
evenly around the clock. That is the correct answer: the column measures
*disproportionately* nocturnal firing, so a rule that pages as often at 3pm as
at 3am scores 0 on it. See the column notes below.

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
- **SHORT** -- episodes that resolved themselves before anyone could act.
- **SILENCED** -- firing time a human had explicitly silenced.
- **FLAP** -- re-fires on the same series within an hour.
- **COFIRE** -- episodes that started alongside three or more other rules, which
  is what a cause-based alert riding someone else's incident looks like.
- **CONC** -- concentration: how much of the firing time sits on a small
  number of series. Not part of the weighted NOISE sum, but shown because it
  can route a noisy rule to `tune` instead of `retire`; the table would
  otherwise show a verdict it can't justify.
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
  proposed for change.
- A rule whose expression changed inside the window is shown but not given a
  verdict. Its history belongs to the old expression.
- Tolerating a one-sample gap slightly inflates episode duration and slightly
  deflates episode count, which biases against calling a rule flappy.
- If Alertmanager is unreachable, `silenced_rate` falls back to whatever the
  store already holds from an earlier scan -- it does not read as zero unless
  the store has nothing either.

## License

Apache 2.0
