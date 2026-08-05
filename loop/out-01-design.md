**VERDICT**

The controller choice is right; the proposed review topology is not. Keep the dumb conductor, but replace the recurring “hostile review → findings → fixes → hostile review” cycle with a finite transactional state machine.

As written, I would not run it unattended yet:

- The negative-control script computes failures but unconditionally returns success at its normal exit: [`good` is counted](/Users/hugh/kek/lip/scripts/harness_negative_control.py:214), then it [`return 0`s](/Users/hugh/kek/lip/scripts/harness_negative_control.py:228). An unknown or lowercase `--only` value also selects zero mutations and succeeds. Its catalog currently contains six mutations, while V8 requires M1–M23.
- `check.py` is strong but not self-authenticating. Its frozen list is limited to nine artifacts ([list](/Users/hugh/kek/lip/scripts/check.py:44)); it does not protect itself, its manifest, the negative-control runner, the spec, or the convention-only Go directories. An agent with workspace write access can weaken the oracle and pass it.
- Step 2 says the driver authors the mutation; R8 says the adversarial thread authors it. Both cannot be true.
- A green test suite cannot detect missing implementation. Completion therefore needs an immutable requirements inventory, not merely green gates and mutable `bd` tasks.
- §17 explicitly says no further broad cross-model review is part of done ([V8](/Users/hugh/kek/lip/notes/harness-spec.md:2113)). Targeted mutation design is appropriate; repeatedly asking a hostile model to “find findings” is not.

The replacement is: finite spec-obligation DAG → bounded implementation attempt → independent machine gates → one bounded implementation-falsification attempt → accept or park. Repairing a surviving mutant does not earn another broad adversarial round.

The repository has already been initialized as Git despite the brief: baseline commit `0ced58e`, current main based at `406f43a`; databases and compressed tapes are ignored and untracked. Use that baseline rather than initializing again.

**CONTROL FLOW**

Rule: dumb conductor as outer loop. Reject Codex-as-outer-loop and reject mutual MCP.

The concrete sequence should be:

1. The conductor acquires a single-instance lock, loads its atomically written state, verifies the last-green commit, protected hashes, absolute deadline, budgets, disk space, and requirement inventory.

2. It selects a ready immutable obligation. `bd` represents and schedules the work, but deleting or rewriting a bead cannot delete the underlying spec obligation.

3. A resumed Codex driver receives the current obligation, relevant spec clauses, dependency state, ledger dispositions, and gate evidence. It returns schema-validated JSON containing:

   - exact scope and spec IDs;
   - allowed paths;
   - acceptance observables;
   - a falsification target, not necessarily the exact mutation;
   - proposed `bd` changes;
   - maximum implementation rounds.

4. One Claude process works in a disposable checkout with no writable access to the live root, Git metadata, credentials, databases, tapes, or protected paths. Claude may run targeted tests for feedback, but its reported results are not evidence.

5. After Claude exits, the conductor independently checks the path allowlist and protected hashes, then runs the gates in a clean environment. An ambiguous Claude failure is never retried in the same checkout: quarantine that checkout, inspect its diff, and either audit it or start a fresh attempt from the same base.

6. A fresh read-only Codex audit sees the directive, exact spec clauses, complete candidate tree in the affected dependency region, diff, and conductor-produced logs. “Directive + diff only” is too narrow. Its result must cite evidence for `FAITHFUL/DRIFT` and `CLEAN/NEW-BREAK`; bare labels are inadmissible.

7. For mandatory M1–M23, use the frozen mutation obligation already authored by the spec. For a genuinely uncovered implementation detail, one fresh Codex challenger may propose at most one semantic mutant or deterministic fault scenario. This is a targeted falsifier, not an open review.

8. The mutant runs in a pristine copy:

   - named behavioral test kills it: classify `COVERED`;
   - mutant survives: classify `ORACLE_GAP` and create one bounded child repair;
   - baseline already exhibits the behavior: classify `BASELINE_DEFECT` and require a direct failing regression;
   - mutation is unreachable or inert: only close on a written positive argument; unattended sessions cannot approve a new `INERT` disposition;
   - spec conflict or uncertain reachability: park for supervision.

9. Codex adjudicates substantive disposition. The conductor only validates the permitted state transition.

10. On acceptance, the conductor stages explicit allowed files, records evidence hashes and ledger disposition, commits the unit, and applies validated `bd` actions. On exhaustion, it parks the obligation and continues only with independent ready work.

Codex remains the intellectual driver. The conductor decides no semantics; it enforces serialization, scope, budgets, evidence formats, retries, and terminal states.

Codex-as-outer-loop is unsuitable for eight hours because context, retry policy, rollback, process supervision, and stopping become model judgments. Mutual MCP is worse: it permits recursive delegation, obscures total budgets, weakens authorship separation, and risks concurrent Claude processes in one directory. Neither gives a reliable crash-resume boundary.

**ANTI-THRASH**

- **R1 — AMEND.** Require an exact spec invariant, deployed reachability trace, observable consequence, and falsifier. The falsifier may be a semantic mutation or deterministic simulator/fault scenario. A current defect cannot always be expressed by “mutating” code that is already defective.

- **R2 — AMEND.** Keep the named-test mutation kill; it is the strongest part of the design. But a killed mutant does not by itself refute the finding. It proves only that this mutant is detected. Closure also requires showing that the baseline does not already exhibit equivalent behavior and that the mutant faithfully represents the reachable claim. A survivor is normally an oracle gap, not automatically a production-code defect.

- **R3 — AMEND.** Use an append-only conductor-owned ledger. Deduplicate only on a stable fingerprint comprising spec IDs, preconditions, observable, and mutation/reproducer hash. Similar prose is not a duplicate; two paths to the same symptom may be materially different. `bd` remains mutable and is not this ledger.

- **R4 — AMEND.** Three semantic implementation rounds is reasonable. Infrastructure retries are separate and bounded. Parking preserves throughput but is not success: any parked required obligation prevents project completion. Repairs of a surviving mutant do not receive a fresh broad challenge.

- **R5 — AMEND.** During unattended work, no gate or test may be deleted or weakened. Long-term, “never remove anything” is unsustainable because obsolete tests and brittle mutation anchors sometimes need replacement. Permit that only under supervision while preserving the covered requirement and killed-mutant set.

- **R6 — AMEND.** No economics, redesign, or imaginative feature work. But §17 is a verification ladder, not a complete implementation inventory. Units must come from a frozen mapping of all applicable `H-*`, `A*`, `V*`, and `M*` obligations. A surviving falsifier may create one bounded verification child, not an indefinitely branching review tree.

- **R7 — STRENGTHEN.** The spec is completely unwritable unattended. An AI-only “audited spec-patch unit” still lets two models agree to weaken the contract. Record `SPEC_CONFLICT`, park affected dependents, and require operator approval in a supervised session.

- **R8 — AMEND.** Separation is useful, but fix the sequence. The driver states the falsification target; Claude implements; the fresh Codex challenger authors the exact novel mutant; Claude may mechanically encode it; a read-only Codex checks faithful encoding. Existing M1–M23 retain their prior independent authorship. A fresh thread is not independent evidence by itself—it is the same model family without conversational history.

Missing rules:

- Protect the conductor, spec, gate programs, mutation inventory, manifest, Git refs, and read-only Go trees outside the agent-writable sandbox.
- Work only in disposable checkouts; the conductor alone commits explicit files.
- Debit attempt and cost budgets before launching a child process, so a conductor crash cannot refund an attempt forever.
- Reject deleted tests, reduced assertions, unknown mutation IDs, empty mutation selections, flaky-pass retries, and requirement obligations with no active bead or evidence.
- Give the conductor its own external missed-heartbeat alarm. The harness spec’s argument about silent death applies equally to the implementation loop.

The mechanism doing the epistemic work is the named-test mutation kill, correctly formulated. The mechanism guaranteeing convergence is the finite, non-renewable challenge budget. The theatre is hostile framing or a fresh thread by itself; without a bounded executable falsifier, it merely produces more prose.

**CONVERGENCE**

Let \(O\) be a frozen finite set of spec obligations. For every \(o \in O\), define finite quotas:

- at most three implementation attempts;
- at most one novel challenge;
- at most \(K\) accepted mutants from that challenge—preferably \(K=1\);
- at most two repair attempts per mutant;
- no challenge descendants for mutant repairs;
- finite infrastructure retries, total model calls, cost, and absolute wall time.

All counters are persisted and decremented before invocation. Every subprocess has a wall timeout and process-group cleanup. Every scheduler pass must either:

1. decrease a counter;
2. mark an obligation `PASS`;
3. mark it `PARKED`; or
4. enter a terminal infrastructure/budget state.

Under those assumptions, the conductor stops in finite time as one of:

- `PROJECT_COMPLETE`;
- `ALLOWED_QUEUE_EXHAUSTED`;
- `BLOCKED`;
- `BUDGET_EXHAUSTED`;
- `DEADLINE_REACHED`;
- `INFRASTRUCTURE_STOP`.

`PROJECT_COMPLETE` requires every mandatory obligation passed, no required parked item, protected hashes intact, V1–V5 green with every M1–M23 accounted for, V6’s full 72-hour evidence, and V7’s live evidence. Use enumerated IDs, not prose counts: §17 currently contains V7.1–V7.11 even though V8 says “all ten rows.”

The eight-hour run therefore cannot terminate as `PROJECT_COMPLETE`. Its successful terminal state is only `ALLOWED_QUEUE_EXHAUSTED` at a green commit boundary.

The loop can livelock plainly if any of these is allowed:

- every survivor receives a fresh adversarial round;
- agents can create unbounded new obligations;
- rewriting a bead resets attempt counts;
- DNS/API failures retry forever;
- invalid structured output retries forever;
- flaky tests are rerun until green;
- a parked dependency is repeatedly selected;
- rotation retries are unbounded;
- a crash occurs before counters are persisted;
- the two agents call each other recursively.

Bounded rounds alone do not prove correctness. They prove that orchestration ends. V6 and V7 remain the empirical checks that the finite model-and-test search did not miss a deployed failure.

**ROTATION**

Only the long-lived Codex driver and Claude implementer sessions rotate. Audit and challenger sessions are deliberately one-shot.

Trigger rotation before a call when:

\[
\text{last observed context} +
\text{estimated next prompt} +
\text{reserved output} +
\text{safety margin}
\ge 0.85W
\]

where \(W\) is the configured context window. Flag rotation at 80%; 85% is the hard watermark. Set Codex auto-compaction above the rotation watermark—around 90%—as an emergency only. Any observed auto-compaction, truncated response, or context-limit error forces retirement.

Use the last turn’s actual context/input occupancy, not the sum of historical input counts. Parse Codex’s JSONL/session usage and Claude’s JSON result/transcript. First calibrate that each counter represents full resumed context. If either metric is ambiguous, use a conservative transcript token estimate plus a fixed fallback such as six resumed turns or one large work unit.

Rotation happens at a green or parked unit boundary whenever possible. At a hard watermark, checkpoint and quarantine the current candidate before rotating.

The authoritative handoff is mechanically authored by the conductor, not the retiring agent. It contains:

- schema version and handoff hash;
- Git base/candidate commit and dirty-tree digest;
- protected-tree and spec hashes;
- current obligation and bead revision/dependencies;
- accepted, parked, and pending obligation IDs;
- directive, allowed paths, and exact next transition;
- gate commands, results, artifact hashes, and ledger offset;
- mutation dispositions;
- remaining attempts, cost, and deadline;
- tool/model/config versions and session IDs;
- no credentials or secret values.

The retiree may append an `uncertainties/claims` record, but it is untrusted testimony. Its incentive to look complete matters; it must not be the sole summary.

Staggering is serial:

1. Pause new work and persist the handoff.
2. Rotate whichever persistent session is nearer its threshold; keep the other session unchanged.
3. Start the replacement without `resume`.
4. Require it to reconstruct, in schema-validated JSON, the exact hashes, current obligation, blockers, budgets, and next action from disk.
5. Mechanically compare the result, then have the still-warm peer check any semantic summary.
6. Atomically switch the active session pointer only after the replacement passes and completes one no-write turn.
7. Rotate the other agent only at the next boundary or after one successful cross-agent handoff.

No two Claude processes run concurrently. “Warm” means its session remains resumable, not that both processes are active.

A rotation fails on timeout/nonzero exit, missing session ID, schema error, state/hash mismatch, omitted blocker, wrong next action, unexpected compaction, or protected-tree change. Keep the old session ID and transcripts until success. Retry a new session at most twice; then enter `BLOCKED_ROTATION` and stop without implementation. If both are simultaneously near the hard limit, rotate Codex first while old Claude remains warm, then Claude while new Codex remains warm.

**FAILURE MODES**

| Rank | Failure | Detection | Response |
|---:|---|---|---|
| 1 | False-green oracle. The current V5 runner exits 0 on survivors, accepts empty `--only`, and covers only six mutations. | Startup canary; exact required-ID comparison; machine-readable per-mutant status; require nonzero on any unexpected outcome. | Do not start a writer until fixed and independently demonstrated failing. |
| 2 | Agent weakens the control plane, spec, manifest, tests, or convention-only Go code. | OS write denial plus pre/post hashes and Git diff from an external trusted list. Detect deleted tests and changed gate files before running them. | Kill the attempt, discard its checkout, record a policy violation; never “audit through” it. |
| 3 | A mutable bead disappears and a requirement silently vanishes. | Frozen requirement-to-bead/evidence matrix; validate acyclic dependencies and complete coverage after every `bd` action. | Reject the task mutation or create a replacement mapping; the obligation remains open. |
| 4 | A mutant is killed for the wrong reason, or a test is tailored to source text rather than behavior. | Require compile success, the named test specifically, baseline green, behavioral assertions, and independent mutant-fidelity audit. | Classify as invalid evidence; park ambiguous reachability rather than fixing repeatedly. |
| 5 | Simulator/tests are green but production wiring never calls the invariant, or the exchange contract changed. | Structural production-call tests, V3 in production build, V4 injection artifacts, then V6/V7. | Never claim completion before live gates; any contract contradiction becomes a supervised spec issue. |
| 6 | Candidate edits contaminate the live tree or disturb `rig.db` collectors. | Disposable checkout, denied database/tape access, explicit staging, collector PID/inode monitoring. | Kill and discard candidate. Never reset, stash, clean, copy, or manipulate the active database tree. |
| 7 | Concurrent Claude sessions or ambiguous partial writes corrupt a candidate. | Exclusive conductor/Claude locks and child-process registry; one attempt per checkout. | Quarantine ambiguous checkout; never retry in place. Stop on evidence of a second Claude writer. |
| 8 | DNS wedge, API outage, sleep, or conductor death silently ends progress. | Real provider connectivity—not `nslookup`; child deadlines; launchd lease; conductor heartbeat/dead-man; wall-vs-monotonic checks. | Bounded jittered infrastructure retries, then persist and safe-stop. Wrap the conductor in `caffeinate`. |
| 9 | Context rotation silently drops an unresolved decision. | Two-phase session-pointer swap, handoff digest, reconstruction test, retained prior session/transcript. | Reject replacement and retain old session; after two failures stop at the last committed boundary. |
| 10 | Disk fills with transcripts/evidence or state is torn on crash. | Free-space floor, capped raw logs, atomic write-and-rename with fsync, startup state validation. | Stop launching agents; preserve last-green commit and emit the external missed-heartbeat/failure signal. |

**MINIMUM VIABLE TONIGHT**

Tonight’s safe version is offline, reversible, and smaller than the proposed loop.

Before sleeping, perform one supervised bootstrap:

1. Use the existing Git baseline; do not run `git init` again. Create a disposable candidate checkout away from the live databases. Never use `git clean`, stash, reset, or broad `git add`.
2. Harden the negative-control gate so it:

   - exits nonzero for every unexpected result;
   - rejects unknown or empty `--only`;
   - emits structured results;
   - verifies the exact expected mutation-ID set;
   - cannot overwrite the canonical full report with a partial run.

3. Pin externally the hashes of the spec, `check.py`, negative-control runner, checksum manifest, frozen artifacts, `go/core`, `go/feed`, `go/store`, and `go/cmd/rig`.
4. Preselect a finite handful of ready V1 pure-code units in `bd`. Give each two implementation attempts and one audit. Do not allow agents to create arbitrary children.
5. Set an absolute cutoff around 7.5 hours, per-call timeouts, cost limits, an exclusive lock, minimum disk space, `caffeinate`, and a conductor dead-man heartbeat.

Then allow only this:

- Codex selects and specifies one V1 unit.
- One Claude implements it in the disposable checkout.
- The conductor independently runs scope checks, build, vet, race tests, `check.py`, and relevant hardened mutations.
- One fresh Codex audits fidelity and change safety.
- The conductor commits or parks, then moves to the next preselected independent unit.

Use fresh sessions per unit tonight. That avoids deploying an untested rotation subsystem during its own first unattended run.

I would not let tonight’s loop:

- edit the spec, gate programs, manifests, tests by deletion/weakening, protected Go directories, Git metadata, `.beads` storage directly, databases, tapes, or collector files;
- declare a novel mutation inert;
- update dependencies or run `go get`;
- access Kalshi credentials or make any external API call from generated code;
- create `live_ok`, run the harness with `--live`, place or cancel orders, or touch real capital;
- install deployment jobs or exercise `pmset sleepnow`, SIGKILL-with-inventory, DNS fault injection, V6, or V7;
- run broad adversarial review or recursively challenge mutation repairs;
- claim the harness complete.

If the control-plane bootstrap cannot be completed before sleep, the only safe unattended run tonight is read-only: build the obligation map, draft directives, and soak existing non-writing gates. Do not give Claude write access under the current false-green V5 runner.