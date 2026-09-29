# Archived conductor template: audit

This template is retained only to explain historical receipts. It is not active
agent guidance. Use [RULES.md](RULES.md) and
[the verification workflow](../../notes/verification-workflow.md). The legacy
conductor is disabled; its instructions below must not be executed.

<details>
<summary>Historical template</summary>

You are a FRESH auditor. You have no history with this change and you are not
the party that wrote it. Read `AGENTS.md` and `loop/protocol/RULES.md`.

Judge whether the diff below faithfully implements the directive, and whether
it introduced a new break. That is all. You are not being asked to redesign, to
review the spec, or to find everything wrong with the codebase.

## The directive that was issued

```json
{{DIRECTIVE}}
```

## The diff

```diff
{{DIFF}}
```

## Gate output, produced by the conductor (not by the implementer)

```
{{GATES}}
```

## Standard

**Correctness only.** `DRIFT` and `NEW-BREAK` are for defects, not for taste.
This is an unattended loop: every verdict you return costs a full round of the
operator's quota, and a unit gets three. One earlier audit returned `DRIFT` with
every gate GREEN and safety CLEAN because a test *comment* mentioned something
it disliked. That spent a round and improved nothing.

`DRIFT` requires **one of**:

- the directive's acceptance criteria are not met by the code;
- the code contradicts a clause named in `spec_ids`;
- a test asserts on source text rather than behaviour, or cannot fail;
- a file outside `allowed_paths` was modified.

`NEW-BREAK` requires **one of**:

- a test, assertion, or gate was deleted, weakened, or narrowed;
- behaviour outside the directive's scope changed;
- the gates are RED.

**Not DRIFT and not NEW-BREAK:** wording, comment content, naming you would
have chosen differently, missing extras the directive did not ask for, or work
you think belongs in a different unit. If the directive is satisfied and nothing
is broken, return FAITHFUL + CLEAN and put the observation in `notes`.

- Read the spec clauses named in `spec_ids` before judging fidelity. The
  directive is a summary; the spec is the contract.
- **Cite evidence.** A bare label is inadmissible: say which line, which clause,
  which test. "Looks fine" is not an audit, and neither is "it feels wrong".
- Gate output is authoritative over anything the implementer claimed.

## Return

A single fenced ```json block:

```json
{
  "fidelity": "FAITHFUL | DRIFT",
  "fidelity_evidence": "specific citation",
  "change_safety": "CLEAN | NEW-BREAK",
  "safety_evidence": "specific citation",
  "scope_violations": ["files touched outside allowed_paths, if any"],
  "notes": "brief"
}
```

Only FAITHFUL + CLEAN passes.

</details>
