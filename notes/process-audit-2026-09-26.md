# Process audit — 2026-09-26

The default workflow is now aimed at a personal trading client: affected tests
while editing, a normal suite at useful integration points, and a small safety
smoke before an attended candidate. The full mutation catalogue is optional.
The seven-hour style of repeating the entire suite per mutation is a diagnostic
option, not a release requirement. No trading behavior, write key, capital/risk
bound, attendance requirement, or clean-exit safeguard was changed. No live
orders, account probes, conductor execution, commit, or remote sync occurred.

The starting point was the dirty working tree and
[readiness assessment](readiness-2026-09-26.md). Its 300 compiling replacements,
22 selected catches and standalone green quick gate were useful historical
receipts, not a full current qualification. The canonical historical report
still describes 287 outcomes. Counts/status are not interchangeable evidence.
The current catalogue contains 300 entries, three inert controls and 238 unique
named catchers; none was removed in this audit.

## Resulting workflow

- **Ordinary edit:** relevant test/package, diff review, finish the task. No
  compulsory mutation, model handoff, release review, clean tree or whole gate.
  Prose does not need a Go run.
- **Integration checkpoint:** `loop/gates.sh` / `--quick`. Build, vet, format,
  ordinary module tests and cheap integrity/process checks. Race is opt-in for
  concurrency changes. All-300 compilation was removed from this path.
- **Candidate code check:** `--candidate` adds race and 16 safety sentinels;
  select further controls when the actual change needs them. This is a useful
  bundled check, not a universal ritual for every local release or bead.
- **Deep review:** `--audit` runs the retained catalogue with named catchers.
  Direct mutation `--suite` retains broad per-mutant replay for diagnostic
  comparisons. Neither is mandatory promotion evidence.
- **Live use:** the operator still reviews actual blockers and live prerequisites
  in [pilot-plan §6](pilot-plan.md#6-entry-and-exit-gates). Local gates cannot
  provide exchange/account/provider evidence. q01's 4–6h qualification remains
  distinct from ordinary experiments; no q01 or live test is claimed here.

See [the workflow](verification-workflow.md) for commands and failure semantics.
Release checks no longer demand a separate scenario-exchange project or an
exhaustive assertion/detector framework. Credible tests of the applicable
production seams suffice. Actual throttling, recovery and funding defects
remain unresolved safety work; this session did not implement them.

## Rule audit

Links under `before/` are verified copies of the starting files. The starting
manifest and patch preserve the initial uncommitted work. “None” in the evidence
column is intentional: removing needless ceremony does not require a substitute.

| Material old rule / source | Purpose | Observed cost, defect or limitation | Decision | Evidence retained/replacement; remaining risk |
|---|---|---|---|---|
| [AGENTS repeated Beads blocks and force-overwrite shell recipes](process-audit-evidence-2026-09-26/before/AGENTS.md) | Orient agents; avoid prompts | Three tracker sections, mandatory context ceremony, `rm -rf`/force advice at odds with preserving work | Simplify to a concise router; remove force recipes | Existing diffs preserved; no replacement for duplicated prose |
| [Mandatory driver/implementer split](process-audit-evidence-2026-09-26/before/loop/protocol/RULES.md) | Independent judgment | Conflicts with direct implementation in overnight rules; extra model transport/long turns | Remove standing roles and per-edit handoffs | Fresh bounded workers only where useful; active agent owns decisions |
| [Never modify pins/spec/process](process-audit-evidence-2026-09-26/before/loop/protocol/RULES.md) | Prevent changing the judge to excuse a defect | Treats incorrect requirements as immutable; control pins did not stop destructive conductor paths | Allow scoped process/spec clarification | Diff review; actual trading assertions and frozen data unchanged |
| [“Never weaken”, append forever](process-audit-evidence-2026-09-26/before/loop/protocol/RULES.md) | Retain safety detection | Accumulates equivalent, stale and expensive controls without a value test | Remove absolute ratchet | Keep useful controls; explain real safety loss. Unnecessary ceremony needs no replacement |
| [Compiling mutation required to admit every finding](process-audit-evidence-2026-09-26/before/loop/protocol/RULES.md) | Demand a falsifiable hazard | Excludes real traces, missing wiring and contract defects; a survivor is not automatically a product defect | Replace evidentiary rule | Reproducers/observations/contracts suffice; mutations are one useful technique |
| [Caught means refuted forever](process-audit-evidence-2026-09-26/before/loop/protocol/RULES.md) | Bound repeated debate | Proves only a particular test distinguishes a particular edit | Remove permanent closure claim | Named sampled result retained; reopen on changed code/config/evidence |
| [No second challenge after repair](process-audit-evidence-2026-09-26/before/loop/START.md) | Bound cost | A repair can introduce a new reachable defect; finite budgets do not prove correctness | Remove ban, retain sensible stopping | Review affected safety changes; no mandatory adversarial round |
| [All tasks/memory must be Beads only](process-audit-evidence-2026-09-26/before/AGENTS.md) | One source of durable status | Conflicts with required audit documents and ephemeral planning | Simplify | Beads holds tasks/dependencies; receipts and short plans allowed |
| [Wake-up DNS/caffeinate/clean-tree prerequisites](process-audit-evidence-2026-09-26/before/notes/overnight-loop.md) | Support long remote/timed runs | Unrelated to ordinary local edits; clean-tree rule threatens inherited work | Relocate to relevant operational runs | No replacement for routine development; preserve dirty work |
| [Quick gate compiles every mutation](process-audit-evidence-2026-09-26/before/loop/gates.sh) | Catch stale replacement signatures early | Hundreds of builds even for unrelated edits; historical quick estimate 523s | Remove from quick path | Cheap all-anchor audit; changed mutations compile before their test; residual untouched catalogue rot may wait until selected/deep audit |
| [Full catalogue before every bead advancement](process-audit-evidence-2026-09-26/before/notes/pilot-plan.md) | Cumulative regression assurance | Conflicts with milestone-only overnight/readiness rules; historical hours per unit | Remove universal requirement | Ordinary scoped acceptance; candidate useful safety/affected checks. Unselected mutation coverage is not re-proven |
| [Full suite for each mutation + duplicate build](process-audit-evidence-2026-09-26/before/loop/gates.sh), [starting runner changes](process-audit-evidence-2026-09-26/starting-diff.patch) | Establish compilation and named kill | ~301 full suites plus ~601 builds; unrelated slow package repeated | Replace default with named catcher/package and one build | Full pristine baseline in deep audit; same named kills and compile validity. Other collateral failures of the mutant are no longer exhaustively sampled |
| Existing three inert controls | Detect unintended effect / corrupted setup | Cannot certify sandbox integrity alone; each still costs a full suite | Retain only in optional deep/selected audit | Same-sandbox baseline plus full inert run; equivalence arguments remain manual |
| Full compile preflight before any outcomes | Avoid late discovery of broken catalogue | Valuable when the subsequent run costs hours; duplicate cost after focused execution | Move to explicit `--build-only`; normal run checks anchors then builds each mutant | Noncompiling mutations cannot be credited. A late invalid mutation can waste earlier work, now minutes rather than seven hours |
| Retry disagreement until expected result | Avoid load flakes costing a round | Asymmetric: first expected failure accepted, disagreement then pass still green | Remove automatic retry | Every observation retained; disagreement unresolved until investigated, no convenient retry erasure |
| Any nonzero test process + scraped name | Detect assertion failure | Signal/timeout/setup errors can masquerade as a kill; no command deadline | Replace with JSON events and bounded Go commands | Named executed/completed failure, completed packages, no malformed/panic/timeout/skipped result; full logs. No claim of universal determinism |
| Pristine baseline outside mutant sandbox | Ensure failures are mutation-caused | Prior missing root config made sandbox tests unrelatedly red | Run baseline in identical copied layout | Root artifacts copied; named tests must pass baseline and fail mutant |
| Overwritten `loop/gates-out` / canonical report | Retain diagnostics | Reruns destroy evidence; partial/build receipts easily overstated | Unique directories; no automatic canonical overwrite | Mode/IDs/source/tool/command/time/log receipt and INCOMPLETE state |
| Race on every iteration; cached PASS “wrong tree” | Find races, ensure fresh tests | Load-sensitive timeout under overlapping jobs; cache claim contradicts Go behavior | Race for concurrency/candidates; cached tests allowed during edits | Fresh checkpoint runs; actual race detection only covers executed paths |
| No coordination between heavy runners | Throughput | Readiness recorded timeout while three mutation jobs competed; caches historically exceeded 190GB | Shared nonblocking lock, bounded Go parallelism | One heavy job at a time; manual raw Go commands must still respect the rule; no cache purge |
| [check.py frozen/readonly hashes](process-audit-evidence-2026-09-26/before/scripts/check.py) | Preserve measured data and port evidence | Cheap, meaningful integrity check | Retain | Manifest verification; does not prove behavior or new API compatibility |
| [check.py skip/error/trailer lint](process-audit-evidence-2026-09-26/before/scripts/check.py) | Catch obvious test evasion/incomplete ports | Regex is bypassable; “confidence: high” is self-report; cannot detect deleted assertions | Retain cheap lint without treating it as proof | Behavioral tests and ordinary diff review. No new AST framework; false negatives remain |
| [Conductor scope checkout/clean, blanket add, symbol-only closure](process-audit-evidence-2026-09-26/before/loop/conductor.py) | Automate bounded progress | Can discard dirty work; test existence does not prove obligation; second weaker mutation oracle | Retire and disable before side effects | Direct supervised workflow; no replacement automation in this audit |
| [Separate scenario exchange, A1–A14 and all F1–F21 completion before observation](process-audit-evidence-2026-09-26/before/notes/pilot-plan.md) | Prove composed hazards / prevent dead configuration | Conflates implementation architecture with evidence and delays useful read-only feedback | Remove architectural/completion ceremony | Applicable behavioral/composed evidence still required before writer; known safety gaps still block |
| 4–6h q01 and provider proof | Show real long-run freshness, restart and monitoring | Local assessor explicitly cannot attest external provider behavior; old alarm bead closed on attestation | Retain as specific live qualification, not ordinary development gate | Actual external/read-only receipts; stale/missing proof remains missing |
| Owner-only stall / real host-clock jump during q01 | Exercise fault response | No safe external mechanism; current q01 bead already uses injected tests | Use deterministic code scenarios; retain genuine restart/disconnect/alarm drill | No fabricated external provenance; true host sleep remains separately operational |
| First writer / S=1 / $2 / clean exit / CR-1 bounds | Prevent unintended exposure and abandoned inventory | Cannot be supplied by local green gates or closed issues | Retain unchanged live safeguards | Operator and account evidence, actual fills/drain; no authorization advanced here |

## Primary guidance and measured behavior

Current OpenAI guidance recommends revisiting accumulated instructions and using
short task-relevant pointers instead of mandatory document itineraries. That
supports the concise router and removal of model-role ceremony; it is not a
reason to relax trading assertions. [OpenAI, September 2026](https://developers.openai.com/blog/rethinking-skills-and-prompts-for-gpt-6-astra)

Go documents that cached test results are keyed to the test binary and relevant
inputs, and `-count=1` forces execution. Its build cache is designed for concurrent
Go commands; our serialization addresses measured contention, not cache corruption.
[Go command documentation](https://pkg.go.dev/cmd/go#hdr-Test_packages)

Go's race detector observes executed paths and can add substantial runtime and
memory overhead. It is valuable for concurrency changes but cannot be a general
correctness oracle. We use explicit CGO=1 for race mode instead of relying on
platform-specific behavior of the old CGO=0 invocation.
[Go race detector](https://go.dev/doc/articles/race_detector)

## Measured results

| Check | Before | After | Interpretation |
|---|---:|---:|---|
| Ordinary broad checkpoint | 523s historical quick estimate | **78.394s measured** | About 85% shorter; intentionally omits routine race and 300-mutation compilation, now checks the full module. Different historical tree/cache, not a controlled benchmark. |
| Four write-guard controls | 70.065s first audit revision (all packages, duplicate compilation) | **24.151s measured** | Same four real mutations and catcher outcomes, package selection and one build each. The original repeated-suite method was not rerun for comparison. |
| 16 selected safety controls | Not previously timed as this subset | **154.592s measured; 16/16 caught** | Includes both keys, post-only, ambiguity, cancellation/reconciliation, caps, canary and durable stop. |
| Full optional catalogue | Historical reports 105 minutes to roughly 6–7 hours | **~32 minutes modeled, not run** | 300 builds, 297 named catcher runs, four broad suites instead of ~601 builds and 301 suites. Unmeasured mutant-induced waits can make this slower. |
| Gate/lock fixtures | No equivalent suites | 10 gate + 3 isolation tests, under 1s | Invalid arguments, failure propagation, partial/full scope, log preservation, drift, collector exclusion, cross-TMPDIR lock and retired entry. |
| Mutation-tool suite | Preflight/anchor-focused tests | 23 tests, 27.683s | Compiler positive/negative, wrong/missing/ambiguous catcher, malformed/timeout results, survivor exit, no retries, one build per mutant, partial labeling. |

The ordinary module run passed all packages. An independent race fixture passed
with synchronization and failed with a real DATA RACE when synchronization was
removed; the timeout probe exited 124 and was classified INCONCLUSIVE. These are
small tooling experiments, not a full new harness race qualification.

The first default-gate attempt was stopped (exit 143) during fingerprinting: the
new code accidentally included a 7.8GB collector tape. No tests or collector
writes occurred. Fingerprinting now excludes collector tapes, streams relevant
fixtures, and writes INCOMPLETE before hashing. A related environment mismatch
in the new lock location was fixed: it now uses a stable path across TMPDIR
settings, with a cross-process refusal/inheritance test. These failures and fixes
are retained in the audit evidence rather than hidden by the successful rerun.

## Changed files

- Instructions/process: `AGENTS.md`, `CLAUDE.md`, `loop/START.md`,
  `loop/protocol/RULES.md`, and the five historical role templates (marked inactive).
- Tooling: `loop/gates.sh`, `loop/conductor.py` (early refusal only),
  `scripts/harness_negative_control.py`, `scripts/test_harness_negative_control.py`.
- New tooling: `scripts/run_gates.py`, `scripts/verification_support.py`,
  `scripts/test_gates.py`, `scripts/test_verification_support.py`,
  `scripts/safety_mutations.json`.
- Policy: `notes/verification-workflow.md`, `notes/pilot-plan.md`,
  `notes/overnight-loop.md`, a process-only clarification in `notes/harness-spec.md`,
  and an addendum preserving the original readiness handoff body.
- Audit: this report and `notes/process-audit-evidence-2026-09-26/`.
- Beads: superseding process notes on `lip-8hn.1`, `lip-q01`, `lip-dwf`;
  R1 description/acceptance and q01 acceptance updated to remove universal
  catalogue prerequisites. No statuses, live thresholds or original receipt
  histories were erased. Before/after issue snapshots are included.

All starting Go sources, preexisting nonprocess edits and the exact 300-entry
mutation catalogue were preserved. The original readiness body is preserved
beneath its addendum. See [preservation.json](process-audit-evidence-2026-09-26/preservation.json).

## Verification and limits

The machine-readable [audit evidence](process-audit-evidence-2026-09-26/) records
commands/results, source preservation and measured timings. Final measurements
and omissions are recorded in `verification.json` beside the raw logs. The full
300-outcome deep audit was intentionally not run: this task changes process,
not the trading candidate, and targeted controls resolve its decisions.

The safety smoke is deliberately small, not a proof of every hazard. Remaining
client blockers are those in the readiness assessment, especially transport/429,
feed recovery and selected-shard funding. No status or percentage substitutes for
those repairs. The old 287-outcome canonical report and all readiness receipts
remain intact. Existing repair beads were not closed and no next repair began.

Short Sol workers handled two independent audits, a gate edit, a mutation edit,
a fresh process review, and the contained mutation optimization. They returned
and stopped; no standing roles or extended sessions were used. The fresh review
found missing gate self-tests, preflight logs, initial receipts and tool identity;
those were fixed. Per-worker token usage was unavailable, so no usage saving is
claimed. Extra delegation is not part of the new normal edit workflow.
