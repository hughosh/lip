# Working in lip

This repository contains a trading client and collected evidence. Work on the
user's requested scope; a readiness handoff is context, not permission to resume
its next repair or to operate an account.

- Preserve the starting working tree. Inspect `git status` and existing diffs;
  never reset, clean, or revert somebody else's work. Do not touch collector
  databases (`*.db` and journals), credentials, or running collectors.
- Local builds and tests use disposable fixtures. Do not launch the live
  harness, place orders, arm write keys, or start unattended work without the
  user's explicit authorization. `loop/conductor.py` is retired and disabled.
- Keep trading behavior and the live operating envelope separate from process
  edits. The frozen artifacts and `go/core`, `go/feed`, `go/store`, `go/cmd/rig`
  preserve historical evidence; changing them requires a separately scoped
  change and requalification, never regenerating hashes merely to pass.
- Use `bd` for durable work and dependencies (`bd show`, `bd ready`, and command
  `--help` as needed). A closed issue is task status, not release evidence.
  Preserve historical receipts; do not commit, push, or sync remotely unless
  authorized. Short plans and audit documents are allowed; do not duplicate
  the mutable issue tracker in them.
- Keep scratch working files (task lists, notes to self) under `loop/run/`. It is
  gitignored and excluded from `scripts/run_gates.py` `source_fingerprint`; a new
  `.md`, `.go`, `.py` or `.json` file elsewhere changes the fingerprint and makes
  every receipt pinned to it stale. Directories under `notes/` whose name
  contains `-evidence-` are excluded too, so candidate reviews and receipts
  written there leave the pins valid.

Read only the guidance relevant to the task:

- [Verification workflow](notes/verification-workflow.md): ordinary edits,
  affected mutations, receipt interpretation, and release checks.
- [Pilot plan](notes/pilot-plan.md) §6: separate code, real read-only, attended
  canary, and continuous-operation qualification. No local gate grants live
  authority.
- [Loop protocol](loop/protocol/RULES.md): bounded supervised task workers and
  evidence review; it applies when delegating, not as mandatory model roles.
- `notes/harness-spec.md`: relevant behavioral clauses when changing the client.
  Process cadence is governed by the verification workflow; the spec's safety
  invariants remain intact.

This is a personal trading project: optimize for short feedback loops and a
working client. Instructions and checks are reviewable choices. Delete needless
ceremony; it does not need a replacement. When a change removes useful safety
evidence, explain the remaining risk and how it will be detected. Do not lower a
behavioral assertion just to obtain green. Findings may be established by a
reproducer, trace, contract conflict, or mutation; a mutation kill proves that
specific test distinguishes that specific change, not that a defect class is
impossible forever.
