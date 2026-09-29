#!/usr/bin/env python3
"""The unattended two-agent implementation loop.

Control flow is owned by THIS SCRIPT, not by either model. An LLM driving an
eight-hour loop accumulates context, drifts from its protocol, and cannot be
bounded, resumed or audited; a script does not drift. Codex keeps the
intellectual lead -- it authors every substantive decision -- and this file owns
only sequencing, budgets, retries, scope enforcement and termination.

It is the argument harness-spec.md §2 makes for I2: the monitor works because it
is STRUCTURALLY unable to be stopped by the thing it watches.

Per unit:

    gates -> DRIVER (codex, resumed)     directive + falsification TARGET
          -> IMPLEMENT (claude)          code + tests
          -> gates + scope check         conductor-produced evidence
          -> AUDIT || CHALLENGE          fresh codex threads, in parallel
          -> mutation execution          findings settled by running them
          -> ADJUDICATE (codex)          ADVANCE | REVISE | PARK
          -> commit / park / retry

CONVERGENCE. Every unit has finite quotas: at most MAX_ROUNDS implementation
attempts, and **exactly one non-renewable challenge**. A repair of a surviving
mutant earns no new challenge. Without that non-renewal the loop provably does
not terminate -- every fix invites a fresh hostile read, which finds something,
which invites another fix. The bounded challenge budget is the convergence
guarantee; the mutation execution is what makes each finding decidable.

Stop with:  touch loop/state/STOP
"""
from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

LIP = Path(__file__).resolve().parent.parent
LOOP = LIP / "loop"
STATE_DIR = LOOP / "state"
RUN_DIR = LOOP / "run"
PROTO = LOOP / "protocol"
STOP_FILE = STATE_DIR / "STOP"
LOCK_FILE = STATE_DIR / "LOCK"
STATE_FILE = STATE_DIR / "STATE.json"
LEDGER = STATE_DIR / "LEDGER.md"
HEARTBEAT = STATE_DIR / "HEARTBEAT"

CODEX = "/Applications/ChatGPT.app/Contents/Resources/codex"
CLAUDE = shutil.which("claude") or "claude"
PY = "/Users/hugh/kek/.venv/bin/python"
MODEL = "gpt-5.6-sol"

# The control plane. If any of these changes, an agent has modified the thing
# that judges it, and every judgement after that point is worthless. Checked
# every iteration. `check.py` already pins the frozen Python and the read-only
# Go trees; this pins the judges themselves, which it cannot.
CONTROL_FILES = [
    "notes/harness-spec.md",
    "scripts/check.py",
    # The implementer runs under bypassPermissions; if it could also edit the
    # permission rules it runs under, the sandbox would be self-modifying.
    ".claude/settings.json",
    "testdata/FROZEN.sha256",
    "testdata/READONLY.sha256",
    "loop/conductor.py",
    "loop/gates.sh",
    "loop/protocol/RULES.md",
    "loop/protocol/driver.md",
    "loop/protocol/implement.md",
    "loop/protocol/audit.md",
    "loop/protocol/challenge.md",
    "loop/protocol/adjudicate.md",
]

MAX_ROUNDS = 3               # implementation attempts per unit, then park
MAX_ITERATIONS = 200         # backstop, not a plan
DEADLINE_HOURS = 7.5         # absolute; the operator expects to wake to a stop
TURN_TIMEOUT = 45 * 60
# Measured 2026-08-14 on lip-357: the implementer exhausted 120 turns at 26.2
# min and returned NOTHING (subtype error_max_turns), losing ~2,275 lines of
# compiling work to a budget rather than to a defect. Both limits were near
# binding at once, so raising turns alone would only move the wall to the clock.
# 2725 = ceil(1572.325s observed x 168/120 turns + 523s), where 523s is the
# measured cost of the one `gates.sh --quick` the implementer is now asked for.
#
# 2026-08-15: 2725 BOUND on the very next unit. lip-732 round 1 attempt 1 ran
# the full 2725s and died at the clock having WRITTEN the implementation and
# never reported it. It survived only because the TimeoutExpired path below
# `continue`s without resetting the tree, so attempt 2 inherited the files and
# spent a further 1290s (82 turns) verifying and gating them -- 4015s of model
# time for ONE logical turn, and attempt 1 was still short of reporting. 2725
# was extrapolated from lip-357 and had never been measured at this scale.
#
# 5400 gives ~35% headroom over that observed 4015s. --max-turns rises with it
# (168 -> 220) because this file's own history is that raising one budget alone
# just relocates the wall to the other: at attempt 2's measured 15.7 s/turn a
# 5400s clock admits ~344 turns, so leaving 168 in place would make the turn
# budget the new binding wall for anything verification-heavy. 220 keeps the
# clock binding while still bounding a runaway.
CLAUDE_TIMEOUT = 5400
MIN_FREE_GB = 5

# Session policy. Fresh-per-unit is the limiting case of rotation and needs no
# untested machinery, so it is the default for an unattended first run. Set
# ROTATE=True to use long-lived sessions with the measured context threshold
# below -- but not on a night when the rotation path has never executed.
ROTATE = False
ROTATE_AT_FRACTION = 0.15    # measured: cost/quota per turn is linear in context
                             # carried (~$3.09/Mtok), so rotating early is
                             # strictly cheaper than rotating late.

RATE_LIMIT_PAT = re.compile(
    r"rate.?limit|429|quota|usage limit|overloaded|too many requests|"
    r"resource_exhausted|capacity", re.I)
NET_FAIL_PAT = re.compile(
    r"dns|getaddrinfo|temporary failure|connection reset|timed? out|"
    r"network is unreachable|no route to host|tls handshake", re.I)


def now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def log(msg: str) -> None:
    line = f"[{now()}] {msg}"
    print(line, flush=True)
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    with (STATE_DIR / "conductor.log").open("a") as fh:
        fh.write(line + "\n")
    HEARTBEAT.write_text(now() + "\n")


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def control_hashes() -> dict:
    return {f: sha(LIP / f) for f in CONTROL_FILES if (LIP / f).exists()}


NC_PATH = LIP / "scripts" / "harness_negative_control.py"


def nc_mutation_ids() -> set[str]:
    """The mutation ids currently in the negative-control catalogue."""
    return set(re.findall(r'^\s*\("(M[0-9]+[a-z]?)",',
                          NC_PATH.read_text(), re.M))


def nc_ratchet_ok(before: set[str]) -> tuple[bool, str]:
    """The negative control is a RATCHET, not a freeze.

    §17 V5 requires all of M1-M23 to exist, so this file MUST grow as the
    harness does -- pinning it flat (as an earlier version of this conductor
    did) makes the loop structurally unable to do the job it was built for.

    But it is also the gate that catches everything else, so it may only ever
    gain coverage. Additions are allowed; removing or renaming a mutation is
    the same failure as deleting a test, and is refused.
    """
    now_ids = nc_mutation_ids()
    lost = before - now_ids
    if lost:
        return False, f"mutation(s) REMOVED from the catalogue: {sorted(lost)}"
    return True, (f"+{len(now_ids - before)} mutation(s)"
                  if now_ids - before else "unchanged")


def load_state() -> dict:
    if STATE_FILE.exists():
        return json.loads(STATE_FILE.read_text())
    return {"iteration": 0, "advanced": 0, "parked": 0, "revised": 0,
            "claude_session": None, "claude_ctx": 0, "claude_window": 1_000_000,
            "codex_thread": None, "unit_rounds": {}, "unit_challenged": [],
            "pending": {},
            "raised": 0, "refuted": 0, "admitted": 0, "inadmissible": 0,
            "control": None, "started": now(), "terminal": None}


def save_state(st: dict) -> None:
    tmp = STATE_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(st, indent=2) + "\n")
    tmp.replace(STATE_FILE)          # atomic: a crash mid-write must not tear it


def ledger(entry: str) -> None:
    with LEDGER.open("a") as fh:
        fh.write(entry.rstrip() + "\n\n")


def gates(quick: bool = False) -> tuple[bool, str]:
    cmd = [str(LOOP / "gates.sh")] + (["--quick"] if quick else [])
    p = subprocess.run(cmd, capture_output=True, text=True, cwd=LIP)
    return p.returncode == 0, (p.stdout or "") + (
        "\n--- stderr ---\n" + p.stderr if p.stderr else "")


def _sleep_for(attempt: int, kind: str) -> int:
    # This host's DNS wedges system-wide roughly every 2.5 hours while nslookup
    # keeps working, so a network failure is expected and survivable. A
    # subscription rate limit wants a far longer wait than a socket error.
    return min((900 if kind == "rate" else 60) * (2 ** attempt), 3600)


def _classify(blob: str) -> str:
    if RATE_LIMIT_PAT.search(blob):
        return "rate"
    return "net" if NET_FAIL_PAT.search(blob) else "other"


def run_codex(prompt: str, art: Path, effort: str,
              resume: str | None = None, attempts: int = 4) -> str | None:
    out_file, log_file = art.with_suffix(".out.md"), art.with_suffix(".raw.log")
    argv = [CODEX, "exec"]
    if resume:
        argv += ["resume", resume]
    argv += ["-m", MODEL, "-c", f"model_reasoning_effort={effort}",
             "--skip-git-repo-check", "--sandbox", "read-only",
             "-o", str(out_file), "-"]
    for attempt in range(attempts):
        if STOP_FILE.exists():
            return None
        try:
            with log_file.open("w") as lf:
                subprocess.run(argv, input=prompt, text=True, cwd=LIP,
                               stdout=lf, stderr=subprocess.STDOUT,
                               timeout=TURN_TIMEOUT)
        except subprocess.TimeoutExpired:
            log(f"    codex TIMEOUT (attempt {attempt+1})")
            continue
        if out_file.exists() and out_file.stat().st_size > 0:
            return out_file.read_text()
        kind = _classify(log_file.read_text()[-4000:] if log_file.exists() else "")
        w = _sleep_for(attempt, kind)
        log(f"    codex empty (kind={kind}); sleeping {w}s")
        time.sleep(w)
    return None


def run_claude(prompt: str, art: Path, st: dict,
               attempts: int = 4) -> str | None:
    for attempt in range(attempts):
        if STOP_FILE.exists():
            return None
        # `acceptEdits` auto-approves EDITS ONLY -- it denies Bash, with no
        # human present to approve. Measured on the first real run: 11 of 11
        # Bash calls denied, so the implementer could not run the gates, the
        # tests, or bd, and burned a $1.00 turn discovering it could not work.
        #
        # The protections that actually matter are conductor-side and do not
        # depend on what Claude is permitted to do: the control-plane hashes,
        # the scope allowlist, and the fact that only the conductor ever
        # commits. What is fenced off below is the small set of commands that
        # could destroy evidence or rewrite history faster than those checks
        # would notice.
        argv = [CLAUDE, "-p", prompt, "--output-format", "json",
                "--permission-mode", "bypassPermissions", "--max-turns", "220",
                "--disallowedTools",
                "Bash(git push),Bash(git reset),Bash(git clean),"
                "Bash(git checkout),Bash(git commit),Bash(git rebase),"
                "Bash(rm -rf),Bash(sqlite3),Bash(bd dolt)"]
        if ROTATE and st.get("claude_session"):
            argv += ["--resume", st["claude_session"]]
        try:
            p = subprocess.run(argv, capture_output=True, text=True, cwd=LIP,
                               timeout=CLAUDE_TIMEOUT)
        except subprocess.TimeoutExpired as e:
            # Python populates .stdout/.stderr on the exception and this handler
            # used to discard both. That is why lip-732 attempt 1's turn count
            # is unrecoverable: the .raw.json write below never runs on this
            # path, so a timeout left NO artifact at all, and there was no way
            # to tell whether the turn was seconds from reporting or nowhere
            # near it -- exactly the number needed to size CLAUDE_TIMEOUT.
            partial = e.stdout or ""
            if isinstance(partial, bytes):
                partial = partial.decode("utf-8", "replace")
            art.with_suffix(f".timeout{attempt+1}.txt").write_text(partial)
            log(f"    claude TIMEOUT (attempt {attempt+1}); kept "
                f"{len(partial)} bytes of partial output")
            continue
        art.with_suffix(".raw.json").write_text(p.stdout or "")
        try:
            env = json.loads(p.stdout)
        except Exception:
            k = _classify((p.stdout or "") + (p.stderr or ""))
            w = _sleep_for(attempt, k)
            log(f"    claude unparseable (kind={k}); sleeping {w}s")
            time.sleep(w)
            continue
        if env.get("is_error"):
            k = _classify(json.dumps(env)[:4000])
            w = _sleep_for(attempt, k)
            log(f"    claude is_error (kind={k}); sleeping {w}s")
            time.sleep(w)
            continue
        st["claude_session"] = env.get("session_id")
        u = env.get("usage") or {}
        # Context occupancy is the LAST internal iteration's, not the sum over
        # the run. A -p run makes many model calls; summing their cache_read
        # counts the same context once per call and overcounts wildly -- the
        # first real run reported 1,235,807 tokens against a 1,000,000 window,
        # i.e. 124%, which would have fired the rotation trigger on every single
        # turn. Codex's review flagged exactly this and I implemented it wrong
        # anyway; the impossible number is what caught it.
        last = (u.get("iterations") or [{}])[-1] or {}
        src = last if last else u
        st["claude_ctx"] = (src.get("input_tokens", 0)
                            + src.get("cache_read_input_tokens", 0)
                            + src.get("cache_creation_input_tokens", 0))
        for m in (env.get("modelUsage") or {}).values():
            if m.get("contextWindow"):
                st["claude_window"] = m["contextWindow"]
        return env.get("result")
    return None


def json_block(text: str | None) -> dict | None:
    if not text:
        return None
    for b in reversed(re.findall(r"```json\s*(.+?)```", text, re.S)):
        try:
            return json.loads(b)
        except Exception:
            continue
    m = re.search(r"\{.*\}", text, re.S)
    if m:
        try:
            return json.loads(m.group(0))
        except Exception:
            return None
    return None


def bd(*args: str) -> str:
    p = subprocess.run(["bd", *args], capture_output=True, text=True, cwd=LIP)
    return (p.stdout or "") + (p.stderr or "")


def park(unit: str, skip: set[str] | None = None) -> None:
    """Take a unit out of the ready queue DURABLY.

    An in-memory skip set does not survive a restart, so every restart re-picked
    each parked unit and spent a codex driver turn re-deciding it -- observed on
    lip-52l and lip-9r3, both operator-only, both re-driven after each restart.

    `bd defer` is the right primitive: deferred is neither blocked nor closed.
    The obligation stays open and revisitable, which is what parking means; it
    just stops presenting itself as ready work.
    """
    bd("defer", unit)
    if skip is not None:
        skip.add(unit)


def next_unit(skip: set[str]) -> str | None:
    for i in re.findall(r"\b(lip-[0-9a-z]+)\b", bd("ready")):
        if i in skip:
            continue
        # Match on the TITLE LINE only. `bd show` also prints a PARENT line,
        # which for every child of an epic contains the word "EPIC" -- so
        # scanning the whole record rejects exactly the work we want.
        title = bd("show", i).splitlines()[0] if bd("show", i) else ""
        if "EPIC" in title:
            continue
        return i
    return None


def run_mutation(f: dict, wd: Path) -> tuple[str, str]:
    """Execute a proposed mutation against the EXISTING gates.

    This is what settles a finding. Caught -> refuted by evidence, closed for
    good. Survived -> a real oracle gap, filed as its own bounded unit. It
    converts a disagreement between two models into a decidable test, which is
    the only reason the finding stream terminates.
    """
    path, old, new = f.get("file"), f.get("old"), f.get("new")
    if not (path and old and new):
        return "inadmissible", "no concrete mutation supplied"
    tree = wd / "mut"
    if tree.exists():
        shutil.rmtree(tree)
    shutil.copytree(LIP / "go", tree, symlinks=True)
    tgt = tree / path
    if not tgt.exists():
        tgt = tree / path.replace("go/", "", 1)
    if not tgt.exists():
        return "inadmissible", f"{path} not found"
    src = tgt.read_text()
    if src.count(old) != 1:
        return "inadmissible", f"anchor appears {src.count(old)}x, need exactly 1"
    tgt.write_text(src.replace(old, new))
    env = {**os.environ, "CGO_ENABLED": "0"}
    if subprocess.run(["go", "build", "./..."], cwd=tree, capture_output=True,
                      env=env).returncode != 0:
        # Caught only by the compiler tests the Go compiler, not the gate.
        return "inadmissible", "does not compile"
    t = subprocess.run(["go", "test", "-count=1", "./harness/...",
                        "./cmd/harness/..."], cwd=tree, capture_output=True,
                       text=True, env=env)
    if t.returncode != 0:
        # r'(\\w+)' is a raw string containing an ESCAPED backslash, so it never
        # matched and every kill was recorded as "(unnamed)" -- which defeats
        # the point, since the ledger's whole value is naming the test that
        # caught it.
        names = re.findall(r"--- FAIL: ([\w/]+)", t.stdout)
        return "refuted", f"CAUGHT by {names or ['(unnamed)']}"
    return "admitted", "SURVIVED every existing gate"


CHALLENGE_MIN_COVERAGE = 0.60


def coverage_of(paths: set[str]) -> tuple[float, str]:
    """Statement coverage of the packages a diff touched.

    The challenge turn is only informative where there is coverage to hole. At
    ~10% implementation it was admitting 3 of 3 findings, 0 refuted -- not
    because the challenger was sharp but because almost nothing was tested yet,
    so every mutation trivially survived. "SURVIVED every existing gate" then
    means "this area has no tests", which is already known and does not need a
    max-effort adversarial turn to discover.

    Worse, it inverted the mechanism's purpose: mutation-survival is meant to
    REFUTE findings the gates already catch, and so bound the finding stream.
    Refuting nothing, it became an unbounded generator of coverage gaps that
    displaced the planned ladder work.
    """
    pkgs = set()
    for p in paths:
        if p.startswith("go/") and p.endswith(".go"):
            pkgs.add("./" + str(Path(p).relative_to("go").parent))
    if not pkgs:
        return 0.0, "no Go packages touched"
    r = subprocess.run(["go", "test", "-cover", "-count=1", *sorted(pkgs)],
                       cwd=LIP / "go", capture_output=True, text=True,
                       env={**os.environ, "CGO_ENABLED": "0"})
    pcts = [float(m) for m in re.findall(r"coverage: ([\d.]+)% of statements",
                                         r.stdout)]
    if not pcts:
        return 0.0, f"no coverage reported for {sorted(pkgs)}"
    worst = min(pcts) / 100.0
    return worst, f"{sorted(pkgs)} worst coverage {worst:.0%}"


def tmpl(name: str, **kw) -> str:
    t = (PROTO / name).read_text()
    for k, v in kw.items():
        t = t.replace("{{" + k + "}}", str(v))
    return t


# Paths that change as a SIDE EFFECT of running the loop, not because the
# implementer wrote to them. Iteration 1 discarded a correct attempt because
# `git status` reported the conductor's own STATE.json, HEARTBEAT and LOCK as
# out-of-scope writes -- and then reverted STATE.json, wiping its own counters.
#
# Excluding loop/ from the SCOPE check is safe precisely because it is covered
# more strictly elsewhere: every protocol file, gates.sh and conductor.py are
# hash-pinned in CONTROL_FILES, so an agent editing one halts the run outright
# rather than merely failing a scope check.
NOT_AGENT_WRITES = (
    "loop/",                              # conductor state, run artifacts
    ".beads/",                            # bd's own storage
    "notes/harness-negative-control.md",  # REGENERATED by gates.sh itself
    "notes/harness-negative-control.partial.md",
)


def changed_paths() -> list[str]:
    out = subprocess.run(["git", "status", "--porcelain"], cwd=LIP,
                         capture_output=True, text=True).stdout
    paths = []
    for ln in out.splitlines():
        if not ln.strip():
            continue
        p = ln[3:].strip()
        if p.startswith('"') and p.endswith('"'):   # git quotes odd names
            p = p[1:-1]
        if any(p.startswith(x) for x in NOT_AGENT_WRITES):
            continue
        paths.append(p)
    return paths


def main() -> int:
    # Process audit 2026-09-26: refuse before any historical side effect.
    print("RETIRED: unattended conductor disabled; see loop/START.md",
          file=sys.stderr)
    return 2

    # Historical implementation retained below for audit, not execution.
    global MAX_ITERATIONS, DEADLINE_HOURS
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--max-iterations", type=int, default=MAX_ITERATIONS,
                    help="stop after N units; use 1 for a supervised smoke run")
    ap.add_argument("--deadline-hours", type=float, default=DEADLINE_HOURS)
    ap.add_argument("--dry-run", action="store_true",
                    help="check startup, control pinning and unit selection, "
                         "then exit without invoking a model")
    args = ap.parse_args()
    MAX_ITERATIONS = args.max_iterations
    DEADLINE_HOURS = args.deadline_hours

    for d in (STATE_DIR, RUN_DIR):
        d.mkdir(parents=True, exist_ok=True)
    if LOCK_FILE.exists():
        print(f"LOCK present ({LOCK_FILE}); another conductor may be running. "
              f"Remove it if not.", file=sys.stderr)
        return 3
    LOCK_FILE.write_text(f"{os.getpid()} {now()}\n")
    STOP_FILE.unlink(missing_ok=True)

    st = load_state()
    if st.get("control") is None:
        st["control"] = control_hashes()
    if not LEDGER.exists():
        LEDGER.write_text("# Finding ledger\n\nAppend-only. Conductor-owned. A "
                          "finding already disposed of here is closed on sight.\n\n")
    save_state(st)

    deadline = time.time() + DEADLINE_HOURS * 3600
    skip: set[str] = set()
    terminal = "ALLOWED_QUEUE_EXHAUSTED"
    # Namespace run artifacts per START, not per iteration number. Deleting
    # STATE.json restarts the counter at 1, which silently OVERWROTE the
    # previous run's 0001/ -- so one directory held a mix of two runs' driver,
    # audit and adjudication records and could not be read as either.
    run_tag = datetime.now(timezone.utc).strftime("%m%dT%H%M")
    log(f"conductor start; max_iterations={MAX_ITERATIONS}; "
        f"deadline {DEADLINE_HOURS}h; rotate={ROTATE}; "
        f"{len(st['control'])} control files pinned")

    if args.dry_run:
        ok, gout = gates(quick=True)
        u = next_unit(set())
        log(f"dry-run: gates={'GREEN' if ok else 'RED'}; next_unit={u}; "
            f"codex={'found' if Path(CODEX).exists() else 'MISSING'}; "
            f"claude={CLAUDE}")
        LOCK_FILE.unlink(missing_ok=True)
        return 0 if (ok and u) else 1

    try:
        while st["iteration"] < MAX_ITERATIONS:
            if STOP_FILE.exists():
                terminal = "STOPPED_BY_OPERATOR"; break
            if time.time() > deadline:
                terminal = "DEADLINE_REACHED"; break
            free = shutil.disk_usage(LIP).free / 2**30
            if free < MIN_FREE_GB:
                terminal = f"INFRASTRUCTURE_STOP (disk {free:.1f}GB)"; break

            cur = control_hashes()
            drift = [f for f, h in st["control"].items() if cur.get(f) != h]
            if drift:
                log(f"FATAL: control plane modified: {drift}")
                ledger(f"## {now()} — CONTROL PLANE MUTATED: {drift} — halted")
                terminal = "BLOCKED (control plane mutated)"; break

            st["iteration"] += 1
            it = st["iteration"]
            wd = RUN_DIR / f"{run_tag}-{it:04d}"
            wd.mkdir(parents=True, exist_ok=True)
            log(f"=== iteration {it} ===")

            # A REVISE must return to the SAME unit. Previously the decision
            # fell through to next_unit(), which re-queried `bd ready` and
            # picked whatever sorted first -- so the repair instruction was
            # discarded, the unit was abandoned mid-flight, and its uncommitted
            # work stayed in the tree to be folded into the NEXT unit's audit
            # diff. Observed: lip-gmf was REVISEd, then lip-6qe was started
            # while 161 lines of lip-gmf's work sat in the working tree.
            pending = st.get("pending") or {}
            unit = pending.get("unit") or next_unit(skip)
            repair = pending.get("repair", "")
            if not unit:
                log("  no ready work"); terminal = "ALLOWED_QUEUE_EXHAUSTED"; break
            rounds = st["unit_rounds"].get(unit, 0) + 1
            st["unit_rounds"][unit] = rounds
            save_state(st)                      # debit BEFORE launching a child,
                                                # so a crash cannot refund a round
            detail = bd("show", unit)
            nc_before = nc_mutation_ids()
            ok, gout = gates(quick=True)
            log(f"  unit {unit} round {rounds}/{MAX_ROUNDS}; "
                f"gates {'GREEN' if ok else 'RED'}")

            # ---- driver -------------------------------------------------
            reply = run_codex(tmpl("driver.md", UNIT=unit, DETAIL=detail,
                                   GATES=gout),
                              wd / "01-driver", "xhigh",
                              resume=st.get("codex_thread") if ROTATE else None)
            directive = json_block(reply)
            if not directive:
                log("  driver produced no directive; skipping"); save_state(st); continue
            (wd / "01-directive.json").write_text(json.dumps(directive, indent=2))
            dec0 = str(directive.get("decision", "")).upper()

            # ALREADY_SATISFIED closes an obligation without doing work, so the
            # claim is VERIFIED, never trusted. The driver must name a Go test
            # symbol; if it is not actually in the tree the unit stays open.
            # Without this a unit can be closed by assertion, which is codex's
            # ranked failure #3 -- a requirement silently vanishing.
            if dec0 == "ALREADY_SATISFIED":
                # NOT AVAILABLE once an implementation attempt has been made.
                # An audit that named a defect makes "already satisfied"
                # incoherent, and it is the cheapest exit on the board, so it
                # attracts exactly the traffic it should not.
                #
                # Observed on lip-ogc round 3/3: the audit had just found that
                # promotion converts a base-P1 reducer to effective P0 and so
                # starves the one class H-QUE-3 forbids starving. The driver
                # answered ALREADY_SATISFIED citing
                # TestEligibleAddingCancelPrecedesFreshRequote -- a real test,
                # for a different obligation (H-Q-9a cancel ordering, not
                # H-QUE-3 reserved capacity). The grep guard passed, the unit
                # closed, and the starvation bug survived at queue.go:937.
                if rounds > 1 or repair:
                    log("  ALREADY_SATISFIED refused: this is round "
                        f"{rounds} and a defect was already named. Treating as "
                        f"REVISE.")
                    ledger(f"## {now()} — {unit} ALREADY_SATISFIED refused on "
                           f"round {rounds} (a defect was already named)")
                    st["pending"] = {"unit": unit, "repair": repair}
                    save_state(st); continue
                sym = str(directive.get("evidence_symbol", "")).strip()
                found = bool(sym) and subprocess.run(
                    ["grep", "-rqn", f"func {sym}", "go"], cwd=LIP).returncode == 0
                if found:
                    log(f"  ALREADY_SATISFIED, verified by {sym} -- closing")
                    bd("update", unit, "--append-notes",
                       f"Closed at iteration {it}: already satisfied by {sym}, "
                       f"verified present in the tree by the conductor.")
                    bd("close", unit)
                    st["advanced"] += 1
                    st["pending"] = {}
                    ledger(f"## {now()} — {unit} ALREADY_SATISFIED, verified "
                           f"by `{sym}`")
                else:
                    log(f"  ALREADY_SATISFIED claimed {sym!r} but it is NOT in "
                        f"the tree -- refusing to close")
                    bd("update", unit, "--append-notes",
                       f"Iteration {it}: driver claimed already-satisfied by "
                       f"{sym!r}, which does not exist. Not closed.")
                    ledger(f"## {now()} — {unit} unverifiable "
                           f"ALREADY_SATISFIED claim (`{sym}`) — left open")
                    # skip is consulted by next_unit(); `pending` bypasses it
                    # entirely, so without this the unit is re-picked forever.
                    st["pending"] = {}
                    skip.add(unit)
                save_state(st); continue

            # OPERATOR_ONLY must PARK, never close. It means the obligation is
            # real and still owed -- just not doable by an agent.
            if dec0 == "OPERATOR_ONLY":
                log("  OPERATOR_ONLY -> parking (obligation remains open)")
                bd("update", unit, "--append-notes",
                   f"OPERATOR_ONLY at iteration {it}: {directive.get('scope','')}")
                bd("tag", unit, "operator-only")
                ledger(f"## {now()} — {unit} OPERATOR_ONLY — parked, still owed")
                st["pending"] = {}      # see SPEC_CONFLICT below: a pending
                                        # unit outranks bd and un-parks itself
                park(unit, skip); st["parked"] += 1; save_state(st); continue

            if dec0 == "SPEC_CONFLICT":
                log("  SPEC_CONFLICT -> parking for supervision")
                bd("update", unit, "--append-notes",
                   f"SPEC_CONFLICT at iteration {it}: {directive.get('scope','')}")
                bd("tag", unit, "spec-patch")
                ledger(f"## {now()} — {unit} SPEC_CONFLICT — needs a human")
                # Clearing `pending` is what makes the park STICK. `pending` is
                # consulted BEFORE next_unit() (:546), so a parked unit that is
                # still pending is re-selected on the very next iteration --
                # `bd defer` cannot hide it, because the pending path never asks
                # bd. Observed 2026-08-14: lip-357 was SPEC_CONFLICTed and
                # parked at iteration 21, then immediately re-picked at 22 for
                # round 3/3, and would have spun ~14 min per iteration (quick
                # gate + driver) for the rest of the night, parking a
                # already-parked unit each time.
                st["pending"] = {}
                park(unit, skip); st["parked"] += 1; save_state(st); continue

            allowed = set(directive.get("allowed_paths") or [])

            # A directive may not target the control plane. Found by the
            # implementer itself, which refused to write and explained why:
            # such an attempt passes the scope check (the path IS in
            # allowed_paths), passes the gates, and is ACCEPTED -- and then
            # kills the run at the next iteration's drift check, with the
            # operator asleep. The driver cannot see CONTROL_FILES, so this is
            # enforcement rather than convention, per RULES.md's own stance.
            undirectable = allowed & set(CONTROL_FILES)
            if undirectable:
                log(f"  directive targets the control plane {sorted(undirectable)} "
                    f"-- parking for the operator")
                bd("update", unit, "--append-notes",
                   f"OPERATOR-ONLY at iteration {it}: the work requires editing "
                   f"{sorted(undirectable)}, which is hash-pinned. An agent "
                   f"cannot do this without halting the run.")
                bd("tag", unit, "operator-only")
                ledger(f"## {now()} — {unit} needs a control-plane edit "
                       f"({sorted(undirectable)}) — operator-only, parked")
                st["pending"] = {}      # as SPEC_CONFLICT
                park(unit, skip); st["parked"] += 1; save_state(st); continue

            # ---- implement ----------------------------------------------
            d_text = json.dumps(directive, indent=2)
            if repair:
                d_text += (f"\n\nTHIS IS A REPAIR ROUND ({rounds}/{MAX_ROUNDS}). "
                           f"Your previous attempt is already in the working "
                           f"tree and was NOT accepted. Apply exactly this "
                           f"repair on top of it; do not start over and do not "
                           f"widen scope:\n\n{repair}")
            result = run_claude(tmpl("implement.md", UNIT=unit,
                                     DIRECTIVE=d_text),
                                wd / "02-implement", st)
            save_state(st)
            if result is None:
                log("  implement failed after retries"); continue
            (wd / "02-implement.md").write_text(result)

            ok, gout = gates(quick=False)
            (wd / "gates.txt").write_text(gout)
            # Exclude tool-owned paths from the diff the reviewers see. bd
            # rewrites .beads/interactions.jsonl on every command, and the
            # auditor reasonably flagged it as an out-of-scope write -- noise
            # that costs an audit round and teaches the reviewer to distrust
            # the scope signal.
            diff = subprocess.run(
                ["git", "diff", "HEAD", "--", ".",
                 ":(exclude)loop", ":(exclude).beads",
                 ":(exclude)notes/harness-negative-control.md",
                 ":(exclude)notes/harness-negative-control.partial.md"],
                cwd=LIP, capture_output=True, text=True).stdout
            (wd / "02.patch").write_text(diff)

            nc_ok, nc_why = nc_ratchet_ok(nc_before)
            if not nc_ok:
                log(f"  NEGATIVE CONTROL WEAKENED: {nc_why} -- discarding")
                subprocess.run(["git", "checkout", "--",
                                "scripts/harness_negative_control.py"], cwd=LIP)
                ledger(f"## {now()} — {unit} weakened the negative control "
                       f"({nc_why}) — discarded")
                save_state(st); continue
            if nc_why != "unchanged":
                log(f"  negative control ratcheted: {nc_why}")

            touched = changed_paths()
            outside = [p for p in touched if allowed and p not in allowed]
            if outside:
                log(f"  SCOPE VIOLATION: {outside} -- discarding attempt")
                subprocess.run(["git", "checkout", "--", *outside], cwd=LIP)
                ledger(f"## {now()} — {unit} scope violation {outside} — discarded")
                save_state(st); continue
            log(f"  gates {'GREEN' if ok else 'RED'}; {len(diff.splitlines())} diff lines")

            # ---- audit || challenge (fresh threads) ----------------------
            # The challenge is spent ONCE per unit and is never renewed for a
            # repair. That non-renewal is the convergence guarantee.
            cov, cov_why = coverage_of(set(touched))
            do_challenge = (unit not in st["unit_challenged"]
                            and cov >= CHALLENGE_MIN_COVERAGE)
            if unit not in st["unit_challenged"] and not do_challenge:
                log(f"  challenge SUSPENDED: {cov_why}, below "
                    f"{CHALLENGE_MIN_COVERAGE:.0%}. A surviving mutation here "
                    f"would only restate that the package is untested.")
            a_p = tmpl("audit.md", DIRECTIVE=json.dumps(directive, indent=2),
                       DIFF=diff[:120000], GATES=gout)
            with ThreadPoolExecutor(max_workers=2) as ex:
                fa = ex.submit(run_codex, a_p, wd / "03-audit", "xhigh")
                fx = (ex.submit(run_codex,
                                tmpl("challenge.md", DIFF=diff[:120000],
                                     LEDGER=LEDGER.read_text()[-20000:]),
                                wd / "04-challenge", "max")
                      if do_challenge else None)
                audit = json_block(fa.result()) or {}
                attack = json_block(fx.result()) if fx else {}
            if do_challenge:
                st["unit_challenged"].append(unit)
            (wd / "03-audit.json").write_text(json.dumps(audit, indent=2))

            verdicts = []
            for f in (attack or {}).get("findings", [])[:2]:
                cls, why = run_mutation(f, wd)
                st["raised"] += 1
                st[{"refuted": "refuted", "admitted": "admitted",
                    "inadmissible": "inadmissible"}[cls]] += 1
                rec = {"title": f.get("title"), "class": cls, "verdict": why}
                if cls == "admitted":
                    rec["filed"] = bd("q", f"[oracle gap] {str(f.get('title'))[:80]}").strip()
                verdicts.append(rec)
                ledger(f"## {now()} — it{it} {unit} — {f.get('title','untitled')}\n"
                       f"- **{cls}**: {why}\n"
                       f"- reachability: {f.get('reachability','(none)')}\n"
                       f"- mutation: `{f.get('file')}`")
            (wd / "05-verdicts.json").write_text(json.dumps(verdicts, indent=2))

            # ---- adjudicate ---------------------------------------------
            # `max` effort is reserved for genuine divergence: a failing gate, a
            # DRIFT/NEW-BREAK audit, a surviving mutant, or a repeat round.
            diverged = (not ok
                        or audit.get("fidelity") == "DRIFT"
                        or audit.get("change_safety") == "NEW-BREAK"
                        or any(v["class"] == "admitted" for v in verdicts)
                        or rounds > 1)
            adj = json_block(run_codex(
                tmpl("adjudicate.md", UNIT=unit, GATES=gout,
                     AUDIT=json.dumps(audit, indent=2),
                     VERDICTS=json.dumps(verdicts, indent=2)),
                wd / "06-adjudicate", "max" if diverged else "xhigh",
                resume=st.get("codex_thread") if ROTATE else None)) or {}
            decision = str(adj.get("decision", "PARK")).upper()
            (wd / "06-adjudicate.json").write_text(json.dumps(adj, indent=2))
            log(f"  {decision}: {str(adj.get('reason',''))[:150]}")

            if decision == "ADVANCE" and ok:
                subprocess.run(["git", "add", "-A"], cwd=LIP)
                subprocess.run(
                    ["git", "-c", "user.name=loop", "-c",
                     "user.email=noreply@localhost", "commit", "-q", "-m",
                     f"{unit}: {str(adj.get('summary','advance'))[:70]}\n\n"
                     f"Loop iteration {it}, round {rounds}. Audit "
                     f"{audit.get('fidelity','?')}/{audit.get('change_safety','?')}. "
                     f"Challenge: {len(verdicts)} mutation(s), "
                     f"{sum(1 for v in verdicts if v['class']=='admitted')} survived."],
                    cwd=LIP)
                bd("close", unit)
                st["advanced"] += 1
                st["pending"] = {}
                st["control"] = control_hashes()   # conductor's own commit is legitimate
            elif decision in ("PARK", "SPEC_CONFLICT") or rounds >= MAX_ROUNDS:
                bd("update", unit, "--append-notes",
                   f"PARKED at iteration {it} after {rounds} round(s): "
                   f"{adj.get('blocker') or adj.get('reason','')}")
                # Discard the abandoned attempt. Leaving it in the working tree
                # would fold it into the NEXT unit's diff, so the audit would
                # judge one unit's change against another unit's directive and
                # the parked work would be committed by accident.
                subprocess.run(["git", "checkout", "--", "."], cwd=LIP)
                subprocess.run(["git", "clean", "-fd", "go/harness",
                                "go/cmd/harness"], cwd=LIP)
                park(unit, skip)
                st["parked"] += 1
                st["pending"] = {}
            else:
                st["revised"] += 1
                st["pending"] = {"unit": unit,
                                 "repair": str(adj.get("repair", ""))[:4000]}

            if ROTATE and st["claude_ctx"] / max(st["claude_window"], 1) >= ROTATE_AT_FRACTION:
                log(f"  rotating claude ({st['claude_ctx']/st['claude_window']:.1%})")
                st["claude_session"] = None
            save_state(st)
    finally:
        st["terminal"] = terminal
        save_state(st)
        LOCK_FILE.unlink(missing_ok=True)
        log(f"TERMINAL={terminal} iterations={st['iteration']} "
            f"advanced={st['advanced']} parked={st['parked']} "
            f"revised={st['revised']} | findings raised={st['raised']} "
            f"refuted={st['refuted']} admitted={st['admitted']} "
            f"inadmissible={st['inadmissible']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
