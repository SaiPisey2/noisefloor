# Security

## Reporting

Open a [security advisory](https://github.com/SaiPisey2/noisefloor/security/advisories/new).
Please do not open a public issue for a vulnerability.

## What noisefloor touches

- It reads a Prometheus HTTP API, and Alertmanager when one is configured.
  Every request is a GET.
- `serve` is read-only and binds to localhost by default. `collect` accepts
  Alertmanager webhooks, with a bounded request body.
- With `-apply`, `remediate` writes to a branch of one GitHub repository and
  opens a pull request. Without it, nothing leaves the machine.
- The GitHub token is read from `$GITHUB_TOKEN` only. It is never accepted
  as a flag, because a flag value is visible in `ps` output and shell
  history, and it is redacted out of any error text before that text is
  printed.

## What is treated as untrusted

Alert names and rule groups come from whoever writes the alerting rules;
a silence's author and comment come from whoever created the silence. They
are rendered with visible escapes before reaching a terminal, and escaped
again before reaching a pull request body, which is read by whoever
reviews it.

`remediate` replaces a rule file wholesale. It refuses the commit unless
the remote file still matches the bytes the edit was computed from.
