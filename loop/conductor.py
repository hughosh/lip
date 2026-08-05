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
    "scripts/harness_negative_control.py",
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
CLAUDE_TIMEOUT = 30 * 60
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


def load_state() -> dict:
    if STATE_FILE.exists():
        return json.loads(STATE_FILE.read_text())
    return {"iteration": 0, "advanced": 0, "parked": 0, "revised": 0,
            "claude_session": None, "claude_ctx": 0, "claude_window": 1_000_000,
            "codex_thread": None, "unit_rounds": {}, "unit_challenged": [],
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
        # acceptEdits auto-approves file edits; the implementer also has to run
        # the gates, go, gofmt and bd, so Bash is granted. The protections that
        # actually matter are conductor-side and do not depend on what Claude is
        # permitted to do: control-plane hashes, the scope allowlist, and the
        # fact that only the conductor ever commits. What IS fenced off here is
        # the small set of commands that could destroy evidence or rewrite
        # history faster than those checks could notice.
        argv = [CLAUDE, "-p", prompt, "--output-format", "json",
                "--permission-mode", "acceptEdits", "--max-turns", "120",
                "--disallowedTools",
                "Bash(git push),Bash(git reset),Bash(git clean),"
                "Bash(git checkout),Bash(git commit),Bash(git rebase),"
                "Bash(rm -rf),Bash(sqlite3),Bash(bd dolt)"]
        if ROTATE and st.get("claude_session"):
            argv += ["--resume", st["claude_session"]]
        try:
            p = subprocess.run(argv, capture_output=True, text=True, cwd=LIP,
                               timeout=CLAUDE_TIMEOUT)
        except subprocess.TimeoutExpired:
            log(f"    claude TIMEOUT (attempt {attempt+1})")
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
        st["claude_ctx"] = (u.get("input_tokens", 0)
                            + u.get("cache_read_input_tokens", 0)
                            + u.get("cache_creation_input_tokens", 0))
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
        return "refuted", f"CAUGHT by {re.findall(r'--- FAIL: (\\w+)', t.stdout) or ['(unnamed)']}"
    return "admitted", "SURVIVED every existing gate"


def tmpl(name: str, **kw) -> str:
    t = (PROTO / name).read_text()
    for k, v in kw.items():
        t = t.replace("{{" + k + "}}", str(v))
    return t


def changed_paths() -> list[str]:
    out = subprocess.run(["git", "status", "--porcelain"], cwd=LIP,
                         capture_output=True, text=True).stdout
    return [ln[3:].strip() for ln in out.splitlines() if ln.strip()]


def main() -> int:
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
    log(f"conductor start; deadline {DEADLINE_HOURS}h; rotate={ROTATE}; "
        f"{len(st['control'])} control files pinned")

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
            wd = RUN_DIR / f"{it:04d}"
            wd.mkdir(parents=True, exist_ok=True)
            log(f"=== iteration {it} ===")

            unit = next_unit(skip)
            if not unit:
                log("  no ready work"); terminal = "ALLOWED_QUEUE_EXHAUSTED"; break
            rounds = st["unit_rounds"].get(unit, 0) + 1
            st["unit_rounds"][unit] = rounds
            save_state(st)                      # debit BEFORE launching a child,
                                                # so a crash cannot refund a round
            detail = bd("show", unit)
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
            if directive.get("decision") == "SPEC_CONFLICT":
                log("  SPEC_CONFLICT -> parking for supervision")
                bd("update", unit, "--append-notes",
                   f"SPEC_CONFLICT at iteration {it}: {directive.get('scope','')}")
                bd("tag", unit, "spec-patch")
                ledger(f"## {now()} — {unit} SPEC_CONFLICT — needs a human")
                skip.add(unit); st["parked"] += 1; save_state(st); continue

            allowed = set(directive.get("allowed_paths") or [])

            # ---- implement ----------------------------------------------
            result = run_claude(tmpl("implement.md", UNIT=unit,
                                     DIRECTIVE=json.dumps(directive, indent=2)),
                                wd / "02-implement", st)
            save_state(st)
            if result is None:
                log("  implement failed after retries"); continue
            (wd / "02-implement.md").write_text(result)

            ok, gout = gates(quick=False)
            (wd / "gates.txt").write_text(gout)
            diff = subprocess.run(["git", "diff", "HEAD"], cwd=LIP,
                                  capture_output=True, text=True).stdout
            (wd / "02.patch").write_text(diff)

            touched = changed_paths()
            outside = [p for p in touched
                       if allowed and p not in allowed
                       and not p.startswith("loop/run/")
                       and not p.startswith(".beads/")]
            if outside:
                log(f"  SCOPE VIOLATION: {outside} -- discarding attempt")
                subprocess.run(["git", "checkout", "--", *outside], cwd=LIP)
                ledger(f"## {now()} — {unit} scope violation {outside} — discarded")
                save_state(st); continue
            log(f"  gates {'GREEN' if ok else 'RED'}; {len(diff.splitlines())} diff lines")

            # ---- audit || challenge (fresh threads) ----------------------
            # The challenge is spent ONCE per unit and is never renewed for a
            # repair. That non-renewal is the convergence guarantee.
            do_challenge = unit not in st["unit_challenged"]
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
                skip.add(unit)
                st["parked"] += 1
            else:
                st["revised"] += 1

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
