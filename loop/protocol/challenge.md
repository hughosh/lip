# Archived conductor template: challenge

This template is retained only to explain historical receipts. It is not active
agent guidance. Use [RULES.md](RULES.md) and
[the verification workflow](../../notes/verification-workflow.md). The legacy
conductor is disabled; its instructions below must not be executed.

<details>
<summary>Historical template</summary>

You are a FRESH challenger. You have not seen the directive, the endorsement,
or any prior argument, and you are not going to be given them. Read `AGENTS.md`
and `loop/protocol/RULES.md`.

**You get exactly one challenge for this unit, and it is not renewable.** If
the repair of a mutant you propose is itself challenged, the loop never
converges — so it will not be. Spend the one shot on the most valuable target.

This is targeted falsification, not open review. `notes/harness-spec.md` §17 V8
is explicit that repeatedly asking a hostile model to "find findings" is not
part of this project's definition of done; designing the mutation that a gate
must catch **is**.

## The change under challenge

```diff
{{DIFF}}
```

## Findings already disposed of — do not raise these again

```
{{LEDGER}}
```

## Your job

Author **at most two** concrete mutations that a competent implementer might
plausibly have left uncaught. A mutation is a text substitution in a named file
that:

1. **compiles** — a mutation caught only by the compiler tests the Go compiler,
   not the gate, and counts as nothing;
2. is **semantic** — it changes behaviour, not formatting or an unused import;
3. has an anchor (`old`) appearing **exactly once** in that file;
4. is **reachable in the deployed configuration of §10.3**, argued in writing.
   "It could in principle" is not an argument. The spec rejected HR-002 on 14.8
   million measured price strings under exactly this standard.

Prefer a mutation that reproduces a failure mode the spec already names
(§2's abandoned inventory, H-Q-5a's sign flip, H-CLOSE-2a's M13, H-TOP-5's
M14) over an invented one.

If you cannot construct a compiling, reachable, semantic mutation, say so and
return an empty list. An empty honest answer is worth more than a mutation that
will be classified inadmissible — it costs the loop a round either way, and a
false one costs it trust in the mechanism.

## Return

A single fenced ```json block:

```json
{
  "findings": [
    {
      "title": "short",
      "file": "harness/quote/skew.go",
      "old": "exact text appearing EXACTLY ONCE",
      "new": "replacement that compiles and changes behaviour",
      "reachability": "why this is reachable under §10.3's deployed config",
      "spec_ids": ["H-Q-5a"],
      "expected_catcher": "the test that SHOULD catch this, if one exists"
    }
  ]
}
```

</details>
