# Finding ledger

Append-only. Conductor-owned. A finding already disposed of here is closed on sight.

## 2026-08-05T16:12:15Z — lip-2bz scope violation ['loop/state/HEARTBEAT', 'loop/state/STATE.json', 'loop/state/LOCK'] — discarded

## 2026-08-05T16:41:54Z — lip-gmf scope violation ['.claude/settings.json'] — discarded

## 2026-08-05T17:02:21Z — it1 lip-gmf — Per-market q copied from market zero
- **admitted**: SURVIVED every existing gate
- reachability: Under §10.3, six markets quote independently with S=100. A normal one-sided fill in a non-first market can leave market A flat while market B carries -61 contracts and enters REDUCING or SETTLING. This mutation emits B's current ticker, state, and SourceSeq but reports A's q=0, reproducing confident silence about B's inventory. Every driven market in the changed test has the identical +61 q, so its exact-q assertion cannot detect the wrong market index.
- mutation: `harness/risk/monitor.go`

## 2026-08-05T17:02:24Z — it1 lip-gmf — WINDING_DOWN plus SETTLING is untested
- **admitted**: SURVIVED every existing gate
- reachability: With §10.3's six live markets, a one-sided fill can leave q nonzero. H-HALT-3 then permits a concrete deployed sequence: SIGTERM latches global WINDING_DOWN while the process, monitor, and reducers remain alive; at close_time-close_lead (1h), that market transitions REDUCING to SETTLING and retains its reducer until final_lead. The mutation suppresses observation exactly then. The global-state test uses only REDUCING markets, while both new SETTLING transitions use global RUNNING, so neither exercises this reachable conjunction.
- mutation: `harness/risk/monitor.go`

## 2026-08-05T17:44:58Z — lip-4yy ALREADY_SATISFIED, verified by `TestMonitorSamplesInEveryMarketState`

## 2026-08-05T17:48:39Z — lip-52l OPERATOR_ONLY — parked, still owed

## 2026-08-05T17:50:45Z — lip-9r3 OPERATOR_ONLY — parked, still owed

## 2026-08-05T17:53:09Z — lip-afr SPEC_CONFLICT — needs a human

## 2026-08-05T17:55:11Z — lip-428 OPERATOR_ONLY — parked, still owed

## 2026-08-05T17:57:31Z — lip-8a8 SPEC_CONFLICT — needs a human

## 2026-08-05T18:04:33Z — lip-428 OPERATOR_ONLY — parked, still owed

## 2026-08-05T18:36:17Z — it13 lip-ogc — Ceiling parsed quantities causes reducer sign flips
- **admitted**: SURVIVED every existing gate
- reachability: Under §10.3, each of six markets posts 12-contract orders, and H-CO-4 explicitly permits fractional partial fills. The measured size corpus includes the exchange-representable value 0.07. IEEE-754 parsing gives 0.07*100 as 7.000000000000001, so this mutation records a +0.07 YES fill as q=+0.08. The resulting 0.08 NO reducer passes validation against that incorrect local q; if fully filled, the exchange position moves from +0.07 to -0.01. A reducing order has therefore changed q's sign, exactly the H-Q-5a failure mode.
- mutation: `harness/num/qty.go`

## 2026-08-05T18:41:53Z — lip-ogc ALREADY_SATISFIED, verified by `TestEligibleAddingCancelPrecedesFreshRequote`

## 2026-08-05T19:13:02Z — it17 lip-d50 — One-quantum positions become signless
- **admitted**: SURVIVED every existing gate
- reachability: Under §10.3, six markets each rest 12.00-contract orders. The captured exchange data in rig.db contains 1,473 fills of exactly 0.01 contracts, so a one-quantum partial fill is observed rather than hypothetical. From flat, a +0.01 YES fill produces Qty(1), but this mutation makes Sign return 0 while IsFlat correctly remains false. A subsequent ordinary SIGTERM enters WINDING_DOWN; §6.2 can no longer select NO as the reducing side, so no 0.01 reducer is emitted. The process remains alive but cannot drain, violating H-HALT-3 and A4 while leaving inventory unmanaged. The -0.01 path is symmetric.
- mutation: `harness/num/qty.go`

## 2026-08-05T19:26:41Z — it18 lip-xyg — Narrowed sign reverses a deployed full-fill position
- **admitted**: SURVIVED every existing gate
- reachability: Section 10.3 deploys six markets posting S=12.00 contracts per side, and V2-FILL explicitly includes a legal full fill. From flat, one full YES fill therefore produces q=Qty(1200). The mutation narrows the fixed-point quantum count before comparing it; int8(1200) is -80, so Sign reports -1 and section 6.2 selects YES instead of the required NO reducer. The market is already beyond inv_hard=7 and enters REDUCING, yet its supposed reducer is an adding-side order: A4 has no true reducer, A8 is violated, and a fill grows q from +12 to +24 instead of strictly decreasing |q| as H-Q-5a requires. The symmetric -12.00 fill is reversed likewise. The new test's 0 and plus-or-minus-one-quanta inputs, and the older incidental plus-or-minus-seven-quanta inputs, all survive int8 conversion, so they do not expose this reachable fixed-point-width defect.
- mutation: `harness/num/qty.go`

## 2026-08-13 — OPERATOR: control plane re-pinned before the overnight run

- `loop/gates.sh` pin updated `f5af1b9b` -> `1cb15acb`. The worktree copy matches
  `HEAD` exactly; it changed in committed operator work (`91badd2` the q01 fix
  batch, `5bd7679` lip-xl7 compile-every-mutation), not by an agent. Left stale,
  the drift check at conductor.py:526 would have halted iteration 1 with
  BLOCKED (control plane mutated).
- Stale `LOCK` (pid 24569, 2026-08-05) removed; no conductor process exists.
- `.codex-handoff/` and `probe-archive-20260813/` moved to `/Users/hugh/kek/`.
  Untracked-but-unignored, they appeared in `git status --porcelain`, which
  `changed_paths()` reads, and would have been scored a SCOPE VIOLATION on every
  iteration -- discarding each attempt and burning all three rounds per unit.
  `changed_paths()` now returns `[]`.

## 2026-08-14 — OPERATOR: iteration 20 halted on an implement-budget defect

lip-357 round 1 died as `subtype: error_max_turns`, 121 turns against
`--max-turns 120`, 26.2 min, $18.87, returning NO report. Two budget facts, both
near-binding at once:

- 120 turns was insufficient for a 7-file / 9-criterion directive.
- `implement.md` told the implementer to run `loop/gates.sh`, whose full
  negative control is ~105 min, against a 30-min `CLAUDE_TIMEOUT`. The child was
  instructed to run a gate it could not finish inside its own turn.

The conductor was killed rather than STOPped: with STOP set, a successful retry
would have reached the audit/adjudicate codex calls, which return None while
STOP exists, defaulting `adj` to PARK (:768) -- which would have reverted the
work and `bd defer`red lip-357 out of the ready queue. An orphaned implementer
child survived the first kill and ran a further ~11 min unsupervised before
being killed; the tree was re-measured afterwards and is stable.

Codex (driver, xhigh) ruled: RAISE_BUDGETS, not decompose -- decomposition would
charge the ~105-min authoritative gate per seam. Applied:

- `--max-turns` 120 -> 168, `CLAUDE_TIMEOUT` 1800 -> 2725s
  = ceil(1572.325 x 168/120 + 523), 523s being tonight's measured `--quick` cost.
- `implement.md` now requires `loop/gates.sh --quick`. Accepted risk, in codex's
  words: "A defect detectable only by the full negative-control suite may waste
  the conductor's 105-minute run and require repair." The conductor still runs
  the authoritative full gate at :678.
- Control hashes re-pinned for both files.
- The ~2,275-line partial tree is KEPT as an UNTRUSTED repair base; the round
  debit STANDS, so the continuation is round 2 of 3.
- lip-357 annotated in bd with the seven paths any directive must keep in
  allowed_paths, so a fresh driver cannot scope them out and destroy the work.

## 2026-08-14T08:19:26Z — lip-357 SPEC_CONFLICT — needs a human

## 2026-08-14 — OPERATOR: lip-357 SPEC_CONFLICT, and the park that would not stick

The round-2 driver (301KB reasoning) ruled SPEC_CONFLICT on lip-357 and it looks
right: it is the second half of the bead's own title. F5's disagree branch
quarantines the book; A13 then forbids every placement decision from a
quarantined book; A4/§5.2/§12 require a live or in-flight reducer whenever q is
nonzero. No F5 resubscription or bounded quarantine-exit exists in the spec, so
option (a) leaves the disagree hazard, (b) is unspecified, (c) contradicts A13.
This needs an operator spec patch and cannot be resolved unattended.

CONDUCTOR DEFECT FOUND AND FIXED. The three driver-branch parks (SPEC_CONFLICT,
OPERATOR_ONLY, control-plane-undirectable) and the unverifiable
ALREADY_SATISFIED branch called park() WITHOUT clearing `st["pending"]`. Since
`pending` is consulted before next_unit() (:546), a parked unit that is still
pending is re-selected immediately -- `bd defer` cannot hide it because the
pending path never asks bd. Observed live: lip-357 parked at iteration 21 and
was re-picked at iteration 22 for round 3/3. Left alone it would have burned
~14 min per iteration (quick gate + driver) for the rest of the night, parking
an already-parked unit each time. All four sites now clear pending; re-pinned.

lip-357's ~2,275 green lines are in `git stash` (stash@{0}), NOT in the tree,
so they cannot contaminate the next unit's audit diff or commit. The loop was
relaunched on lip-4ak with a 14:10Z deadline.

## 2026-08-14T15:02:36Z — it23 lip-4ak — Retry rejection loses its reason
- **refuted**: CAUGHT by ['TestTheShippedExampleConfigLoads']
- reachability: Under §10.3, six live markets use retry_same_coid_max=3. A concrete H-ORD-2b sequence is attempt 1 receiving HTTP 503 before validation, followed by the same-coid attempt 2 receiving HTTP 400 with error.code insufficient_balance. F12 makes the first response ambiguous; H-CAP-5/F10 requires the second response to trigger SEV1 and global WINDING_DOWN. This compiling mutant still reports REJECTED and status 400, but leaves RejectReason empty because Attempts is 2, so the owner cannot identify F10 and may continue six-market quoting while its capital accounting is wrong. Every current nonempty-reason assertion uses a first-attempt 4xx; the multi-attempt test terminates with 409 instead.
- mutation: `go/harness/rest/write.go`



## 2026-08-14 — OPERATOR: lip-357 SPEC_CONFLICT RESOLVED — the F5 quarantine invariant

The conductor was NOT running for any of this. Tree was clean, control drift
NONE, iteration 23, terminal DEADLINE_REACHED.

THE CONFLICT, RESTATED AND VERIFIED. F5's disagree branch (harness-spec.md:1496)
quarantines the book; A13 (:1977) forbids every placement decision from a
quarantined book; A4 (:1968), section 5.2 (:443-446) and section 12 (:1603) all
require a live reducer whenever q is nonzero. All six citations were checked
against the file, not taken from the driver's summary.

Two findings sharpened the fix beyond what the SPEC_CONFLICT directive said:

1. F2 ALREADY CARRIES THE CLAUSE F5 IS MISSING. F2 (:1493) quarantines "until
   resnapshot + portfolio reconcile complete". F5's disagree branch had no
   release clause at all. Half the gap was an omission, not a design hole.
2. THE GATE ALREADY LIFTS QUARANTINE. harness/wsx/gate.go:419-422 sets
   quarantined = false on a snapshot, commented "The snapshot is what lifts the
   quarantine". The missing half is the RESUBSCRIPTION REQUEST: run.go:980-992
   raises RESNAPSHOT_DEFERRED SEV2 and waits for the socket to cycle, which on a
   one-market pilot may never happen.

Also corrected a stale belief carried in the handoff: harness-spec.md is NOT
pinned in testdata/FROZEN.sha256 (that file pins rig.py, replay.py, score.py,
auth.py and the testdata TSVs). It is pinned ONLY in conductor.py CONTROL_FILES.
scripts/check.py passes unchanged after the patch; all four section 9 checks ok.

WHY section 12's "live" could not be read as "merely uncancelled". The table is
titled "the inversion" and qualifies "live" per row where it needs to
(Disconnect > 60s is "live (cancels over REST)"; Close lead is "live, capped at
|q|"). F5's row was a bare "live" with Process: alive. Section 5.2 sizes the
reducing side "at touch" in every state and section 6.4:601 says the reducing
quote "is requoted to follow the touch under section 6.5 like any other quote".
So section 12 demands a MAINTAINED reducer. That is what made halt-and-escalate
a non-resolution: a halt does not flatten inventory, so it converts an unbounded
silent breach of A4 into a bounded loud one and still never satisfies "live".

THE DECISION. Of three coherent options put to the operator -- reducer priced
from the retained REST book, reducer priced from the last accepted websocket
book plus bounded escalation, or bound the quarantine and formally concede the
section 12 gap -- the operator chose the FIRST.

THE PATCH (5 edits, +47/-3):
- F5 row (:1496): the fetched REST book is retained as the reducer's pricing
  source; in-session resubscription requested immediately.
- H-FAIL-5 (:1539-1542, new para): names H-FAIL-7 and states that the REST book
  is a DIFFERENT book, so this is not an exception to the quarantine rule.
- H-FAIL-7 (:1569-1606, NEW RULE): the substance. Reducing side may be placed,
  resized and requoted under section 6.5 from the retained REST book including
  after the reducer fills or |q| moves; adding side off under A8 and A13; A7,
  A11, A12 evaluated identically. Quarantine ends on snapshot + reconcile as
  F2's does; unlifted after disconnect_halt_s escalates to SEV1.
- section 12 F5 row (:1647): "live (priced from the retained REST book)",
  matching the existing per-row qualification idiom.
- A13 (:2021): records that the F5 reducer's source is the REST book, not the
  quarantined one.

NO NEW CONFIG PARAMETER. The escalation bound reuses disconnect_halt_s, which
F4 already defines for the analogous condition. This matters because the config
hash is pinned into any qualification bundle's Metadata.

A13 IS NOT WEAKENED. The quarantined websocket book remains a source for
nothing. The spec never glossed "placement decision", but every usage attaches
it to an act of checking or sending (H-CO-6, A7, H-Q-10) and H-FAIL-4 says "all
new dispatch stops" -- so passive persistence of a resting order was never one,
which is exactly how run.go:1944-1949 already reads it.

BEAD. lip-357 un-deferred, spec-patch label removed, still P1 so it sits behind
the two P0s in bd ready and cannot be selected ahead of lip-732. Its SCOPE
section, which said "Do not implement from this bead without deciding which",
was rewritten to say the fork is CLOSED -- otherwise a fresh driver would
re-rule SPEC_CONFLICT and burn a round. unit_rounds["lip-357"] reset 3 -> 0:
rounds 1-3 were spent against a contract that could not be satisfied.

STASH KEPT. stash@{0} still holds the ~2,275-line unaudited partial. Under
H-FAIL-7 its H-FAIL-6 REST cross-check work is MORE relevant, not less, since
the cross-check is what fetches the book the reducer is now priced from. It
remains an untrusted repair base and does not satisfy the unit.

Control hashes re-pinned after the commit.
## 2026-08-15T00:43:33Z — it24 lip-732 — Delta gate failure never starts
- **refuted**: CAUGHT by ['TestTheShippedExampleConfigLoads']
- reachability: Section 10.3 runs six live markets at S=12, and after the initial snapshot their books advance through orderbook_delta frames. Use HR-012's concrete book shape: Target Size is 1,000, an accepted snapshot contains 1,300 contracts per side, and our 12-contract adding quotes rest. External cancellations then arrive as accepted current-generation deltas removing 900 per side, leaving 400 and making core.Book.Qualifies() return 0. Portfolio polls can remain healthy and deltas can keep arriving, so neither A13 nor the quiet-feed detector forces a resnapshot. This mutant ignores those deltas in noteBookGate, gateFailSince never opens, and after 30 seconds the owner remains QUOTING with adding orders live and fillable throughout a reward-zero interval—the exact HR-012 failure. Every current gate-failure test starts the failing interval with an orderbook_snapshot, so this deployed transition is untested.
- mutation: `go/cmd/harness/run.go`

## 2026-08-15 — OPERATOR: CLAUDE_TIMEOUT 2725s is undersized, and the retry hid it

One iteration after `CLAUDE_TIMEOUT` was raised 1800 -> 2725s, it bound again.
lip-732 round 1 attempt 1 ran the full 2725s and died at the clock
(`claude TIMEOUT (attempt 1)`, 18:03Z), having WRITTEN the implementation but
never reported it. Nothing in the run.log says the unit nearly died; the loop
appeared to proceed normally.

WHY IT RECOVERED, AND WHY THAT IS NOT REASSURING. `run_claude` (:234,
`attempts: int = 4`) handles `TimeoutExpired` (:261-263) with a bare `continue`
-- no tree reset, and, uniquely among its retry paths, no backoff sleep. Attempt
2 therefore INHERITED attempt 1's files, verified them against all 13 acceptance
criteria, ran `gates.sh --quick` and reported: 82 turns, 21.5 min, $4.68, 11
tests added, 7/7 quick steps GREEN, 287/287 mutations still compiling. The
recovery is real but incidental -- it depends on the absence of a reset that no
comment claims is deliberate. Cost of the save: 2725 + 1290 = 4015s of model
time, ~67 min, for one logical turn.

THE 2725 WAS NEVER VALIDATED AT THIS SCALE. Per its own derivation comment
(:88-93) it is `ceil(1572.325 x 168/120 + 523)`, extrapolated entirely from
lip-357 -- a different unit, and one measured on a turn that itself died on
budget rather than completing. lip-732 is the first unit to test the figure, and
it exceeded it.

THE PROMPT STRUCTURALLY CANNOT WARN A RETRY. Wording for an inherited tree does
exist (:686-691, "Your previous attempt is already in the working tree"), but it
is keyed to a repair ROUND: `repair` is read from `st["pending"]` (:553), which
is written only by an adjudication (:826). The prompt string is built ONCE at
:692, before `run_claude`'s attempt loop, so every attempt within a round
receives a byte-identical prompt. `implement.md` (54 lines) says nothing about
tree state either. Attempt 2 was therefore never told that attempt 1 had written
to the tree -- it had to DEDUCE it. It deduced correctly this time; a retry that
instead started over would have produced duplicated or conflicting edits inside
the unit's own diff.

ATTEMPT 1'S TURN COUNT IS UNRECOVERABLE. The `.raw.json` write is at :264, after
`subprocess.run` returns, so the timeout path writes no artifact. Python's
`TimeoutExpired` does carry `.stdout`/`.stderr`, and the handler discards both.
We consequently cannot say whether attempt 1 was seconds from reporting or far
from it -- which is exactly the number needed to size the replacement value.
Capturing `e.stdout` costs one line and would make the next such event
measurable rather than inferential.

DO NOT RAISE THE CLOCK ALONE. That is the failure mode :88-91 already names for
lip-357: "raising turns alone would only move the wall to the clock", and the
converse now applies. At attempt 2's measured rate (82 turns / 1290s = 15.7
s/turn), `--max-turns 168` binds at ~2643s -- within 3% of the 2725s clock. Which
wall binds first depends on the turn mix, and a writing-heavy attempt is slower
per turn than attempt 2's verifying one, so the clock probably binds first here.
But the two are close enough that raising either alone just relocates the wall.

NOT APPLIED. This entry records the measurement only. No file under `loop/` was
edited while the conductor was running, because its ADVANCE commit is `git add
-A` and an operator edit would ride along inside the unit's commit under the
unit's message.

BEARING ON THE NEXT RUN. iteration 24 ended REVISE, so the next launch resumes
lip-732 round 2/3 against a pending repair -- one `run.go` change plus two test
cases, a smaller turn than round 1, on which 2725s is less likely to bind. The
figure nonetheless remains unvalidated rather than vindicated. When lip-q6r is
eventually reached it is LARGER than lip-732, not comparable to it: 429 handling
across every REST method, ten named acceptance areas, four new
compiling-mutation families, and genuinely new concurrency machinery -- a
bounded transport pool with at least one slot reserved for P1 reducers, capital
reservation kept atomic across workers, and a proven hard upper bound from P1
readiness to network dispatch. If 2725s bound on lip-732 it will bind there.

## 2026-08-15 — OPERATOR: it24 lip-732 REVISEd, and M26's RED is a FALSE POSITIVE

Iteration 24 ended REVISE, not ADVANCE, and the conductor exited
DEADLINE_REACHED at 00:47Z with no commit. Its own ledger entry records only the
refuted challenge finding, so what follows is the part the loop did not record.

THE AUDIT CONFIRMED THE IMPLEMENTER'S OWN CAVEAT, AND THIS IS THE REAL FINDING.
Round 1 reported honestly that acceptance criterion 6 -- oversized aggregate uses
cancel-confirm-place -- was covered structurally (`AllowPlaceThenCancel: false`,
H-Q-9 off) and asserted only at-cap via `targetSize`, with no dedicated oversized
scenario. The audit returned DRIFT on exactly that and made it concrete:
`run.go:2305-2336` resizes only when `quote.Decide` reports a price move;
`quote/requote.go:262-265` reports no move for an order already at the touch,
without considering `OurSize`; and `run.go:2669-2676` only clamps a negative
placement remainder to zero. So section 10.3's 12-contract symmetric order can
enter REDUCING at q=+1 and remain 12 contracts, violating H-Q-5a/A12 and able to
drive q to -11 -- an overshoot straight through flat into the opposite sign.
`gate_failure_test.go:1541-1613` exercises only an aggregate exactly equal to |q|.
A self-reported caveat that a fresh xhigh auditor independently reaches is worth
more than either alone. This finding stands on its own and is the sound reason
to REVISE.

THE CHALLENGE IS SPENT. It raised one finding, "Delta gate failure never starts",
and the mutation was REFUTED -- caught by `TestTheShippedExampleConfigLoads`.
lip-732 is now in `unit_challenged`, and the challenge is never renewed for a
repair (conductor.py:736-737). Rounds 2 and 3 run with audit only.

M26 IS PROVABLY EQUIVALENT, SO THE RED GATE IS A FALSE POSITIVE. The gate failed
one step only -- `negative-control`, 1 of 287 -- with `M26: NOT INERT ->
['TestSEV2IsLimitedPerClassMarketAndReportsSuppression']`. M26 replaces
`q == 0` with `q.Float() == 0.0` in `Qty.IsFlat`. Read the three lines it depends
on: `type Qty int64` (qty.go:37), `const QtyScale = 100` (:20), and
`func (q Qty) Float() float64 { return float64(q) / QtyScale }` (:130). For q = 0
both forms are true. For any q != 0, |float64(q)| >= 1 -- exact below 2^53, and
rounding never produces zero above it -- so dividing by 100 gives magnitude
>= 0.01, nowhere near underflow, and both forms are false. The two expressions
agree on EVERY representable Qty. No test can distinguish them, and therefore no
code change, lip-732's or anyone's, can make M26 observable. The catalogue's
inert claim at :100-106 is not merely plausible; it is provable.

THE TEMPTING FIRST READING WAS TESTED AND REFUTED. Because the only Go delta
since iteration 23's GREEN full gate is lip-732's diff -- 27f61b7 in between
touched `notes/harness-spec.md` and no code -- it is natural to conclude that
lip-732 made the no-op observable, and that `--quick` was blind to it.
Reproduction refuses that. Applying M26 by hand and running the named test
passes. The whole `harness/ping` package passes 12 of 12 mutated runs, once more
mutated under `-race`, and 8 of 8 plus `-count=20` unmutated. Nothing about the
mutation is detectable on this tree.

THE MECHANISM IS THE RUNNER'S ATTRIBUTION.
`scripts/harness_negative_control.py:4402-4404` sets `caught = code != 0` from the
exit status of ONE `go test -count=1 <PKGS>` run across the whole package set,
and takes the catcher names from parsing which tests failed in that output. So
any flaky failure anywhere in PKGS during a mutation's single run marks that
mutation caught -- and for an inert-expected row, `ok = not caught` reports it as
NOT INERT, naming whichever test happened to fail. With 287 mutations each
running the full suite exactly once, a per-run flake probability of a few tenths
of a percent produces about one spurious row per gate. That is precisely the
observed shape: 286 correct, 1 anomalous.

`harness/ping` supplies a plausible flake. `stub_test.go:213-225` (`awaitHealthy`)
waits out the store's REAL retry ladder against a 30-second WALL-CLOCK deadline.
Under the load of a 287x full-suite sweep that is exactly the kind of bound that
breaks; on an idle machine it passes comfortably, which is what was measured
above.

CONSEQUENCES.
- The RED gate is not evidence about lip-732. The DRIFT finding is, and it is
  sufficient on its own to justify REVISE.
- The adjudicator's repair says "Keep M26 classified as inert -- do not relabel
  it or add retries -- and require the full negative-control rerun to show it
  inert and every gate GREEN." Refusing a catalogue relabel is right: relabelling
  would convert a signal into a recorded expectation and lose it. But the
  instruction treats a flake as a defect. A rerun will most likely show M26
  inert, which will read as though the reducer fix cured it and will bury the
  real problem.
- The flake is not specific to M26. Expect roughly one spurious row per full
  gate, on a different mutation each time. Round 2 therefore carries a standing
  chance of another spurious RED costing another six hours.
- This bears directly on the promotion gate. lip-8hn.1's acceptance requires the
  complete catalogue green with "no survivor, did-not-build, wrong catcher or
  unexplained inert result". A runner that emits about one spurious row per run
  cannot certify that criterion. Needs a bead: attribute per-test rather than by
  whole-suite exit code, or re-run a failing mutation once before recording it.
  `scripts/harness_negative_control.py` is NOT in conductor CONTROL_FILES, so
  such a fix needs no control re-pin; the `nc_ratchet` check at conductor.py:715
  only forbids removing mutation IDs, which a runner-logic fix does not do.
## 2026-08-15T22:07:09Z — lip-732 scope violation ['go/harness/wsx/gate.go', 'go/harness/wsx/gate_test.go'] — discarded

## 2026-08-15 — OPERATOR: it26 destroyed work on a scope violation, it27 hit the codex wall

Two iterations, no advance, and the loop is now BLOCKED until codex quota
returns on 2026-08-20 09:20. lip-732's implementation is finished and
gate-verified but has never been audited.

IT26: A GREEN GATE, THEN THE CONDUCTOR DISCARDED THE WORK. Round 3's full gate
returned VERDICT: GREEN, ADVANCE_ELIGIBLE: YES on all seven steps. The attempt
was then discarded at conductor.py:730 as a SCOPE VIOLATION on
go/harness/wsx/gate.go and go/harness/wsx/gate_test.go. The implementer never
touched those files. That iteration's driver narrowed allowed_paths to the three
go/cmd/harness files, and changed_paths() reports the WHOLE tree -- so rounds
1-2's accumulated work read as out of scope and was reverted with
git checkout. This is exactly the destruction mode already recorded for lip-357,
which carries a bd annotation pinning its paths; lip-732 had none.

RECOVERED. 02.patch is written at conductor.py:713, BEFORE the discard at :730,
so it captured the reverted files. Both were re-applied and the tree verified
byte-identical to the tree that passed the GREEN gate (hash of the excluded-path
diff matches 02.patch exactly). lip-732 now carries a bd note naming all five
paths every future directive must keep in allowed_paths.

02.patch IS NOT A COMPLETE BACKUP. It is `git diff HEAD`, which excludes
UNTRACKED files, so it does not contain go/cmd/harness/gate_failure_test.go --
the new test file this unit exists to add. That file survived it26 only because
it happened to be inside the narrowed allowed_paths. A durable backup of all
five files now exists as refs/backup/lip-732-round3 (76ba1f9), built through a
temporary index so neither the working tree nor the real index was touched.
Recover with: git diff HEAD refs/backup/lip-732-round3 | git apply

THE IMPLEMENTER ENDED ITS TURN EARLY, AND THE LOOP SCORED IT AS SUCCESS. it26's
implement turn returned 297 bytes saying it had armed a Monitor and would report
the gate result "as soon as it completes", then ended. The run is a single
non-interactive `claude -p`, so the turn ending ends the run. run_claude accepts
any non-empty result -- only None trips the failure guard -- so the conductor
went on to spend six hours gating a tree no one had reported on. It happened to
be complete. Filed as lip-048.

IT27: CODEX QUOTA EXHAUSTED. The driver's first call returned
"You've hit your usage limit ... try again at Aug 20th, 2026 9:20 AM". The
conductor classified it kind=rate and began the 900/1800/3600s backoff, which
cannot help against a five-day reset: all four attempts would fail, the driver
would return no directive, and the run would end having spent a round. Killed
attended. NOTE for the next session: the log was 7,678 bytes, not the few
hundred that the "judge a quota refusal by log SIZE" heuristic expects, because
it contains the echoed prompt before the error. Read the tail for the explicit
ERROR line instead.

ROUNDS REFUNDED TWICE, 3 -> 2 each time. Neither iteration produced a verdict.
it26's scope branch short-circuits before the audit; it27's driver never ran at
all. Charging a unit for a harness defect and for a vendor quota is not what
MAX_ROUNDS is for, and lip-357's 3 -> 0 reset is the standing precedent.

WHERE THIS LEAVES lip-732. unit_rounds is 2, so the next launch is round 3/3 and
a non-ADVANCE decision will park it. pending still holds the repair, re-issued at
4,605 chars with an operator status block explaining that the instruction was
already carried out and came back GREEN, that the discard was a scope accident
rather than a judgement, and that the turn must not end while waiting on a
background task. The working tree is the gate-verified, never-audited
implementation. Nothing can move until codex returns.

DISK. 34GB free. Each full gate costs ~15GB and the reclaim between runs is
incomplete: 91 -> 78, then 69 -> 54, then 48 -> 33. There is room for about one
more gate before gates.sh hits its floor, so space must be reclaimed before the
next authoritative run.

BEADS FILED THIS SESSION, all P1, all discovered-from lip-732: lip-bca (the
negative control attributes any suite flake to the mutation in flight),
lip-y3q (the catalogue does not catch a gateStopped-restricted reducer check),
lip-048 (the implementer can end its turn waiting on a background task). None is
yet a declared dependency of lip-8hn.1; the first two bear directly on whether a
green catalogue can certify that gate.

================================================================================
2026-08-19 OPERATOR (attended, no conductor, codex out of quota until 08-20)
================================================================================

DISK RECLAIMED. The go build cache had reached 169GB. "go clean -cache" took 13
minutes and returned the volume from 32GB free to 202GB. This is the second
occurrence of the failure mode lip-9vc already documents; the cache is now 8KB.
Disk is no longer a constraint on the next authoritative run.

lip-732 REVIEWED OFFLINE, AND IT DID NOT SURVIVE. Codex being out, the loop's
audit / challenge / adjudicate was reproduced with two independent non-codex
reviewer threads and operator adjudication. The audit returned ADVANCE. The
challenge returned BREAKS. The operator re-verified every load-bearing citation
against the source and sided with the challenge.

THE GREEN GATE WAS TRUE BUT NOT SUFFICIENT. Iteration 26 passed all seven steps
including test-race and the full 287-mutation negative control while a blocking
regression sat in the change. No test covered disconnect-with-inventory or
pre-snapshot evaluation, and both new SETTLING tests inject a clean orders walk
BEFORE the replacement leg is sized. A mutation catalogue can only kill code the
tests already exercise, so a green catalogue is evidence about covered paths and
says nothing about uncovered ones. This sharpens lip-bca's argument that the
oracle cannot on its own certify lip-8hn.1.

D1, BLOCKING, NOW FIXED. The recap predicate carried no "target > 0" guard.
fundedReducer answers 0 on an empty book; SizeR caps Reduce at funded while
SizesFor still reports HasReduce for any q != 0, so "wanted" stays true, target
becomes 0, and the predicate degenerates to atRisk > 0 -- cancelling ANY resting
exit, including a compliant one. ApplyDisconnect sets ResetBooks on an abnormal
close, ResetOnReconnect empties every level, and the owner loop evaluates after
every select arm, so this fired on every abnormal disconnect while holding
inventory, and again at startup with adopted inventory. The recap sits above the
!actionable guard, so the cancel dispatched from a book nothing could be placed
against. The harness deleted its own exit at the moment it went blind: the exact
inversion of I1 and A4. It is a REGRESSION -- HEAD has no cancelConfirmed field
and zero KindCancelConfirmPlace sites, so the branch is entirely new, and
lip-732 is also the first thing to activate the cancel-confirm-place machinery
in quote/queue.go at all.

Fixed by guarding on "target > 0", with the false in-code comment corrected.
Added TestAnAbnormalDisconnectDoesNotCancelTheCompliantExit, whose exit is sized
at exactly the quantized |q| so that ONLY the target collapse can produce a
reducing-side cancel, with the clean close as control. Verified to FAIL on both
abnormal cases with the guard reverted and PASS with it restored; build, vet and
every harness package pass.

D2, CONFIRMED, DELIBERATELY LEFT OPEN for the next round. The replacement leg of
the cancel-confirm-place is deterministically unbuildable in production timing:
ConfirmAbsent sets the latch but does not touch pf.LiveOrders, restingOn never
consults cancelConfirmed, so remainder clamps to 0, build errors, the intents
are dropped and a WRITE_NOT_BUILDABLE SEV2 fires. The side is then absent for up
to position_poll_s. Not fixed here because the repair is a design judgment --
discounting confirmed-absent quantity in restingOn trades against H-FAIL-3 --
and that deserves codex's independent read rather than an operator's. Full
detail and the missing test are on the bead.

INDEPENDENCE CAVEAT, recorded so it is not overstated later: both reviewers were
Claude-family. This is different-model independence, not the cross-vendor
independence codex supplies. It found a blocking defect a single reviewer would
have shipped, but it does not retire the audit codex still owes this unit.

FULL GATE RELAUNCHED on the fixed tree 2026-08-19 21:32:49Z under caffeinate,
output outside the repo. lip-732 must NOT be closed until that gate is green AND
D2 is dispositioned.

RECOVERY REFS, all including the untracked gate_failure_test.go:
  refs/backup/lip-732-round3    76ba1f9  the GREEN iteration-26 tree
  refs/backup/lip-732-pre-d1fix 8aec288  identical, taken immediately pre-fix
  refs/backup/lip-732-d1fix     436d5f6  the same tree plus the D1 fix and test
