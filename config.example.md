# The pilot config file, annotated

`config.example.json` beside this file is a CANARY-rung configuration that loads
through `loadConfig` (`go/cmd/harness/config.go`) as written, once `ticker` is
replaced. It exists because `-provision` cannot be invoked without a config, and
until now there was nothing to copy.

JSON has no comments, so the annotations live here.

## How to use it

```sh
cp config.example.json ~/.lip/canary/config.json
$EDITOR ~/.lip/canary/config.json        # set `ticker`
harness -config ~/.lip/canary/config.json -provision
```

At `s` = 1 the binary starts without a `-rung` flag; every larger size requires
`-rung <name>` matching the file (`main.go`, `checkRung`).

## The two rules the file obeys

**Human units.** Sizes are in CONTRACTS and money is in DOLLARS. The loader
converts (`num.QtyFromFloat`, `num.MoneyFromDollars`). Writing the scaled
integers that `cfg.Params` stores internally would configure the harness a
factor of 100 away from what it reads back as.

**Deviations only.** `loadConfig` overlays the file on `cfg.Default()` — §16's
table — so a key written at its default adds nothing but review surface.
`config.go` states the intent outright: "a diff of the file against an empty one
is the whole config review". Three knobs the operator may expect to see are
therefore deliberately ABSENT here, all at their defaults:
`early_close_lead_h` (4h), `drain_timeout_h` (12h), `heartbeat_s` (3600).

Unknown keys are a HARD ERROR (`DisallowUnknownFields`), so a misspelt knob is
refused rather than silently ignored. There is no `close_time` key: the close is
READ from `/markets/{ticker}`, never configured.

## Every key

| Key | Required | §16 default | Why this value |
|---|---|---|---|
| `ticker` | **yes** | — | The one operator-chosen market. There is no selection algorithm in this binary; `q1select.py` picks it. |
| `rung` | **yes** | — | `canary`. ASSERTED against `s`, never used to derive it — a file that says canary and sizes like the pilot is refused. |
| `s` | no | 12 | 1 contract. The canary's whole question is whether the order path works at all. |
| `s_max` | no | 48 | 1. The per-side-per-market aggregate cap. Left at 48 the canary could rest 48× its base quote. |
| `inv_soft` | no | 3 | 0.1 — see the caveat below. |
| `inv_hard` | no | 7 | 0.25. A single 1-contract fill exceeds this, so the market goes REDUCING on the first directional fill. The §16 defaults (3/7/18) can never fire at S=1. |
| `inv_kill` | no | 18 | 0.5 — see the caveat below. |
| `capital_max` | no | $100 | $2, not $1. H-CAP-8's arithmetic needs the deployable fraction (1 − `capital_reserve` = 0.75) to cover a worst-case reducer fill of 1 × $0.99; $1 leaves $0.75 and does not. |
| `pnl_kill` | no | −$15 | −$1. A LOSS FLOOR, therefore negative; a non-negative value fires immediately. |
| `n_markets` | no | 6 | 1. Also the divisor in H-CAP-2's per-market cap. |
| `paths.db` | **yes, absolute** | — | The five-record store. Its DIRECTORY also receives launchd's stdout/stderr and is the job's working directory (`deploy.go`). |
| `paths.anomaly_log` | **yes, absolute** | — | The append-only anomaly journal. |
| `paths.latch` | **yes, absolute** | — | The durable halt latch (H-HALT-4). |
| `paths.lock` | **yes, absolute** | — | The single-instance lock. |
| `paths.key` | **yes, absolute** | — | The RSA private key signing every request. |
| `paths.env` | **yes, absolute** | — | The file holding `KALSHI_API_KEY_ID`. |

Every path is required and none has a default, deliberately: a binary that
invents the path to its own halt latch can be handed a fresh empty one by being
started from a different directory, which is H-HALT-4 erased by a `cd`. Relative
paths are refused for the same reason — they resolve differently under launchd
than under a shell.

## Why the state paths sit outside the repo

`/Users/hugh/.lip/canary/` rather than anywhere under `/Users/hugh/kek/lip`.
`harness.halt` is not in the repo's `.gitignore`, so inside the worktree it is
an untracked file that `git clean -fd` deletes — H-HALT-4 erased by routine
housekeeping. The `canary/` segment keeps each rung's DB, latch and lock
separate, so raising the ladder does not inherit the previous rung's state.

Credentials point at the existing `~/.kalshi`. They are named explicitly rather
than defaulted so that WHICH ACCOUNT this process can reach is a property of the
config and visible in a diff. Neither `loadConfig` nor `resolvePaths` stats these
files; a wrong path surfaces at startup, not at load.

## Caveat: three of these knobs are not currently enforced

Read this before starting anything. `inv_kill`, `pnl_kill` and `capital_max`
are written here at values chosen from the spec, and as of 2026-08-08 the
harness acts on none of them. Each was verified against the code by a reviewer
instructed to refute it.

- **`inv_kill` (F17, global WINDING_DOWN)** — parsed and bounds-checked;
  nothing anywhere compares live inventory against it. F17's detector is
  specified as the position poll, and that poll uses `cfg.Params` only for
  drift (`q_local` vs `q_exch`), never for `|q|`. Tracked as `lip-lqw`.
- **`pnl_kill` (§12, H-HALT-5)** — same: zero production reads. The loss floor
  does not fire. Tracked as `lip-gp8`.
- **`capital_max` via H-CAP-8** — `risk.CheckFundable` implements the rule and
  is tested, but has no caller on the startup path, so a config that cannot
  fund its reducer starts without complaint. Tracked as `lip-lpf`.

**What this means for the canary.** pilot-plan §7.9 bounds it by "the first
directional fill latches `WINDING_DOWN`". That latch is one of the three above.
What still bites at these values is `inv_hard`, which takes the market to
REDUCING — but REDUCING is market-scoped and it is left at exactly flat, going
IDLE and then QUOTING again. So on a normal maker fill this configuration
reduces to zero and then **resumes adding by itself**, with nothing written to
the halt latch, no SEV1 raised, and no trace surviving a restart.

The stops that DO still work: a taker fill (F14), `pos_drift_hard` (F13), a
foreign fill, `insufficient_balance` (H-CAP-5), and the operator's own SIGINT
or halt latch.

This is recorded here rather than papered over because an operator reading this
file would otherwise reasonably believe three kill conditions are protecting the
canary that are not.
