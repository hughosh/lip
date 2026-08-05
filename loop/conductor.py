#!/usr/bin/env python3
"""The unattended two-agent implementation loop.

Control flow is owned by THIS SCRIPT, not by either model. That is the whole
design decision. An LLM driving an eight-hour loop accumulates context, drifts
from its protocol, and cannot be bounded, resumed, or audited; a script does not
drift. Codex keeps the intellectual lead -- it authors every substantive
decision -- and this file owns only sequencing, budget, retries and termination.

It is the same argument harness-spec.md §2 makes for I2: the monitor works
because it is STRUCTURALLY unable to be stopped by the thing it watches.

Per unit:

    gates(quick)
      -> codex DRIVER   (resumed thread)   directive
      -> claude IMPLEMENT                  code + tests
      -> gates(full)
      -> codex AUDIT  ||  codex ATTACK     (fresh threads, in parallel)
      -> mutation-survival test            findings settled by evidence
      -> codex ADJUDICATE (resumed)        advance | revise | park
      -> commit, bd update, ledger, rotate

Stop with:  touch loop/state/STOP
"""
from __future__ import annotations

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
STATE_FILE = STATE_DIR / "STATE.json"
LEDGER = STATE_DIR / "LEDGER.md"

CODEX = "/Applications/ChatGPT.app/Contents/Resources/codex"
CLAUDE = shutil.which("claude") or "claude"
PY = "/Users/hugh/kek/.venv/bin/python"
SPEC = LIP / "notes" / "harness-spec.md"

# Reasoning effort per turn. `max` is ~10 minutes of wall clock with no output,
# so it is spent only where an independent hostile read is the actual product.
EFFORT = {"driver": "xhigh", "audit": "xhigh", "attack": "max", "adjudicate": "max"}
MODEL = "gpt-5.6-sol"

MAX_ROUNDS_PER_UNIT = 3          # then park; parking never blocks the queue
MAX_ITERATIONS = 200             # backstop, not a plan
TURN_TIMEOUT = 45 * 60           # a max-effort turn can legitimately take ~10min
CLAUDE_TIMEOUT = 30 * 60
ROTATE_AT_FRACTION = 0.15        # of the reported context window (measured: the
                                 # cost/quota per turn is linear in context
                                 # carried, ~$3.09 per Mtok, so rotating early
                                 # is strictly cheaper than rotating late)
ROTATE_CODEX_EVERY = 6           # units; staggered against Claude's rotation so
                                 # the two never reboot in the same iteration

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
    with (STATE_DIR / "conductor.log").open("a") as fh:
        fh.write(line + "\n")


def load_state() -> dict:
    if STATE_FILE.exists():
        return json.loads(STATE_FILE.read_text())
    return {
        "iteration": 0,
        "units_advanced": 0,
        "units_parked": 0,
        "claude_session": None,
        "claude_ctx_tokens": 0,
        "claude_ctx_window": 1_000_000,
        "codex_driver_thread": None,
        "codex_units_since_rotate": 0,
        "claude_rotations": 0,
        "codex_rotations": 0,
        "spec_sha": None,
        "findings_raised": 0,
        "findings_refuted": 0,
        "findings_admitted": 0,
        "started": now(),
    }


def save_state(st: dict) -> None:
    STATE_FILE.write_text(json.dumps(st, indent=2) + "\n")


def sha(path: Path) -> str:
    import hashlib
    return hashlib.sha256(path.read_bytes()).hexdigest()


def ledger(entry: str) -> None:
    with LEDGER.open("a") as fh:
        fh.write(entry.rstrip() + "\n\n")


# --------------------------------------------------------------------------
# gates -- the oracle
# --------------------------------------------------------------------------

def gates(quick: bool = False) -> tuple[bool, str]:
    cmd = [str(LOOP / "gates.sh")] + (["--quick"] if quick else [])
    p = subprocess.run(cmd, capture_output=True, text=True, cwd=LIP)
    out = (p.stdout or "") + ("\n--- stderr ---\n" + p.stderr if p.stderr else "")
    return p.returncode == 0, out


# --------------------------------------------------------------------------
# model invocation, with the failures that actually happen on this machine
# --------------------------------------------------------------------------

def _retry_sleep(attempt: int, kind: str) -> int:
    # macOS on this host wedges DNS system-wide roughly every 2.5 hours and
    # nslookup keeps working throughout, so a network failure here is expected
    # and survivable rather than exceptional. Rate limits on a subscription plan
    # want a much longer wait than a transient socket error.
    base = 900 if kind == "rate" else 60
    return min(base * (2 ** attempt), 3600)


def run_codex(prompt: str, artefact: Path, effort: str,
              resume: str | None = None, attempts: int = 4) -> str | None:
    """One codex turn. Returns the last message, or None if it never landed."""
    out_file = artefact.with_suffix(".out.md")
    log_file = artefact.with_suffix(".raw.log")
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
                p = subprocess.run(argv, input=prompt, text=True, cwd=LIP,
                                   stdout=lf, stderr=subprocess.STDOUT,
                                   timeout=TURN_TIMEOUT)
        except subprocess.TimeoutExpired:
            log(f"  codex TIMEOUT after {TURN_TIMEOUT}s (attempt {attempt+1})")
            continue
        if out_file.exists() and out_file.stat().st_size > 0:
            return out_file.read_text()
        tail = log_file.read_text()[-4000:] if log_file.exists() else ""
        kind = "rate" if RATE_LIMIT_PAT.search(tail) else (
            "net" if NET_FAIL_PAT.search(tail) else "other")
        wait = _retry_sleep(attempt, kind)
        log(f"  codex produced nothing (rc={p.returncode}, kind={kind}); "
            f"sleeping {wait}s")
        time.sleep(wait)
    return None


def run_claude(prompt: str, artefact: Path, st: dict,
               attempts: int = 4) -> tuple[str | None, dict]:
    """One Claude Code implementer turn. Returns (result_text, envelope)."""
    for attempt in range(attempts):
        if STOP_FILE.exists():
            return None, {}
        argv = [CLAUDE, "-p", prompt, "--output-format", "json",
                "--permission-mode", "acceptEdits",
                "--max-turns", "120"]
        if st.get("claude_session"):
            argv += ["--resume", st["claude_session"]]
        try:
            p = subprocess.run(argv, capture_output=True, text=True, cwd=LIP,
                               timeout=CLAUDE_TIMEOUT)
        except subprocess.TimeoutExpired:
            log(f"  claude TIMEOUT (attempt {attempt+1})")
            continue
        artefact.with_suffix(".raw.json").write_text(p.stdout or "")
        try:
            env = json.loads(p.stdout)
        except Exception:
            blob = (p.stdout or "") + (p.stderr or "")
            kind = "rate" if RATE_LIMIT_PAT.search(blob) else "other"
            wait = _retry_sleep(attempt, kind)
            log(f"  claude unparseable output (kind={kind}); sleeping {wait}s")
            time.sleep(wait)
            continue

        if env.get("is_error"):
            blob = json.dumps(env)[:4000]
            kind = "rate" if RATE_LIMIT_PAT.search(blob) else "other"
            wait = _retry_sleep(attempt, kind)
            log(f"  claude is_error (kind={kind}); sleeping {wait}s")
            time.sleep(wait)
            continue

        # Context accounting for rotation. contextWindow is reported per model
        # in the envelope, so fullness is measurable from outside the session
        # and needs no transcript parsing.
        st["claude_session"] = env.get("session_id") or st.get("claude_session")
        u = env.get("usage") or {}
        carried = (u.get("input_tokens", 0) + u.get("cache_read_input_tokens", 0)
                   + u.get("cache_creation_input_tokens", 0))
        st["claude_ctx_tokens"] = carried
        for m in (env.get("modelUsage") or {}).values():
            if m.get("contextWindow"):
                st["claude_ctx_window"] = m["contextWindow"]
        return env.get("result"), env
    return None, {}


def json_block(text: str) -> dict | None:
    """Pull the last fenced json block out of a model reply."""
    if not text:
        return None
    blocks = re.findall(r"```json\s*(.+?)```", text, re.S)
    for b in reversed(blocks):
        try:
            return json.loads(b)
        except Exception:
            continue
    # tolerate a bare object
    m = re.search(r"\{.*\}", text, re.S)
    if m:
        try:
            return json.loads(m.group(0))
        except Exception:
            return None
    return None


# --------------------------------------------------------------------------
# bd -- the durable work queue
# --------------------------------------------------------------------------

def bd(*args: str) -> str:
    p = subprocess.run(["bd", *args], capture_output=True, text=True, cwd=LIP)
    return (p.stdout or "") + (p.stderr or "")


def next_unit() -> str | None:
    out = bd("ready")
    ids = re.findall(r"\b(lip-[0-9a-z]+)\b", out)
    # Skip the epics themselves; they are containers, not work.
    for i in ids:
        if "EPIC" not in bd("show", i).split("\n")[0]:
            return i
    return ids[0] if ids else None


# --------------------------------------------------------------------------
# the mutation-survival test -- how a finding is settled
# --------------------------------------------------------------------------

def mutation_survives(finding: dict, workdir: Path) -> tuple[bool, str]:
    """Apply a proposed mutation to a pristine copy and see if a gate catches it.

    Caught  -> the finding is refuted BY EVIDENCE and closed forever.
    Survives-> the finding is real and becomes work.

    This is the rule that makes the loop converge: a disagreement between two
    models is settled by a test rather than by another round of argument.
    """
    path = finding.get("file")
    old = finding.get("old")
    new = finding.get("new")
    if not (path and old and new):
        return False, "inadmissible: no concrete mutation supplied"

    tree = workdir / "mut"
    if tree.exists():
        shutil.rmtree(tree)
    shutil.copytree(LIP / "go", tree / "go", symlinks=True)
    target = tree / path if (tree / path).exists() else tree / "go" / path
    if not target.exists():
        return False, f"inadmissible: {path} does not exist"
    src = target.read_text()
    if src.count(old) != 1:
        return False, (f"inadmissible: anchor appears {src.count(old)} times in "
                       f"{path}, must appear exactly once")
    target.write_text(src.replace(old, new))

    build = subprocess.run(["go", "build", "./..."], cwd=tree / "go",
                           capture_output=True, text=True,
                           env={**os.environ, "CGO_ENABLED": "0"})
    if build.returncode != 0:
        # A mutation caught only by the compiler tests the Go compiler, not the
        # gate. It is not evidence either way.
        return False, "inadmissible: mutation does not compile"

    test = subprocess.run(
        ["go", "test", "-count=1", "./harness/...", "./cmd/harness/..."],
        cwd=tree / "go", capture_output=True, text=True,
        env={**os.environ, "CGO_ENABLED": "0"})
    if test.returncode != 0:
        failing = re.findall(r"--- FAIL: (\w+)", test.stdout)
        return False, f"CAUGHT by {failing or ['(unnamed)']}"
    return True, "SURVIVED every existing gate"


# --------------------------------------------------------------------------
# prompt assembly
# --------------------------------------------------------------------------

def tmpl(name: str, **kw) -> str:
    t = (PROTO / name).read_text()
    for k, v in kw.items():
        t = t.replace("{{" + k + "}}", str(v))
    return t


# --------------------------------------------------------------------------
# main loop
# --------------------------------------------------------------------------

def main() -> int:
    for d in (STATE_DIR, RUN_DIR, PROTO):
        d.mkdir(parents=True, exist_ok=True)
    st = load_state()
    if st.get("spec_sha") is None:
        st["spec_sha"] = sha(SPEC)
        save_state(st)
    if not LEDGER.exists():
        LEDGER.write_text("# Finding ledger\n\nAppend-only. A finding already "
                          "here is closed on sight without re-argument.\n\n")

    log(f"conductor start; spec pinned at {st['spec_sha'][:16]}")

    while st["iteration"] < MAX_ITERATIONS:
        if STOP_FILE.exists():
            log("STOP file present -- exiting cleanly")
            break

        # The spec is the contract. If it moved, something violated the
        # quarantine and every downstream judgement is suspect.
        if sha(SPEC) != st["spec_sha"]:
            log("FATAL: harness-spec.md CHANGED outside a spec-patch unit. "
                "Stopping. This is the failure mode the quarantine exists for.")
            ledger(f"## {now()} — SPEC MUTATED OUTSIDE QUARANTINE — loop halted")
            break

        st["iteration"] += 1
        it = st["iteration"]
        wd = RUN_DIR / f"{it:04d}"
        wd.mkdir(parents=True, exist_ok=True)
        log(f"=== iteration {it} ===")

        ok, gout = gates(quick=True)
        (wd / "gates-pre.txt").write_text(gout)
        if not ok:
            log("  tree is RED before any work; the only admissible unit is "
                "making it green")

        unit = next_unit()
        if not unit:
            log("  no ready work in bd -- queue exhausted")
            ledger(f"## {now()} — queue exhausted at iteration {it}")
            break
        detail = bd("show", unit)
        (wd / "unit.txt").write_text(detail)
        log(f"  unit {unit}")

        # ---- 1. driver -------------------------------------------------
        d_prompt = tmpl("driver.md", UNIT=unit, DETAIL=detail, GATES=gout)
        (wd / "01-driver.prompt.md").write_text(d_prompt)
        reply = run_codex(d_prompt, wd / "01-driver", EFFORT["driver"],
                          resume=st.get("codex_driver_thread"))
        if reply is None:
            log("  driver turn failed after retries; skipping iteration")
            save_state(st); continue
        directive = json_block(reply) or {"directive": reply, "unit": unit}
        (wd / "01-directive.json").write_text(json.dumps(directive, indent=2))

        # ---- 2. implement ----------------------------------------------
        i_prompt = tmpl("implement.md", UNIT=unit,
                        DIRECTIVE=json.dumps(directive, indent=2))
        (wd / "02-implement.prompt.md").write_text(i_prompt)
        result, env = run_claude(i_prompt, wd / "02-implement", st)
        save_state(st)
        if result is None:
            log("  implement turn failed after retries; skipping iteration")
            continue
        (wd / "02-implement.result.md").write_text(result)

        ok, gout = gates(quick=False)
        (wd / "gates-post.txt").write_text(gout)
        diff = subprocess.run(["git", "diff", "HEAD"], cwd=LIP,
                              capture_output=True, text=True).stdout
        (wd / "02-diff.patch").write_text(diff)
        log(f"  gates after implement: {'GREEN' if ok else 'RED'}; "
            f"diff {len(diff.splitlines())} lines")

        # ---- 3. audit || attack (fresh threads, parallel) --------------
        a_prompt = tmpl("audit.md", DIRECTIVE=json.dumps(directive, indent=2),
                        DIFF=diff[:120000], GATES=gout)
        x_prompt = tmpl("attack.md", DIFF=diff[:120000],
                        LEDGER=LEDGER.read_text()[-20000:])
        (wd / "03-audit.prompt.md").write_text(a_prompt)
        (wd / "04-attack.prompt.md").write_text(x_prompt)
        with ThreadPoolExecutor(max_workers=2) as ex:
            fa = ex.submit(run_codex, a_prompt, wd / "03-audit", EFFORT["audit"])
            fx = ex.submit(run_codex, x_prompt, wd / "04-attack", EFFORT["attack"])
            audit_txt, attack_txt = fa.result(), fx.result()
        audit = json_block(audit_txt or "") or {}
        attack = json_block(attack_txt or "") or {}
        (wd / "03-audit.json").write_text(json.dumps(audit, indent=2))
        (wd / "04-attack.json").write_text(json.dumps(attack, indent=2))

        # ---- 4. settle each finding by mutation, not by argument -------
        verdicts = []
        for f in (attack.get("findings") or [])[:8]:
            survived, why = mutation_survives(f, wd)
            st["findings_raised"] += 1
            if survived:
                st["findings_admitted"] += 1
                nid = bd("q", f"[from attack] {f.get('title','untitled')[:90]}"
                         ).strip()
                verdicts.append({"title": f.get("title"), "verdict": why,
                                 "filed": nid})
            else:
                st["findings_refuted"] += 1
                verdicts.append({"title": f.get("title"), "verdict": why})
            ledger(f"## {now()} — iteration {it} — {f.get('title','untitled')}\n"
                   f"- verdict: **{why}**\n"
                   f"- reachability: {f.get('reachability','(none given)')}\n"
                   f"- mutation: `{f.get('file')}`")
        (wd / "05-verdicts.json").write_text(json.dumps(verdicts, indent=2))
        log(f"  findings: {len(verdicts)} "
            f"({sum(1 for v in verdicts if 'SURVIVED' in v['verdict'])} survived)")

        # ---- 5. adjudicate ---------------------------------------------
        j_prompt = tmpl("adjudicate.md", UNIT=unit, GATES=gout,
                        AUDIT=json.dumps(audit, indent=2),
                        VERDICTS=json.dumps(verdicts, indent=2))
        (wd / "06-adjudicate.prompt.md").write_text(j_prompt)
        adj_txt = run_codex(j_prompt, wd / "06-adjudicate",
                            EFFORT["adjudicate"],
                            resume=st.get("codex_driver_thread"))
        adj = json_block(adj_txt or "") or {"decision": "PARK",
                                            "reason": "adjudication unavailable"}
        (wd / "06-adjudicate.json").write_text(json.dumps(adj, indent=2))
        decision = str(adj.get("decision", "PARK")).upper()
        log(f"  decision: {decision} -- {str(adj.get('reason',''))[:160]}")

        if decision == "ADVANCE" and ok:
            subprocess.run(["git", "add", "-A"], cwd=LIP)
            subprocess.run(
                ["git", "-c", "user.name=loop", "-c", "user.email=noreply@localhost",
                 "commit", "-q", "-m",
                 f"{unit}: {str(adj.get('summary', 'advance'))[:70]}\n\n"
                 f"Loop iteration {it}. Audit: {audit.get('fidelity','?')}/"
                 f"{audit.get('change_safety','?')}. "
                 f"Findings raised {len(verdicts)}, admitted "
                 f"{sum(1 for v in verdicts if 'SURVIVED' in v['verdict'])}."],
                cwd=LIP)
            bd("close", unit)
            st["units_advanced"] += 1
        elif decision == "PARK":
            bd("update", unit, "--append-notes",
               f"PARKED at loop iteration {it}: {adj.get('reason','')}")
            st["units_parked"] += 1
        # REVISE falls through: the same unit is picked up next iteration.

        # ---- 6. staggered rotation -------------------------------------
        st["codex_units_since_rotate"] += 1
        frac = st["claude_ctx_tokens"] / max(st["claude_ctx_window"], 1)
        rotate_claude = frac >= ROTATE_AT_FRACTION
        rotate_codex = st["codex_units_since_rotate"] >= ROTATE_CODEX_EVERY
        if rotate_claude and rotate_codex:
            # Never reboot both in one iteration: one agent must always hold
            # warm context so the other's handoff is checked by a peer that
            # remembers what it was for.
            rotate_codex = False
        if rotate_claude:
            log(f"  rotating Claude session (ctx {frac:.1%} of window)")
            st["claude_session"] = None
            st["claude_ctx_tokens"] = 0
            st["claude_rotations"] += 1
        if rotate_codex:
            log("  rotating codex driver thread")
            st["codex_driver_thread"] = None
            st["codex_units_since_rotate"] = 0
            st["codex_rotations"] += 1

        save_state(st)

    log(f"conductor stop. iterations={st['iteration']} "
        f"advanced={st['units_advanced']} parked={st['units_parked']} "
        f"findings raised/refuted/admitted="
        f"{st['findings_raised']}/{st['findings_refuted']}/{st['findings_admitted']}")
    save_state(st)
    return 0


if __name__ == "__main__":
    sys.exit(main())
