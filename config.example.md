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

The rung's `maxS` is a sizing ceiling, not a claim that every size under it is
fundable. H-CAP-8 separately checks market count and deployable capital. For
example, six markets with $500 and the 25% reserve permit whole-contract S up
to 63: `6 * S * $0.99 <= $375`. The scale ceiling of 100 requires at least
$792 for six markets under that calculation. This clarifies the existing
loader behavior; it does not change the one-market canary or authorize scale.

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
| `inv_soft` | no | 3 | 0.1 — see the canary bound below. |
| `inv_hard` | no | 7 | 0.25. A single 1-contract fill exceeds this, so the market goes REDUCING on the first directional fill. The §16 defaults (3/7/18) can never fire at S=1. |
| `inv_kill` | no | 18 | 0.5 — see the canary bound below. |
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

## Current enforcement and the canary bound

Checked against the source on 2026-09-26: the earlier warning that these three
parameters had no production callers is obsolete.

- **`inv_kill` (F17, global WINDING_DOWN)** — the owner consumes the reconciler's
  `InvKill` effect and calls `requestStop("inv_kill", ...)` in
  `go/cmd/harness/run.go`.
- **`pnl_kill` (§12, H-HALT-5)** — the owner's `evaluatePnL` compares trading
  P&L, including fees and excluding rewards, against the inclusive loss floor
  and requests a global stop.
- **`capital_max` via H-CAP-8** — `loadConfig` calls `risk.CheckFundable` after
  parameter validation, before running, provisioning, or deploying. This
  validates configured fundability; it does not guarantee liquidation or cap
  the account's lifetime loss.

These code paths do not establish release qualification. See the dated
[revival assessment](notes/revival-2026-09-26.md) and the unchanged pilot gates
for the remaining safety and operational evidence.

**What this means for the canary.** pilot-plan §7.9 bounds it by "the first
directional fill latches `WINDING_DOWN`", and as of `lip-2t6` (2026-08-09) it
does. That first-fill bound comes from the rung, independently of the inventory
and P&L thresholds. Fills are fractional to the 0.01-contract quantum, F17
compares with a strict `>`, and `inv_kill` must sit strictly above `inv_hard`,
which must sit strictly above `inv_soft`, which must be positive; so there is no
ordering in which every 0.01 fill breaches `inv_kill`. What bites below it is
`inv_hard`, which takes the market to REDUCING — and REDUCING is market-scoped
and clears at exactly flat, going IDLE and then QUOTING again. If no global
threshold has fired, those inventory transitions can reduce to zero and resume
adding. They do not enforce a first-directional-fill limit.

So the bound is a property of the RUNG rather than a knob. Selecting
`"rung": "canary"` is what turns it on; there is deliberately no config key for
it, because a file that could call itself canary while disabling its principal
exposure bound is a file that lies. After the adoption completes, the first
positive quantity from any of three sources latches a durable global
`WINDING_DOWN`: a create acknowledgement carrying a fill (`canary_ack_fill`), a
newly classified owned fill (`canary_owned_fill`), or a complete position walk
that first shows inventory where the model had none (`canary_position_nonzero`).
History does not count — the trade ids §7.5 saw are frozen into the adoption, so
restarting a canary that has already traded does not re-latch on its own past —
and a stronger cause arriving on the same event keeps the latch, so a fill that
is ALSO a taker fill latches as `portfolio_read` (the fills walk's own stop) and
not as `canary_owned_fill`. Nothing here changes the pilot or any later rung.

The stops that DO still work: the canary's own first-fill latch, a taker fill
(F14), `pos_drift_hard` (F13), a foreign fill, `insufficient_balance` (H-CAP-5),
and the operator's own SIGINT or halt latch.

Those are also now DELIVERED fail-safe, which they were not before
`lip-vxo` (2026-08-08). Until then, any of them could be lost silently: if the
write to `paths.latch` failed, the harness dropped the stop and kept adding, and
whether it ever tried again depended on whether the condition happened to fire a
second time — which a taker fill and a signal do not. A cause that cannot be made
durable is now held and rewritten on every 250 ms tick until it lands; adding
stops immediately and stays stopped for the whole gap, while cancelling,
reducing, polling and monitoring continue; and `WINDING_DOWN` is not published
until the latch is on disk. **If you see a SEV1 `LATCH_WRITE_FAILED`, the harness
has decided to stop and cannot record it — check that `paths.latch`'s directory
is writable.** A SEV2 `LATCH_WRITE_RECOVERED` says the write later succeeded.

This is recorded here rather than papered over because an operator reading this
file would otherwise reasonably believe three kill conditions are protecting the
canary that are not.
# Balance-derived attended sizing (2026-09-26)

Use `"capital_source": "selected_shard_balance"` to derive the frozen run cap
from authenticated primary-subaccount cash on the selected market's authoritative
`exchange_index`. Omit `capital_max` for the balance-derived cap alone; an explicit
`capital_max` becomes an additional entry ceiling. The legacy default remains
`configured` for compatibility with historical files.

The new `sizing` rung permits `S <= 12` and retains the first-owned-fill durable
stop. It supersedes the required S=1/$2 stage for the current task, while preserving
reserve, inventory limits, loss stop, maker-only writes and complete cleanup.
Inherited selected-market commitments start recovery only: their conservative
committed value is added to cash for reducer accounting, with no new adds. This
recovery accounting can exceed a newly lowered entry ceiling; it does not permit
additional risk. Nonselected exposure requires CR-2 and refuses a one-market start.

Periodic scoped cash observations do not raise the frozen capital cap. Missing
or stale observations stop adding. Cash accounting conservatively reserves live,
inflight and unknown orders even where the exchange may already reserve them;
this can undersize. Confirm order/cash behavior in an attended run before relying
on it for continuous operation. Effective parameters are stored in the run row,
with exact funding basis and limits in `FUNDING_LIMITS`/`FUNDING_EXACT` records.
