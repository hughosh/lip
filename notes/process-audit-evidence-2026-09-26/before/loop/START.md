# Running the loop

## Start

```
cd /Users/hugh/kek/lip && caffeinate -is /Users/hugh/kek/.venv/bin/python loop/conductor.py
```

`caffeinate` is not optional: this Mac idle-sleeps and a sleep silently voids
the run (the same F7 the harness spec worries about).

To survive the terminal closing, run it under `nohup` or in a `screen`/`tmux`
session:

```
cd /Users/hugh/kek/lip && nohup caffeinate -is /Users/hugh/kek/.venv/bin/python loop/conductor.py > loop/state/stdout.log 2>&1 &
```

## Stop

```
touch /Users/hugh/kek/lip/loop/state/STOP
```

It stops at the next clean boundary rather than mid-write. To stop *now*, kill
the process — the working tree is recoverable with `git checkout -- .` and every
advance is already its own commit.

## Watch

```
tail -f loop/state/conductor.log          # one line per phase
cat  loop/state/STATE.json                # counters, terminal state
cat  loop/state/LEDGER.md                 # every finding and its disposition
git  log --oneline                        # one commit per advanced unit
bd   list --status open                   # what is left
ls   loop/run/                            # per-iteration artifacts, prompts, diffs
```

## What it does per unit

    gates -> DRIVER (codex, xhigh)   directive + falsification target
          -> IMPLEMENT (claude)      code + tests
          -> gates + scope check     conductor-produced evidence
          -> AUDIT || CHALLENGE      fresh codex threads, in parallel
          -> mutation execution      findings settled by running them
          -> ADJUDICATE (codex)      ADVANCE | REVISE | PARK

Roughly 30–45 minutes per unit, so expect **8–12 units** in a night, not
hundreds. `max` reasoning is spent only on divergence: a red gate, a DRIFT or
NEW-BREAK audit, a surviving mutant, or a second round.

## Why it terminates

Every unit has at most 3 implementation rounds and **exactly one challenge,
which is never renewed**. A repair of a surviving mutant earns no fresh hostile
read. Without that non-renewal the loop provably does not converge: every fix
invites another review, which finds something, forever.

Findings are settled by execution, not argument. A claimed defect must be a
compiling, single-anchor, semantic mutation with a written reachability
argument, and it is *run* against the existing gates:

- **caught** → refuted by evidence, closed for good, mutation kept forever
- **survives** → a real oracle gap, filed as its own bounded unit
- **inadmissible** → not evidence of anything, in either direction

## What it cannot do

- Edit `notes/harness-spec.md`. Hash-pinned; drift halts the run. A believed
  spec defect is recorded as `SPEC_CONFLICT` and parked for you.
- Edit `check.py`, the negative-control runner, either checksum manifest, the
  conductor, or any protocol file. Same pin, same halt.
- Edit `go/core`, `go/feed`, `go/store`, `go/cmd/rig`, or any frozen Python —
  `check.py` fails.
- Weaken a gate. `check.py` refuses `t.Skip`, `testing.Short`, stub panics and
  discarded errors.
- Write outside the directive's `allowed_paths` — the conductor reverts it.
- Commit. Only the conductor commits, and only on ADVANCE with green gates.
- Touch any `*.db`, push, reset, or rewrite history.

## Terminal states

`ALLOWED_QUEUE_EXHAUSTED` (the good one for a night), `DEADLINE_REACHED` (7.5h),
`STOPPED_BY_OPERATOR`, `BLOCKED (control plane mutated)`, `INFRASTRUCTURE_STOP`.

It cannot reach "project complete" in a night, and does not claim to.

## Known-untested

The five model turns have never run end-to-end. Everything else — control-plane
pinning, unit selection, all five mutation classification paths, template
rendering, JSON extraction, gate propagation — was tested before launch. Watch
the first iteration.

## Gotcha: STATE.json pins the control-plane hashes

If you edit `loop/conductor.py` or anything in `loop/protocol/`, the next start
will see control-plane drift and halt (by design). After a deliberate edit:

```
rm loop/state/STATE.json
```

That re-pins on the next start. Do NOT delete it to silence a drift you did not
make — that is the same failure as re-freezing a manifest to pass a gate.

## Checking it before a long run

```
python loop/conductor.py --dry-run                 # startup, gates, unit pick
python loop/conductor.py --max-iterations 1        # one supervised unit
```

## Do not edit the repo while the loop is running

The conductor cannot tell your writes from the implementer's. It sees any
unexpected modification as a scope violation and runs `git checkout --` on it.
An uncommitted edit of yours WILL be reverted mid-run.

If you must change something while it runs: `touch loop/state/STOP`, wait for
the exit, edit, **commit**, then restart. A committed change is safe — the
revert restores files to HEAD, so your change survives it.

## Do not delete STATE.json while a unit is mid-flight

STATE.json carries `pending` — the unit a REVISE is returning to and the repair
it must apply. Deleting it while an implementation is in the working tree
abandons that unit with its work still uncommitted, and the next unit's audit
diff then contains both. Stop, let the unit finish or park, then reset.
