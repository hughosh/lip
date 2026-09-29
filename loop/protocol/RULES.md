# Supervised work protocol

[AGENTS.md](../../AGENTS.md) governs repository work;
[verification-workflow.md](../../notes/verification-workflow.md) governs evidence.
This replaces the historical unattended two-model protocol. The conductor is
disabled; these instructions do not authorize launching it or the trading client.

The active agent owns synthesis, implementation decisions, review, and honest
handoff. Delegate only independent questions or contained edits that improve
elapsed time, usage, or quality. Keep short dependent steps with the driver.

For this audit and subsequent similarly scoped work:

1. Give each worker one question/change, explicit input paths, owned output
   paths if editing, required deliverable, and a stop condition.
2. Size work for about ten minutes. Request partial findings if it grows;
   split the remainder. Do not allow thirty minutes without a useful result.
3. Use Luna/Sol when sufficient; Astra owns synthesis and decisions. Record
   elapsed time and available usage rather than assuming extra agents are free.
4. Finish an assignment when its deliverable arrives. Use a fresh worker with
   relevant context for a new task; do not keep standing roles for an epic.
5. Run resource-heavy verification serially. Workers may read logs while a gate
   runs, but must not compete with it for test/build resources.
6. Each worker invocation is single-shot. Finish and observe any required check
   before returning; a promise to report a background result later is incomplete
   work. Return the observed outcome and receipt, or the concrete blocker. The
   active agent must not accept a missing outcome as successful verification.

Review findings on their merits. A reproducible current failure, a missing
production connection, or a contract contradiction can be actionable without
inventing a hypothetical mutant. Separate a production defect, a test gap, and
an operational evidence gap. A surviving mutant requires reachability/equivalence
review; a killed mutant demonstrates only its named catcher's sampled coverage.
Reopen an old conclusion when code, configuration, contracts, or evidence change.

Use bounded repair attempts to control cost, not to hide new risk. A repaired
safety-sensitive path gets an affected review and regression check; there is no
absolute ban on another challenge. If progress stalls, retain the edits and
receipts, record the blocker, and hand off. Never discard work to enforce scope.

Process/spec changes are allowed when authorized by the active task. Delete needless ceremony outright. Explain any actual loss of safety evidence
and its remaining risk; do not invent a replacement for an unnecessary rule. A safety-envelope change
needs its own explicit scope; process authorization alone does not permit it.
