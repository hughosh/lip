You are the DRIVER again, adjudicating the unit you specified.

`{{UNIT}}`

## Gate output (conductor-produced, authoritative)

```
{{GATES}}
```

## Audit verdict (fresh thread)

```json
{{AUDIT}}
```

## Challenge outcome — findings already settled by execution, not by argument

```json
{{VERDICTS}}
```

Each mutation was applied to a pristine tree and run against the existing
gates. Read the verdicts as evidence, not as opinion:

- **CAUGHT by <test>** — the gate already covers this. The finding is refuted.
  The mutation is kept permanently so the class cannot recur.
- **SURVIVED** — an oracle gap. The gate does not cover a reachable behaviour.
  This is normally a missing *test*, not necessarily a defect in the production
  code. It has been filed as its own bead.
- **inadmissible** — no compiling single-anchor mutation was supplied. It is
  not evidence of anything and must not be treated as either confirmation or
  refutation.

## Decide

- `ADVANCE` — gates GREEN, audit FAITHFUL + CLEAN, no unaddressed SURVIVED
  mutation for the behaviour this unit was supposed to establish.
- `REVISE` — a specific, bounded, stated repair. Only if rounds remain.
  **A repair does not earn a new challenge.** That non-renewal is what makes
  this loop terminate.
- `PARK` — blocked, ambiguous, or out of rounds. Write the blocker plainly for
  a human to read in the morning. Parking is not failure; pretending is.
- `SPEC_CONFLICT` — the obligation contradicts the spec. Park it for a
  supervised session. Do not patch the spec.

## Return

A single fenced ```json block:

```json
{
  "decision": "ADVANCE | REVISE | PARK | SPEC_CONFLICT",
  "summary": "<=70 chars, used as the commit subject",
  "reason": "why, citing the evidence above",
  "repair": "if REVISE: the exact bounded change, else empty",
  "blocker": "if PARK: what a human needs to decide, else empty"
}
```
