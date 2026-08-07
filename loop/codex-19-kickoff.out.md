## VERDICT

**PARK at the accepted v3 boundary. Resume only if primary evidence establishes the 2027 branch and a redesigned commercial probe clears its pre-registered gate; stop if the September branch stands or a clean probe hard-fails.**

## Q1 PARK POINT

**Yes—the v3 boundary is the right park boundary, although the current state needs one final acceptance round before it truly reaches that boundary.**

It is coherent because:

- `lip-6w5` now has a complete component-level contract, with its remaining integration obligations recorded on `lip-eyq` and `lip-3af`.
- [`cmd/harness/main.go`](/Users/hugh/kek/lip/go/cmd/harness/main.go:1) still structurally exits 2. The parked artifact cannot accidentally trade.
- Advancing only through `lip-eyq` would add code without producing a more usable or safer resting artifact. Advancing to `lip-3af` would create a runnable binary before the reduced milestone gates and canary, making the park point less safe.
- The local commits preserve the implementation history. The remaining durability exposure is single-disk loss, not uncommitted-work loss.

The 124/124 result is valuable, but worth less than the headline suggests. It means 124 specific mutations produced their declared outcomes on that tree—not that 124 independent properties exhaust the failure space. The five rotten anchors and the initially inert `M-P-REFUSESILENT` prove both ways a catalogue can overstate coverage. The current runner’s exact-one-anchor check materially improves the evidence, but it remains enumerated evidence, not completeness or integration evidence.

## Q2 MINIMUM PATH

**Minimum: one further driver round, entirely on `lip-6w5`. No bead beyond it.**

1. **Driver round 1 — bounded v3 acceptance**

   Verify the 18-item directive, the nine declared behaviour changes, and the downstream notes. Independently rerun the ordinary gates and the full negative control. No new catalogue work is presently justified; expected count remains **124**, with each existing mutation still anchored exactly once and caught by its named test.

   This is a fidelity/change-safety acceptance round, not another open-ended bug hunt.

2. **If it passes**

   I recommend—not direct—that the operator:

   - record the acceptance verdict, commit `7e8002f`, gate result, and 124/124 count on `lip-6w5`;
   - close `lip-6w5`;
   - leave `lip-eyq` blocked while the programme is parked;
   - correct `lip-q01` and `lip-jy4` to the pilot-plan scope instead of the stale 72-hour/M1–M23 wording;
   - record the unresolved commercial branch and probe decision.

3. **Archive**

   Preserve off-device: the two checkpoint commits/branch, the Beads database, `notes/`, `scripts/`, the mutation catalogue/result, and an operator-managed consistent backup of the evidence databases. Agents should not touch the live `*.db` files.

Cost: **one driver round**, plus operator administration and the browser check. If acceptance fails, `lip-6w5` remains the unit and gets at most two repair/adjudication rounds under the three-round cap.

## Q3 INVERSION RULING

**Round 18 is directionally right about buying commercial evidence before funding more build work, but it over-claims the probe. Invert budget allocation, not the causal role of the harness.**

The end-date check is genuinely decisive and belongs first. The resolving observable is exact, rendered text on a Kalshi-owned help-centre or regulatory-notice page explicitly amending the global programme sunset, captured with URL and retrieval time. Per-market API `end_date` values are corroboration, not resolution: the spec defines those as individual reward-period ends, not the programme’s authorization sunset.

The proposed probe answers a narrower question:

> “For this market, period, size, and observed presence, did the realised gross payout agree with the visible-book scoring model?”

It does not establish that thin-book competition generally survives, that the `$282/$596` capacity curve holds, or that the strategy is net profitable.

Three quantities must be pre-registered separately:

- `P0`: static pre-entry forecast from the selection snapshot.
- `Pint`: prediction integrated from the actual book and actual resting presence.
- `A`: attributable realised payout.

Then:

- `A / Pint` tests scoring/payout calibration.
- `Pint / P0` measures forecast erosion from competition, gating, book movement, and downtime—still not competition alone.
- `A + trading P&L` is the commercial result, but one market has essentially no power on adverse selection.

This matters because an integrated `revmodel` prediction already absorbs visible competitive response. Competition can destroy absolute revenue while `A / Pint` remains near 1. Round 18 conflated those two questions.

The proposed `<60%` rule has no in-tree justification and discards the existing pre-registered asymmetric rule in [`phase2-spec.md`](/Users/hugh/kek/lip/notes/phase2-spec.md:213):

- `A / Pint ≥ 0.7`: scoring model holds; this authorizes the Go canary, not scale.
- `0.3–0.7`: inconclusive; repeat in a different market family.
- `<0.3`: model/instrument hard failure; stop further capital and diagnose.

A one-market, one-period observation is sufficient only for a clean catastrophic fail—especially zero or `<0.3` when `Pint` comfortably exceeds the $1 floor and presence/attribution checks pass. It is not sufficient for a positive general conclusion or a `<60%` strategy-wide stop.

Finally, `probebot.py` is not safe to run as Round 18 described. Its halt path cancels without managing inventory, and it contains an automatic taker flatten using `post_only=False` ([probebot.py](/Users/hugh/kek/lip/probebot.py:536)). With `S=12`, one full directional fill exceeds its 10-contract flatten threshold. Attendance does not neutralize an automatic crossing path. The probe needs a deliberately bounded operating protocol or repair before use.

Therefore: complete the one-round v3 park first; then buy the date fact; under the 2027 branch, buy a redesigned probe result before `lip-eyq`.

## Q4 LADDER

| Branch | Programme action |
|---|---|
| **September branch** | Accept and park v3; stop the Go programme. Leave incomplete beads open/deferred with the reason recorded. Preserve the rig, models, spec, and evidence. A Python run would be optional research, not a reason to resume the build. |
| **2027 branch + clean hard miss** | Remain parked and stop. No `lip-eyq`, `lip-bw0`, or `lip-3af` work. |
| **2027 branch + 0.3–0.7 or merely `<60%`** | Inconclusive. Run a second independent period in another market family; do not resume Go work yet. |
| **2027 branch + confirmed result** | Resume the reduced pilot ladder below. |

For the confirmed branch:

1. `lip-eyq`: startup resolution of every outstanding reservation, binding/abandonment, terminal `Result.Err` handling, and drain → cancel writer → close. Use the full money-path review.
2. `lip-bw0`: durable halt/startup adoption, single-instance operation, supervision and deployment.
3. Before wiring acceptance, finish the canary-critical off-chain work: `lip-dgg`, `lip-b1r`, and `lip-0qj`.
4. `lip-3af`: wire the pilot binary, including immediate `BindOrder` on `rest.CreateResult`. Keep structural live guards.
5. `lip-jy4`: use the reduced milestone semantics. Preserve the existing catalogue; the current floor remains 124. Do not recreate M1–M23 as fresh work.
6. `lip-q01`: 4–6 hours read-only with forced disconnect, resnapshot, restart, and schedule jump—not 72 hours.
7. `lip-dwf`: `S=1` canary, first directional fill latching `WINDING_DOWN`.
8. `lip-gp8`: exact P&L kill before repeated `S=12` cycles. Then one-market pilot; second market only after a clean reduction/restart cycle and payout.

The driver ladder should no longer allocate 3–5 rounds automatically to every bead. For money-path beads: directive/implementation, one combined fidelity-adversarial round with concrete compiling mutations, and an adjudication round only when something survives. Plumbing beads can use a single bounded acceptance round.

Unconditional work in every branch is only: v3 acceptance, tracker correction, primary end-date verification, decision recording, and off-device preservation.

## WHAT I VERIFIED, AND COULD NOT

I read in-tree:

- The complete harness contract, including §17 and the pilot override.
- The round 15–18 prompts and outputs, plus the relevant round-14 directive.
- `reeval-verdict.md`, `economics.md`, `pilot-plan.md`, and the earlier phase-2 probe specification/prediction.
- The current v3 implementation surfaces, named tests, all 18 new mutation entries, and the recorded 124/124 catalogue.
- Commit `7e8002f`, its parent checkpoint, the exit-2 harness stub, and the absence of a configured Git remote.

The substantive source tree has no code diff; the current round prompt is the sole untracked path.

I could not verify:

- Either programme end date, today’s competition, current API population, or current payouts—there is no network access.
- The live Beads database: read-only Dolt initialization was denied. I used the operator-supplied session counts/statuses as authoritative.
- The gates independently: Go could not create its temporary build directory in this read-only sandbox. The green result is therefore a committed/operator-reported artifact, not a test run I personally observed.

## WHERE ROUNDS 15–18 WERE WRONG

- **Round 15:** its `CHANGE-SAFETY: CLEAN / ADVANCE: YES` verdict was wrong in hindsight. Round 16 demonstrated two material reachable failures. It also treated cancel-with-backlog limbo as a later shutdown-order concern; F9 showed queued callers needed terminal results now.
- **Round 16:** F1 and F2 were high-value and substantially correct. F7(a) was factually wrong because §13.3 explicitly mandates five-minute SEV1 deduplication. It also overstated the in-run `seen` loss as persisting across restart. Its findings were not initially settled through the protocol’s compiling-mutation test, although subsequent failing-first tests and catalogue ratchets repaired that evidence gap.
- **Round 17:** I found no reason to reverse its substantive F1/F2 adjudication or the v3 repairs. Its tracker statement “19 open, 0 closed” was wrong, and its estimate used stale `lip-q01`/`lip-jy4` scope. It also called the H-ORD-9 tri-state treatment “no rule change” too confidently; it is an interpretation of literal text and needed the durable note that was eventually recorded.
- **Round 18:** it stated the unconfirmed 2027 date too strongly, treated one probe as decisive for competition, invented the `<60%` cutoff, and overlooked that the proposed Python instrument automatically crosses and can halt with unmanaged inventory. Its dependency inversion is therefore only partially valid. Its remote-backup warning remains valid, but the uncommitted-work half has been discharged.

## MOBILE RELAY

**VERDICT: PARK at v3 after one acceptance round. Resume only if primary evidence proves the 2027 branch and a redesigned probe clears its gate.**

- v3 is the right park boundary: coherent component contract, downstream obligations recorded, and the binary still structurally refuses to run.
- One more driver round should audit v3 and independently rerun gates at 124 mutations; then I recommend the operator close `lip-6w5`, leave `lip-eyq` blocked, and back up Git/Beads off-device.
- Check the global sunset on a rendered Kalshi-owned page. Per-market API end dates do not settle it.
- Round 18’s one-market `<60%` rule is not decisive. Separate static forecast, integrated prediction, actual payout, and trading P&L; one market supports only a catastrophic fail-fast.
- If September stands or a clean probe hard-fails, stop. If the 2027 branch and probe both hold, resume `eyq → bw0 → 3af`, then reduced gates and the `S=1` canary.