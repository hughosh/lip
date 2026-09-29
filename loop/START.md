# Legacy conductor: disabled

`loop/conductor.py` returns a refusal before creating state, running gates,
calling models, modifying Beads, or touching the working tree. Do not invoke it
for ordinary work. Use [AGENTS.md](../AGENTS.md) and the
[verification workflow](../notes/verification-workflow.md).

The audit found automatic checkout/clean on scope failures, blanket git add,
full catalogue repetition per bead, test-symbol-only closure, a second mutation
runner with weaker failure classification, and long-lived role/budget rules.
Control-plane pins did not prevent those failures. Existing STATE, LEDGER, and
run artifacts are historical evidence; do not delete STATE to re-pin edited rules.

Reactivating automation is a separate requested project. It needs isolated
checkouts, preservation tests, structured receipt consumption, bounded fresh
workers, safe failure/retry behavior, and integration tests of the actual loop.
Removing the refusal alone is insufficient. A supervised `--dry-run` used to
execute gates and mutate state; it is also disabled.
