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
Episodes   6565
Silences   0

NOISE  CONF  VERDICT   GROUP  RULE           FIRES  SHORT  SILENCED  FLAP  COFIRE  CONC  NIGHT
58     1.0   tune      demo   DemoFlapping   3776   100%   0%        100%  7%      0%    73%
52     1.0   retire    demo   DemoCauseA     583    100%   0%        0%    100%    0%    72%
52     1.0   retire    demo   DemoCauseB     583    100%   0%        0%    100%    0%    72%
52     1.0   retire    demo   DemoCauseC     583    100%   0%        0%    100%    0%    72%
52     1.0   retire    demo   DemoCauseD     583    100%   0%        0%    100%    0%    72%
38     1.0   retire    demo   DemoSpiky      428    100%   0%        0%    7%      0%    73%
9      1.0   automate  demo   DemoSustained  29     0%     0%        0%    7%      0%    76%
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

## What the columns mean

- **NOISE** -- 0 to 100, weighted from the five scored signals below.
- **CONF** -- how much the evidence is worth, separately from how bad it looks. A
  rule that fired three times can score terribly and mean nothing, so below 0.5
  the verdict is always `keep`.
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
- **NIGHT** -- share of fires outside weekday working hours. This is the fifth
  weighted signal behind NOISE (`offhours_rate`).

## Verdicts

- `retire` -- self-resolving and silenced. Nobody acts on it.
- `tune` -- flapping or concentrated on a few series. The threshold or `for:` is
  wrong, not the rule.
- `automate` -- real, frequent and long-running. A runbook candidate.
- `keep` -- fine, or not enough evidence to say otherwise.

## Configuration

`noisefloor init` writes a commented `noisefloor.yaml`. The scoring weights are
in it and are meant to be edited; there is no model, just a weighted sum you can
read.

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
- History is bounded by Prometheus retention, and the report says so when the
  window was clipped.
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
