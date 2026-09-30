# Build, check, try

This is a personal trading client, not a production certification program.
Optimize for a working client and fast feedback. This workflow supersedes the
old loop cadence and release ceremony. It does not authorize account access,
change trading behavior, or relax live write/capital/exit safeguards.

## Daily work

1. Change one coherent thing. Preserve the existing working tree.
2. Run the affected test/package. For example, from `go/`:
   `go test ./harness/rest -run '^TestWriteGuardTruthTable$'`.
3. Run a broader check when the change crosses packages or at a useful checkpoint:
   `loop/gates.sh` (also `--quick`). It builds, vets, checks format and repository
   integrity, and runs the module tests. It does not run mutations or race tests.
4. Review the diff and finish the bead when its actual task is done. Say what
   remains unverified. Task completion is not approval to trade.

Prose needs review and `git diff --check`, not a Go suite. `--static` is a cheap
integrity/process-tool check when useful. No test run is required after every
sentence. Use `--race` for concurrency/ownership/lifecycle changes, not routine
formatting or documentation. Cached Go results are useful during edits;
checkpoint and mutation commands use `-count=1` to get a fresh observation.

Add a regression test for an actual behavioral fix. Add or run a mutation when
it helps establish that a safety test would notice a broken behavior. There is
no requirement for a mutation for every new behavior, a second model for every
change, a compulsory challenger, a clean working tree, or a full gate per bead.
Do not create replacement paperwork for a rule that has no remaining purpose.

## Mutations when useful

From the repo root:

```sh
# A changed guard: baseline and this named catcher in its package.
/Users/hugh/kek/.venv/bin/python scripts/harness_negative_control.py --only M-ES6-FLAG
# Inspect cost/scope without running Go.
/Users/hugh/kek/.venv/bin/python scripts/harness_negative_control.py --plan
# Optional deep audit: every retained mutation, using named catchers.
/Users/hugh/kek/.venv/bin/python scripts/harness_negative_control.py
# Slow diagnostic comparison only; the old repeated-suite approach.
/Users/hugh/kek/.venv/bin/python scripts/harness_negative_control.py --only M-ES6-FLAG --suite
```

Normal execution checks anchors first, runs a pristine baseline in the same
sandbox layout, then compiles each mutant once and runs its exact named catcher
in the catcher's package. Full catalogue mode runs the full pristine suite;
selected mode runs the selected catchers. The three inert controls still run
the full suite. `--build-only` is available for diagnosing catalogue compilation;
it proves no mutation was detected. `--focused` is a compatibility alias for
selected mode. `--suite` is not a release prerequisite.

The old 300 full-suite repetitions are unnecessary: the asserted property is
that a named test distinguishes a specific semantic change. The broad pristine
suite remains useful; unrelated tests repeated against every mutant are not.
Removing duplicate compilation also loses no evidence on unchanged inputs.

Use judgment to select affected mutations: changed behavior, callers, shared
helpers and test catchers matter more than filenames. Broaden selection when
impact is uncertain. All 300 entries remain available; they are not a quota or
a universal release requirement. Repair or retire redundant, equivalent, or
unreachable entries with a short reason. A real lost safety check needs an
explicit decision; a useless check does not need another ritual in its place.

A caught mutant is sampled evidence about its named test, not permanent proof
that a defect class is impossible. A survivor can mean a test gap, equivalent
code, an unreachable path, or a bad experiment. Review it. Current defects can
be established by traces, reproducers, or contract conflicts without a mutant.

## Before an attended trial

A useful code checkpoint is `loop/gates.sh --candidate`: the ordinary suite,
race tests, and the small safety sentinel set in
[safety_mutations.json](../scripts/safety_mutations.json). It checks accidental
writes, maker-only orders, ambiguous exposure, cancel/reconcile/drain, reducer
bounds, and durable stop/canary behavior. Add affected tests/mutations for the
actual changes. Reuse an unchanged result; do not replay it because a bead or
review has a new name. A narrowly irrelevant change does not force this command.

`--candidate` is code evidence, not a complete release decision. Review actual
known blockers for the intended rung, the changed paths and test results. A
survivor or inconclusive result in required evidence needs resolution; an
unrelated research bead or absent grand simulator is not automatically a blocker.
`--audit` adds all retained mutations for an unusually broad safety change,
mutation-runner maintenance, or a deliberate deep review. It is optional; the current measured timing model is roughly 32 minutes, not
a measured full-run duration. Ordinary checkpoint measured 78 seconds; the
16-control safety subset measured 155 seconds on this host/cache.
`--release` is a compatibility alias for `--candidate`; no mode grants live use.

For genuine live qualification, use the current
[attended event stages](attended-stages-2026-09-26.md) with
[pilot-plan.md](pilot-plan.md) §6: operator-run real read-only evidence,
20-minute minimum and 45-minute stop target for q01, externally fired
missed-dead-man proof, then a balance-derived, one-market `S <= 12` attended
first-owned-fill stop with both write keys and verified clean exit. Local code
cannot prove provider alarms, current exchange contracts, account-wide exposure,
or selected-shard spendable funds. Ordinary read-only experiments and local
development do not require repeating q01. No qualification or orders were
performed in the historical process audit.

The eight fault scenarios and applicable safety properties still need credible
behavioral evidence before a live writer. They need not be implemented as a
separate scenario-exchange project, exhaustive production assertion framework,
or an all-detectors completion certificate. Test the actual production seams
and the intended one-market envelope. Known throttling/transport, feed-recovery,
and selected-shard funding defects remain blockers; process cleanup does not
repair them. Continuous participation across market/program turnover retains
the stronger §6.4 and attended-stage criteria, including external SEV1 primary
and backup acknowledgment/escalation.

## Honest, inexpensive evidence

Use one heavy check at a time. Estimate long runs from recent receipts and the
runner's `--plan`; do not start an hours-long check by habit. Gate and mutation
entry points share a nonblocking lock. Do not edit tested inputs mid-run.
Defaults bound Go parallelism and use an explicit offline environment without
inherited credentials; loopback permission may be needed for fake servers.
Use `/Users/hugh/kek/.venv/bin/python`; system Python from the repo root can
accidentally import this repository's `select.py` instead of the standard library.
The `scripts/test_*.py` suites are `unittest` (pytest is not installed):
`/Users/hugh/kek/.venv/bin/python -m unittest scripts/test_operator_stage.py`.

A gate receipt's `source_sha256_*` is the repository fingerprint
(`run_gates.source_fingerprint`). A mutation receipt's `source_sha256_*` hashes
only the sandbox inputs (the `go/` tree plus the runner scripts), so the two never
match; compare each with itself before and after. The candidate's Go source
identity is `operator_stage.source_manifest(...)[1]`. It covers every non-ignored
`.go` file under `go/`, including `_test.go`, so a test-only edit also changes it.
`operator_stage.py` then refuses the current candidate's build receipt even
though the binary is unchanged. Batch test-only Go changes with the next
candidate.

Receipts and full logs have unique paths under `loop/gates-out` by default.
They identify inputs, command, tool, scope, time and outcome. Preserve failures.
Timeout, signal, bad JSON, missing catcher, skipped test, build/setup failure,
and an unrelated package failure are inconclusive, never a mutation kill.
There is no automatic retry to green. Investigate a failed run; a later pass
is additional evidence, not deletion of the failure. Known flaky safety catchers
must be fixed or replaced before relying on them.

A closed Bead, test name, coverage percentage or model endorsement is not a
receipt. Keep only useful durable task/dependency notes; do not mirror status in
multiple process ledgers. Neither `LOCAL_CHECKS: PASS` nor a candidate smoke
means live-qualified. `ADVANCE_ELIGIBLE` and `LIVE_ELIGIBLE` remain NO to prevent
historical automated consumers from making that mistake.
