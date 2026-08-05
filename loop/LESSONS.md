# What the first loop run actually taught us

Written 2026-08-05 after ~3 hours and 19 iterations, before revising the plan.

## The numbers, unflattered

```
19 iterations   6 advanced   6 parked   2 revised
11 restarts     = 11 manual interventions
 5 commits by the loop
18 commits by the operator, most of them fixing the loop
 6 findings raised   6 admitted   0 refuted   0 inadmissible
```

**The loop cost more than it produced.** That is the headline and it should not
be softened. Roughly 15 defects were found in the scaffolding; every restart was
caused by one, and none by the two agents disagreeing badly.

It is also true that it found four things a careful human reading would not
have, listed under "what it was worth" below. Both facts are real.

## Root cause of the 11 restarts

The conductor was written in about an hour and launched unattended **without a
single end-to-end test of the model turns.** Everything mechanical was tested —
control pinning, unit selection, all five mutation classification paths, gate
propagation, JSON extraction — and all of it worked. Every failure was in the
part that had never been run.

The defects, in the order they bit:

| # | Defect | Class |
|---|---|---|
| 1 | scope check flagged the conductor's own state files, discarded every attempt, then `git checkout` reverted its own counters | didn't test with a real diff |
| 2 | negative control pinned flat as a control file, so M1-M23 could never be added — the one thing V5 requires | pinned the thing that must grow |
| 3 | context occupancy summed `cache_read` across a run's iterations, reporting **124%** of a 1M window | never ran it once |
| 4 | `r'(\\w+)'` — escaped backslash in a raw string, so every caught mutation logged as "(unnamed)" | never ran it once |
| 5 | `--permission-mode acceptEdits` denies Bash headless; **11 of 11** implementer Bash calls denied, $1.00 burned learning it | assumed a flag's semantics |
| 6 | driver could emit directives naming hash-pinned files — unimplementable, and if implemented would halt the run | two rules written without cross-checking |
| 7 | bd's `.beads/interactions.jsonl` polluted every audit diff | didn't look at what git reports |
| 8 | run dirs keyed by iteration number, so a restart overwrote the previous run's artifacts | state reset not considered |
| 9 | `REVISE` re-queried `bd ready` and picked a *different* unit, discarding the repair and leaking work into the next diff | didn't trace the decision paths |
| 10 | `SKIP` meant both "already satisfied" and "operator-only", and **both closed the bead** | overloaded a decision |
| 11 | `ALREADY_SATISFIED` closed a unit whose audit had just found a real starvation bug, citing a real test for a *different* obligation | cheapest path attracts traffic |
| 12 | queue had no notion of build-vs-polish: 4 of 5 advances were micro-coverage on already-built packages while three packages did not exist | optimised the visible metric |

## The four lessons that generalise

**L1 — Test the expensive path once before running it unattended.** Every
mechanical component was tested and every one worked. Everything that failed was
in the five model turns, which had never executed. One supervised iteration
before launch would have caught 1, 3, 4, 5, 7 immediately.

**L2 — The cheapest decision on the board attracts all the traffic.** Twice.
`SKIP` and then `ALREADY_SATISFIED` both became the path of least resistance to
closing a unit, and the second one closed an obligation with a *known, named*
defect. The fix was never a better prompt; it was making the cheap path
structurally unavailable (refused after round 1).

**L3 — A guard that verifies the cheap half of a claim is not a guard.**
`ALREADY_SATISFIED` required naming a Go test symbol, and the conductor grepped
for it. That proves the symbol exists. It cannot prove it covers the right
obligation, and the driver named a real test for a neighbouring rule. Verifying
the checkable half gave false confidence in the unchecked half.

**L4 — Mutation-survival only bounds a finding stream where coverage already
exists.** 6 raised, 6 admitted, **0 refuted**. The mechanism was designed to
*refute* findings the gates already catch, and so converge. At ~10%
implementation it refuted nothing and became an unbounded generator of "this
isn't tested yet". The coverage gate helped, but has an inverted edge: it
enables the challenge precisely where coverage is already high, generating more
work in the best-covered package.

## What it was worth

Four findings that justify the exercise even at this cost, none of which a
careful read would have produced:

1. **The V5 gate could not fail.** `harness_negative_control.py` counted
   survivors and returned 0 unconditionally, so a mutation surviving — by
   definition a hole in the verification — read as a passing gate to every
   automated caller. Found by codex reading the source.
2. **H-QUE-3 starvation.** Anti-starvation promotion converted a base-P1 reducer
   to effective P0, losing it the reserve, so the mechanism starved the one
   class the spec forbids starving. Found by the audit turn, with the existing
   test shown unable to reach the state (it dequeued at 1s, promotion is at 30s).
3. **§5.2's diagram contradicted H-CLOSE-2** — it put `SETTLING` 60s before
   close instead of an hour, collapsing the window A4 requires a reducer in.
   **M13 reintroduced through the picture rather than the prose**, in the area
   the original red-team called its highest-value finding.
4. **`ParseQty("0.07abc")` returns 0.07, no error.** `Sscanf("%g")` does not
   require consuming its input, so a malformed wire count silently becomes a
   position — in the parser H-CO-4a makes the foundation of every sign and zero
   comparison.

Note the shape: **one from an independent model reading source, two from the
adversarial turns, one from the audit.** None from the implementer, and none
from running tests. The value was in the *reading*, not the automation.

## What that implies for the next version

- **Do not run an unattended loop to write ordinary code.** It was slower and
  more expensive than direct implementation, by a wide margin.
- **Do keep the cross-model reading.** Its four findings were the whole return,
  and three of them were latent defects in work already believed finished.
- The cheap, high-yield shape is therefore: implement directly, and spend codex
  on *review at decision points* — which is what the pre-existing
  `codex-red-team-workflow` memory already said before any of this was built.
