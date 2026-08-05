# Design brief: an unattended two-agent implementation loop

You are being asked to CO-DESIGN a control system, not to write code. Read this
brief, read the two files named in §2, and return the structured answer in §7.

## 1. The situation

A Go implementation of a specified trading harness is ~10% done. The
specification is complete, frozen-by-convention, and 2,233 lines
(`notes/harness-spec.md`). Its §17 is a verification ladder (V1 unit tests → V2
deterministic simulator → V3 invariants → V4 fault injection → V5 mutation
negative-control → V6 72h dry run → V7 live minimum size). The ladder is also
the build order.

The operator wants to sleep and wants the remaining work to proceed unattended,
overnight and beyond, driven by two AI agents that check each other:

- **codex CLI** (`gpt-5.6-sol`, reasoning effort `max`) — intended as the
  intellectual driver: picks work, adjudicates, decides what advances.
- **Claude Code** (Opus) — implementer: writes Go, runs gates, produces evidence.

The operator's words: *"orchestrate each other to progress the program until it
is completed, but also red team / adversarially challenge / investigate
**without getting stuck continually fixing things**."*

That last clause is the hard constraint. Two LLMs reviewing each other generate
an unbounded finding stream; each finding triggers a fix; each fix triggers a
review. The loop must converge.

## 2. Read these before answering

- `notes/harness-spec.md` §17 (the verification plan) and §2 (the defect the
  harness exists to not have). Skim the rest.
- `scripts/check.py` and `scripts/harness_negative_control.py`.

## 3. What already exists, and is unusually strong

This repository has a **machine oracle**, not just tests:

- `scripts/check.py` — refuses stub panics, `t.Skip`, `testing.Short`,
  discarded errors, and `TODO(port)` markers; requires a `// confidence:`
  trailer on every non-test file; SHA256-freezes a list of artifacts. **An agent
  cannot fake progress past it.**
- `scripts/harness_negative_control.py` — mutation testing. It applies a
  semantic mutation to a pristine copy of the tree, requires it to COMPILE, and
  requires a NAMED test to catch it. A mutation that survives is recorded as a
  defect in the *verification*, not in the code.
- The spec's own admissibility standard (§17 V8): *"a finding counts only if it
  is reachable in the deployed configuration, and reachability is argued in
  writing, never assumed, in either direction."* One prior finding (HR-002) was
  rejected on 14.8M measured price strings under exactly this rule.

## 4. The shape I currently propose — attack it

**Control flow owned by a dumb conductor script**, not by either LLM. Each agent
turn is a fresh, small, precisely-scoped invocation whose prompt is rebuilt from
files on disk. State lives in files, never in an LLM's context. Rationale: an
LLM driving an 8-hour loop drifts, fills its context, and cannot be bounded or
resumed; a script never drifts. Codex keeps the *intellectual* lead (it authors
every substantive decision); the script owns only sequencing, budget, retries
and termination.

Per work unit, the conductor runs:

1. gate check (build, vet, `go test -race`, `check.py`, negative control)
2. **codex driver turn** (resumed thread, read-only) → directive: which unit,
   acceptance criteria, and **the mutation whose survival would disprove it**
3. **Claude implementer turn** (headless, restricted tools) → code + tests +
   the mutation wired into the negative control
4. **codex audit turn** (fresh thread, read-only, sees only directive + diff) →
   FIDELITY: FAITHFUL|DRIFT, CHANGE-SAFETY: CLEAN|NEW-BREAK
5. **codex adversarial turn** (fresh thread, hostile framing, no prior context)
   → findings, each of which MUST name a concrete code mutation and a
   reachability argument
6. **codex adjudication** (driver thread) → classify each finding against a
   durable ledger
7. advance, or park after 3 rounds and move to the next unit

**Anti-thrash rules I propose:**

- **R1 admissibility** — a finding without (a) a concrete mutation and (b) a
  reachability argument is inadmissible. Recorded, not worked.
- **R2 mutation-survival test** — an admitted finding is first expressed as a
  mutation and run against the *existing* gates. If a named test already catches
  it, the finding is refuted by evidence and closed; the mutation is kept
  forever. **Only a mutation that SURVIVES becomes work.** This converts an
  unbounded debate into a decidable test and makes every admitted finding
  permanently strengthen the oracle, so the same class cannot be re-litigated.
- **R3 ledger dedup** — every finding and its disposition is appended to a
  durable file; a duplicate is closed on sight without re-argument.
- **R4 bounded rounds** — 3 per unit, then park with a blocker note; parking
  must not block the rest of the queue.
- **R5 ratchet** — gates only ever get added. Never removed, never weakened.
  Green at every unit boundary.
- **R6 no invented work** — units come only from §17's ladder or from a
  surviving mutation. Agents may not add work from imagination.
- **R7 spec-patch quarantine** — the spec permits patching itself when
  implementation finds a rule wrong. Unattended, that is a redesign vector and
  the loop's most dangerous failure mode: **it could edit the spec to make the
  code pass.** So a spec patch must be its own audited unit, never a side effect
  of an implementation turn, and the spec file is SHA-pinned.
- **R8 separated authorship** — the mutation that gates a unit is authored by
  the ADVERSARIAL thread, never by the agent that wrote the code. A gate whose
  failure case was written by the same agent that wrote the gate is barely
  evidence.

## 5. Session rotation (operator requirement, needs a design)

Both CLIs accumulate context. The operator wants: rotate a session out at a
context threshold (~15% of the window remaining), start a fresh one, and
**stagger the two agents so one always holds warm context while the other
reboots** — to save tokens and prevent drift.

Established facts:

- codex: `codex exec resume <uuid|thread-name>`, sessions at
  `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`, index at
  `~/.codex/session_index.jsonl`. Config keys settable per-invocation with `-c`:
  `max_context_window`, `auto_compact_token_limit`, `max_output_tokens`.
  `--json` emits a JSONL event stream; `-o FILE` writes the last message.
- Claude Code: `claude -p --output-format json` returns
  `{result, session_id, total_cost_usd, input_tokens, output_tokens}`;
  `--resume <id>`, `--fork-session`, `--max-turns`, `--max-budget-usd`;
  transcripts at `~/.claude/projects/<slug>/<id>.jsonl`.

Design the rotation: what triggers it, how the handoff is authored and by whom,
what must survive it, how staggering is sequenced, and how a rotation failure is
detected. Note that a handoff authored by the agent being retired is a summary
written by the party with an interest in it looking complete — say whether that
matters and what to do about it.

## 6. Constraints and known hazards

- The tree is **NOT a git repository** and neither is its parent. No rollback
  exists today. (I intend to `git init` with a `.gitignore` for `*.db` /
  `*.jsonl.gz`; two collector processes are actively writing `rig.db` and must
  not be disturbed.)
- `go/core`, `go/feed`, `go/store`, `go/cmd/rig` are "read-only, never edit" by
  convention and are **NOT** covered by any mechanical check. Only Python files
  and testdata are SHA-frozen.
- Concurrent Claude Code instances in one directory are documented as unsafe
  (`.claude.json` races, transcript interleaving).
- macOS DNS wedges system-wide roughly every 2.5 hours on this machine;
  `nslookup` keeps working and masks it. Any network call can fail for minutes.
- The Mac idle-sleeps; a `caffeinate` assertion is currently held.
- Task tracking should use `bd` (beads, v1.0.5, dependency-aware issue tracker)
  for durable epics, kept mutable — tasks deleted or rewritten when findings
  contradict them.

## 7. Return exactly these sections

**VERDICT** — is the §4 shape right? If not, what replaces it, and why.

**CONTROL FLOW** — who calls whom, concretely. Rule on: dumb-conductor vs
codex-as-outer-loop vs mutual-MCP (`codex mcp-server` and `claude mcp serve`
both exist). Justify against the 8-hour unattended requirement specifically.

**ANTI-THRASH** — accept/reject/amend each of R1–R8, with reasons. Add what is
missing. Name the single mechanism you think actually does the work, and name
the one you think is theatre.

**CONVERGENCE** — state the loop's termination conditions formally. Under what
conditions does this loop provably stop, and under what conditions does it
livelock? If it can livelock, say so plainly rather than designing around it.

**ROTATION** — the §5 design.

**FAILURE MODES** — rank the ways this loop produces a confidently wrong result
or silently stops. For each: detection and response.

**MINIMUM VIABLE TONIGHT** — the operator is going to sleep now. What is the
smallest version that is safe to run unattended for ~8 hours, and what must be
deferred to a supervised session? Be concrete about what you would NOT let it do
while nobody is watching.

Be decisive. Where you disagree with me, say so directly and say what you would
do instead.
