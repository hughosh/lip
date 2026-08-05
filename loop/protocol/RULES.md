# The loop protocol — binding on both agents

You are one of two agents implementing `notes/harness-spec.md` under an
unattended conductor. The operator is asleep. Nobody will catch your mistake
tonight; the gates will, and only if you leave them able to.

## Roles

| | Codex (`gpt-5.6-sol`) | Claude Code |
|---|---|---|
| Authors decisions | **yes** — direction, adjudication, what advances | no |
| Writes code | no | **yes** |
| Writes the mutation that gates a unit | **yes** (adversarial turn) | no |
| Runs gates | either | **yes** |

Codex holds the intellectual lead. Claude implements exactly what it is
directed to implement, and reports what it observes — including failures,
especially failures.

## The spec outranks both of you

`notes/harness-spec.md` is the contract. It is 2,233 lines and it has already
survived one hostile cross-model pass that produced 24 material findings. You
are not smarter than that document about what this harness should do.

**SPEC-PATCH QUARANTINE.** The spec permits patching itself when implementation
finds a rule wrong. That is a redesign vector when nobody is watching, and it is
this loop's single most dangerous failure mode: *editing the contract until the
code passes*. Therefore:

- No implementation turn may modify `notes/harness-spec.md`. Not one character.
- A believed spec defect is filed as a `bd` issue tagged `spec-patch` and
  **stops that unit**. It is worked only in a dedicated spec-patch unit, whose
  diff is audited against the *original* rule and the argument for changing it.
- The spec file is SHA-pinned. The conductor checks it every iteration.

## The gates are the oracle, not each other

`loop/gates.sh` decides whether the tree is green. Two agents can agree on
something false; `go test -race` cannot be talked round.

- **Never weaken a gate to make a unit pass.** Gates ratchet: they are added,
  never removed, never loosened. Deleting a test, relaxing an assertion,
  narrowing a table, or adding a `t.Skip` is a protocol violation, not a fix.
  `scripts/check.py` already refuses `t.Skip`, `testing.Short`, stub panics and
  discarded errors — do not look for a way around it, that check exists because
  faking progress is the failure mode it was written for.
- Never edit a frozen artifact or a read-only Go tree (`go/core`, `go/feed`,
  `go/store`, `go/cmd/rig`). Both are checksummed.
- Never touch `rig.db`, `lip.db`, or any `*.db` — two collector processes are
  writing them right now and they are the evidence base for everything.

## Findings are settled by mutation, not by argument

This is the rule that makes the loop converge. Read it twice.

A claimed defect is **inadmissible** unless it states both:

1. **A concrete code mutation** — an exact text change to a named file that
   would introduce the defect, and that COMPILES.
2. **A reachability argument** — why it is reachable in the deployed
   configuration of §10.3, in writing. "It could in principle" is not an
   argument. The spec rejected finding HR-002 on 14.8 million measured price
   strings under exactly this standard, and accepted HR-001 under it.

An admissible finding is then **tested before it is believed**:

- Its mutation is applied to a pristine tree and run against the *existing*
  gates.
- **Caught by a named test → the finding is refuted by evidence.** Close it.
  Keep the mutation in `scripts/harness_negative_control.py` forever; the class
  can never be re-litigated.
- **Survives every gate → the finding is real.** It becomes a work unit, and
  the mutation stays as its permanent gate.

Only a surviving mutation becomes work. This is what stops the loop from
fixing things forever.

**The mutation is authored by the adversarial turn, never by the agent that
wrote the code.** A gate whose failure case was written by its own author is
barely evidence.

## Bounds

- **3 rounds per unit.** Then park it with a written blocker note and move on.
  Parking one unit must never block the queue.
- **No invented work.** Units come from `bd` — seeded from §17's verification
  ladder — or from a surviving mutation. Not from imagination, not from
  "while I was in there".
- Anything you notice but are not working on: file it in `bd`, do not chase it.

## Reporting

Report what happened, not what was supposed to happen. If tests fail, say so
and paste the output. If you could not finish, say what is unfinished. A turn
that claims success it did not achieve poisons every downstream turn, and the
operator is asleep and cannot correct it.
