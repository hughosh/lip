# Brief: what in this plan is not worth building

You reviewed the orchestration design earlier. This is a different question:
**ruthless scope reduction.** Read the files, then answer §5.

## 1. The economics changed, and nothing downstream was re-derived

`notes/harness-spec.md` was written assuming §10.3's opening configuration:
6 markets, S = 100 contracts/side, `capital_max` = $500, against a modelled
~$596/day gross. The verification plan in §17 was sized against that.

**The operator is funding the account with $100, not $500**, until first
returns. §10.3/§16 were rescaled today: S = 12, `inv_soft/hard/kill` = 3/7/18,
`pnl_kill` = −$15. Six markets at 12 contracts.

Nothing else was re-derived. §17's ladder — V1 unit tests, V2 a deterministic
simulated exchange, V3 fourteen invariants, V4 eighteen injected faults, V5
twenty-three mutations, V6 a **72-hour** dry run, V7 live at S=1 — is unchanged
from the $500 design.

**The question that follows: at $100 of capital, with a maximum realistic loss
bounded by the position sizes above, is that verification ladder proportionate?**
Be concrete about which rungs earn their cost at this size and which are
insurance against a loss that cannot happen with $100 at stake.

Do not simply say "safety is good". The operator's time is the scarce resource
and dev time is the thing being cut.

## 2. What exists now

```
harness/num      Qty (exact fixed-point), Money   DONE, tested
harness/cfg      §16 parameter table              written, untested
harness/quote    state.go, queue.go (§6.6, V1.9)  DONE incl. H-QUE-3
                 skew, external, requote, machine MISSING
harness/risk     snapshot, monitor, A5            capital, pnl, A1-A4/A6-A14 MISSING
harness/rest     signed REST client               MISSING
harness/wsx      websocket + ping/pong            MISSING
harness/hstore   harness.db                       MISSING
harness/ping     ntfy alerts                      MISSING
harness/sim      the V2 simulator                 MISSING (0%)
cmd/harness      monitor loop DONE; not wired, refuses to start
```

V5 has 6 of 23 mutations. V3 asserts 1 of 14 invariants.

An unattended two-agent loop (you as driver/auditor, Claude as implementer) is
grinding this at roughly 20-30 minutes per substantive unit, 8-12 units a night.
On current scope that is 7-10 nights to V1-V5, then 72 hours of wall clock for
V6, then V7.

## 3. Specific things to judge

Give a verdict on each. "Keep", "cut", "defer", or "replace with something
cheaper" — and say what breaks if it is cut.

1. **V2, the deterministic simulator.** §17 calls it "the centrepiece": an
   in-process fake REST+websocket driven by a real captured tape, with a fill
   model that "enumerates the legal outcomes" (zero/partial/full/delayed/
   cancel-race/multiple) because queue position is unobservable (V2-FILL,
   `core/rig.go:13-39`). It is the largest single item and everything after it
   depends on it. **Is a full simulated exchange justified at $100, or is there
   a materially cheaper artifact that buys most of the confidence?**
2. **V4's eighteen injected faults.** How many are reachable-and-costly enough
   at this size to be worth a harness?
3. **V5's twenty-three mutations.** Which are load-bearing and which are
   ceremony? M1 and M13/M14 have explicit justification; the rest vary.
4. **V6's 72-hour dry run**, and its entry gate (a missed-heartbeat alarm that
   does not exist).
5. **The N-market design.** Six markets at S=12 versus one or two markets at a
   larger size: which is the better first live configuration, given that
   per-market machinery (selection, per-market state, concentration caps,
   re-selection hysteresis) is a large fraction of the remaining work?
6. **`harness/hstore` and `harness/ping`.** Operational record and phone alerts.
   At $100, how much of §15's eleven tables is justified?
7. Anything else in the spec you judge to be gold-plating relative to $100 at
   risk. Be specific and name the rule IDs.

## 4. Model tiering — help us cut dev time

Turns in the loop, with measured wall clock at their current setting:

| turn | current | measured |
|---|---|---|
| driver (pick unit, write directive from the spec) | `xhigh` | ~3 min |
| implementer | (Claude) | ~5-10 min |
| audit (fidelity + change-safety on a diff, cite evidence) | `xhigh` | ~5 min |
| challenge (author a compiling, reachable mutation) | `max` | ~10-15 min |
| adjudicate (ADVANCE/REVISE/PARK from gate output + audit) | `max` on divergence, else `xhigh` | ~5-15 min |

Available models here: `gpt-5.6-sol`, `gpt-5.6-sol-wm`, `gpt-5.6-terra`,
`gpt-5.6-luna`, `gpt-5.5`, `gpt-5.4`, `gpt-5.4-mini`. Effort levels: `low`,
`medium`, `high`, `xhigh`, `max`, `ultra`.

**Recommend a model+effort per turn.** Which turns genuinely need frontier
reasoning, and which are mechanical enough for a cheap model? Be specific about
the failure you would expect if a turn is under-powered — we have already seen
one real defect from an under-specified rule (a driver closed a unit with a
known bug by citing a real test for a different obligation).

## 5. Return exactly these sections

**PROPORTIONALITY** — the honest answer to §1. Is the §17 ladder right-sized for
$100? What is the actual maximum loss the harness can produce at the current
parameters, and what verification does that magnitude justify?

**CUT** — a ranked list of what to cut or defer, each with what it costs you.
Lead with the largest dev-time saving.

**KEEP** — what must not be cut at any capital level, and why. Be short and
absolute here; this is the list that protects the operator.

**SIMULATOR** — your verdict on V2 specifically, and if you would replace it,
what with.

**MODEL TIERING** — the table for §4, with the expected failure mode of each
downgrade.

**FASTEST PATH TO FIRST REVENUE** — if the goal is a harness safely earning LIP
rewards on $100 as soon as possible, what is the shortest defensible sequence?
Name what you would build, in order, and what you would consciously skip until
after first returns.

Be decisive and quantitative where you can. Where you disagree with the spec,
say so and name the rule.
