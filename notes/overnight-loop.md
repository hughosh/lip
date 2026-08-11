# Driver loop — direct implementation, delegated evidence

Binding for any `/loop` session that names this file. Read it fully at every
wake-up; do not act from memory of it.

Project root `/Users/hugh/kek/lip`. Go module root `/Users/hugh/kek/lip/go`.
Branch `harness/lip-6w5-v2-checkpoint`. **No remote. Never push.**

---

## 0. Roles and context

The active driver owns design, ordinary implementation, tracker state and
verification. There is no mandatory decision/implementation baton.

Delegate bounded read-only work that would otherwise flood the driver context:
repository sweeps, raw-log inspection, dependency reconstruction, test
archaeology and independent challenges. Each packet names one question, bounded
paths, expected evidence and a concise return shape. The driver adjudicates the
result; reviewer output never becomes tracker work automatically.

Use independent or cross-model review for safety architecture, spec conflicts,
disputed findings and promotion decisions. The measured value of the courier
cycle was its correction and evidence discipline, not a handoff on every bead.

Keep active context to the current milestone, candidate unit, hard constraints,
decisions and verification status. Durable state belongs in `bd`; narrative
history must not restate mutable tracker status as if it were current.

---

## 1. Preconditions — check every wake-up, in this order

1. **The machine is awake.** `pgrep -x caffeinate` must return a pid. If not:
   `nohup caffeinate -dimsu > /dev/null 2>&1 &`. The iMac idle-sleeps and a gap
   silently corrupts anything time-based.
2. **DNS actually resolves.** Test with a real `getaddrinfo`:
   `/Users/hugh/kek/.venv/bin/python -c "import socket;socket.getaddrinfo('api.openai.com',443)"`
   **Do not use `nslookup`** — it keeps working through the wedge and masks it.
   getaddrinfo wedges system-wide roughly every 2.5h. On failure: wait one cycle
   and retest rather than concluding codex is down.
3. **Nothing is mid-flight.** `pgrep -f gates.sh` and `pgrep -f
   harness_negative_control` must be empty before ANY edit to `go/`. A mid-run
   edit yields spurious DID-NOT-BUILD and costs a ~105-minute round.
4. **The tree is clean** apart from `.beads/interactions.jsonl`. If not, work out
   what the previous cycle left behind before starting a new one.

---

## 2. The cycle — coherent candidate trains, exact promotion gates

The full negative control measured 4h10m at 231 entries. It is a promotion
oracle, not the atom of ordinary implementation.

Per coherent implementation unit:

1. Select one production seam or dependency cone. It may contain several beads
   whose changes and tests share that seam.
2. Record the adjudicated design in `bd`, then claim the unit before writing.
3. Run the baseline build/vet/format/race/check suite, the real-catalogue anchor
   audit, every touched catcher, and all mutations in the affected package,
   dependency and composition closure. A changed shared test helper selects all
   catalogue catchers in that package.
4. A named catcher is a CANDIDATE until the exact named test is observed failing
   against the exact compiling mutation. Editing the mutation, catcher or shared
   helper invalidates that observation.
5. Keep completed work `in_progress` or explicitly `awaiting promotion`; do not
   call it release-green merely because the targeted checks pass.

Run the complete catalogue once on each exact tree promoted to:

- read-only qualification;
- the first live writer; and
- CR-1 continuous operation.

That full run retains every catalogue entry and inert argument. A survivor,
did-not-build, wrong catcher or unexplained NOT INERT is RED. This cadence removes
unchanged replay between milestones; it does not weaken promotion acceptance.

**Any bd command codex invents: check every flag against `bd <cmd> --help`
BEFORE running it inside a `set -e` block.** A half-applied block left the
tracker inconsistent once already.

---

## 3. Work admission and dependencies

- A bead with a recorded **DESIGN** is already decided unless new evidence
  contradicts it. A bead without one is decided by the active driver; it does
  not require a separate model turn by default.
- Prefer the current milestone's causal path over global `bd ready` order.
- Classify every discovery before making it a blocker: reachable production
  defect, evidence gap, or operational prerequisite. A production defect needs
  a compiling mutation and deployed-profile reachability. An evidence gap blocks
  only when the milestone's own acceptance requires that evidence. An
  operational prerequisite names the datum or action it enables.
- Add `A depends on B` only when A's stated acceptance cannot be satisfied
  without B. Eventual CR-1 relevance is not a dependency edge.
- **Never** work a bead marked P4 `RED-TEAM FIRST (do not implement)` or
  `record, do not implement` (`lip-qhi`, `lip-cb0`). Reading them is fine.

---

## 4. Gate integrity — the rules that make the rest worth anything

- A survived mutation is a **finding**, not an obstacle. File it, fix the gate,
  never delete or weaken the mutation to get green.
- Never declare a mutation `inert` without a positive argument written into the
  catalogue. An inert declaration is exactly how a real hole would hide.
- The three inert canaries going NOT INERT means the gate is corrupted. Stop and
  investigate that before anything else.
- After ANY production signature change, re-run the anchor audit AND `--only` the
  affected ids. The audit validates `old` but never `new` (`lip-xl7`), so a
  changed signature can leave a mutation that no longer compiles.
- **Report failures verbatim.** A turn that claims a success it did not achieve
  poisons every downstream turn, and the operator is asleep.

---

## 5. Hard stops — end the loop and leave it for the morning

Do not ask codex to authorise any of these. Stop, write the handoff, exit.

1. **Anything that would run `cmd/harness` against the live Kalshi account.**
   Building, testing and gating are in scope; executing is an operator action.
2. Any `git push`, any remote, any Dolt remote sync.
3. Editing `notes/harness-spec.md`, `go/core`, `go/feed`, `go/store`,
   `go/cmd/rig`, the frozen Python, or any `*.db`. `rig.db` and `lip.db` have
   live collectors writing them.
4. Full gates RED twice on the same bead after one fix attempt.
5. An inert canary reporting NOT INERT.
6. Anything that would weaken, delete or skip a gate to make a run pass.

Also never: `perl -pi` (or any tool with an encoding layer) on a file containing
non-ASCII — it double-encodes silently and every gate is blind to it. Use the
Edit tool. And never run `loop/conductor.py`.

---

## 6. When a delegated reviewer is unavailable

Routine work continues under the active driver. Stop only when the missing
review is itself required: a safety-architecture promotion decision, an
unresolved spec conflict, or a disputed finding whose resolution changes the
operating envelope. Record that precise blocker in `bd`; do not manufacture a
review requirement for ordinary code.

---

## 7. State that survives compaction

Context will fill. Everything that matters goes to disk BEFORE it does.

- Append one line per cycle to `notes/overnight-run.log`: timestamp, bead, what
  codex decided, gate verdict, commit sha. This is the morning's index.
- Keep codex prompts and logs in the scratchpad, not in context. Read only the
  final answer.
- Offload verbose read-only work — log parsing, file sweeps, dependency
  reconstruction and test archaeology — to bounded subagents. Do not compete
  CPU-heavy agents or builds with the full serial mutation run until that load
  interaction is measured.
- Run `/handoff` into a file before compacting, not after.
- Do not put `/compact` in the loop prompt itself: it would fire every cycle and
  throw away context you are about to need. Compact when the statusline is red
  (~70%), and only after the run log is written.

---

## 8. Morning deliverable

At the top of `notes/overnight-run.log`, a summary the operator can read in
thirty seconds:

- beads closed, with commit shas
- beads opened, and what each is waiting on
- every decision codex made, in one line each
- current gate verdict and mutation count
- anything that hit §5, stated plainly
