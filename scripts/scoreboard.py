#!/usr/bin/env python3
"""port-spec.md §10.3/§10.4 — the optimisation tournament's scoreboard.

Measures the three lexicographic dimensions and, on the baseline, their NOISE
FLOORS. Without a floor "beat the metric" is unfalsifiable: a 0.4% win on a
metric with 3% run-to-run spread is not a win.

  L0  correctness  gates 1-7. Not scored here -- run negative_control.py and
                   diffdb.py. A change that moves a row loses regardless of
                   anything below.
  L1  throughput   median frames/s over --reps replays of the same tape, plus
      + allocs     allocations, which are near-deterministic and therefore carry
                   a much smaller floor than wall clock.
  L2  live CPU     the GO/PYTHON ratio sampled over the same window, not
      + RSS        absolutes: exchange activity varies by orders of magnitude
                   between rounds and the ratio cancels most of it.

Usage:
    python scoreboard.py --tape T.gz --label baseline --reps 5
    python scoreboard.py --tape T.gz --label baseline --reps 5 --live 120
"""
from __future__ import annotations

import argparse
import re
import statistics
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIP = HERE.parent
GO_SRC = LIP / "go"
GO_BIN = "/usr/local/bin/go"
PY = "/Users/hugh/kek/.venv/bin/python"
BOARD = LIP / "notes" / "scoreboard.md"


def sh(cmd, **kw):
    return subprocess.run(cmd, capture_output=True, text=True, **kw)


# ---------------------------------------------------------------- L1


def build(out: Path) -> None:
    r = sh([GO_BIN, "build", "-o", str(out), "./cmd/replay"], cwd=GO_SRC,
           env={"CGO_ENABLED": "0", "PATH": "/usr/local/bin:/usr/bin:/bin",
                "HOME": str(Path.home())})
    if r.returncode != 0:
        sys.exit(f"build failed:\n{r.stderr}")


def parse_replay(stderr: str) -> dict:
    """Both replayers print the same 'frames N in Ts -> R/s' line."""
    out = {}
    m = re.search(r"frames\s+([\d,]+)\s+in\s+([\d.]+)s\s+->\s+([\d,]+)/s", stderr)
    if m:
        out["frames"] = int(m.group(1).replace(",", ""))
        out["seconds"] = float(m.group(2))
        out["fps"] = float(m.group(3).replace(",", ""))
    m = re.search(r"allocs\s+(\d+)\s+\(([\d.]+)/frame\)\s+total\s+(\d+)\s+MiB", stderr)
    if m:
        out["allocs"] = int(m.group(1))
        out["allocs_per_frame"] = float(m.group(2))
        out["total_mib"] = int(m.group(3))
    m = re.search(r"gc\s+(\d+) cycles\s+total pause ([\d.]+) ms\s+worst pause ([\d.]+) us", stderr)
    if m:
        out["gc_cycles"] = int(m.group(1))
        out["gc_total_ms"] = float(m.group(2))
        out["gc_worst_us"] = float(m.group(3))
    m = re.search(
        r"latency/us\s+p50 ([\d.]+)\s+p90 ([\d.]+)\s+p99 ([\d.]+)\s+"
        r"p99\.9 ([\d.]+)\s+p99\.99 ([\d.]+)\s+max ([\d.]+)\s+mean ([\d.]+)", stderr)
    if m:
        for i, k in enumerate(("p50", "p90", "p99", "p999", "p9999", "max", "mean")):
            out[k] = float(m.group(i + 1))
    return out


def measure_latency(tape: Path, reps: int, binary: Path) -> dict:
    """L1. A SEPARATE pass from --bench: two clock reads per frame and a 9 MiB
    sample buffer perturb both the throughput and the allocation numbers.

    Tail percentiles are single-sample statistics over one tape and are the
    noisiest thing measured here, so every rep is kept and the MEDIAN of each
    percentile is reported -- not the best.
    """
    got = {k: [] for k in ("p50", "p90", "p99", "p999", "p9999", "max")}
    for i in range(reps):
        r = sh([str(binary), "--bench", "--latency", str(tape)])
        if r.returncode != 0:
            sys.exit(f"go latency replay failed:\n{r.stderr}")
        p = parse_replay(r.stderr)
        for k in got:
            got[k].append(p[k])
        print(f"  lat  rep {i+1}/{reps}  p99 {p['p99']:>7.2f}  "
              f"p99.9 {p['p999']:>7.2f}  max {p['max']:>8.2f} us")

    def summarise(xs):
        med = statistics.median(xs)
        spread = (max(xs) - min(xs)) / med * 100 if med else 0.0
        return {"median": med, "min": min(xs), "max": max(xs),
                "floor_pct": spread / 2, "n": len(xs)}

    return {k: summarise(v) for k, v in got.items()}


def measure_l1(tape: Path, reps: int, binary: Path, also_python: bool) -> dict:
    """Replay `reps` times with no DB write, so the metric is the handler."""
    res = {"go_fps": [], "py_fps": []}
    allocs = None
    for i in range(reps):
        r = sh([str(binary), "--bench", str(tape)])
        if r.returncode != 0:
            sys.exit(f"go replay failed:\n{r.stderr}")
        p = parse_replay(r.stderr)
        res["go_fps"].append(p["fps"])
        allocs = p  # deterministic enough that the last is representative
        print(f"  go   rep {i+1}/{reps}  {p['fps']:>9,.0f} f/s  "
              f"{p.get('allocs_per_frame', 0):>6.2f} allocs/frame  "
              f"{p.get('gc_cycles', 0):>4} gc")
    if also_python:
        for i in range(reps):
            r = sh([PY, str(LIP / "replay.py"), str(tape), "--bench"])
            if r.returncode != 0:
                sys.exit(f"python replay failed:\n{r.stderr}")
            p = parse_replay(r.stderr)
            res["py_fps"].append(p["fps"])
            print(f"  py   rep {i+1}/{reps}  {p['fps']:>9,.0f} f/s")

    def summarise(xs):
        if not xs:
            return None
        med = statistics.median(xs)
        # The noise floor: half the observed spread, as a percentage of the
        # median. An improvement must exceed this to count (§10.4).
        spread = (max(xs) - min(xs)) / med * 100 if med else 0.0
        return {"median": med, "min": min(xs), "max": max(xs),
                "floor_pct": spread / 2, "n": len(xs)}

    return {"go": summarise(res["go_fps"]), "py": summarise(res["py_fps"]),
            "allocs": allocs, "frames": allocs.get("frames") if allocs else None}


# ---------------------------------------------------------------- L2


def pids(pattern: str) -> list[int]:
    r = sh(["pgrep", "-f", pattern])
    return [int(x) for x in r.stdout.split()] if r.returncode == 0 else []


def sample(pid: int) -> tuple[float, float] | None:
    """(cpu_percent, rss_mib) for one pid, or None if it has gone."""
    r = sh(["ps", "-o", "%cpu=,rss=", "-p", str(pid)])
    if r.returncode != 0 or not r.stdout.strip():
        return None
    cpu, rss = r.stdout.split()
    return float(cpu), float(rss) / 1024.0


def measure_l2(go_pid: int, py_pid: int, seconds: int, interval: int = 5) -> dict:
    """Sample both rigs over the SAME window.

    ps %cpu is an average over process lifetime, not an instantaneous rate, so
    the first sample is discarded and the metric is taken from the deltas in
    cumulative CPU time instead.
    """
    def cputime(pid: int) -> float | None:
        r = sh(["ps", "-o", "time=", "-p", str(pid)])
        if r.returncode != 0 or not r.stdout.strip():
            return None
        parts = r.stdout.strip().replace("-", ":").split(":")
        parts = [float(p) for p in parts]
        secs = 0.0
        for p in parts:
            secs = secs * 60 + p
        return secs

    t0 = {"go": cputime(go_pid), "py": cputime(py_pid)}
    if t0["go"] is None or t0["py"] is None:
        sys.exit("one of the rigs is not running; L2 needs both, simultaneously")

    rss = {"go": [], "py": []}
    waited = 0
    while waited < seconds:
        time.sleep(interval)
        waited += interval
        for name, pid in (("go", go_pid), ("py", py_pid)):
            s = sample(pid)
            if s:
                rss[name].append(s[1])
        print(f"  live {waited:>4}/{seconds}s  "
              f"go rss {rss['go'][-1] if rss['go'] else 0:>7.1f} MiB   "
              f"py rss {rss['py'][-1] if rss['py'] else 0:>7.1f} MiB")

    t1 = {"go": cputime(go_pid), "py": cputime(py_pid)}
    if t1["go"] is None or t1["py"] is None:
        sys.exit("a rig exited during the window; the sample is not comparable")

    go_cpu = (t1["go"] - t0["go"]) / waited * 100
    py_cpu = (t1["py"] - t0["py"]) / waited * 100
    return {
        "window_s": waited,
        "go_cpu_pct": go_cpu, "py_cpu_pct": py_cpu,
        "cpu_ratio": go_cpu / py_cpu if py_cpu else None,
        "go_rss_mib": statistics.median(rss["go"]) if rss["go"] else None,
        "py_rss_mib": statistics.median(rss["py"]) if rss["py"] else None,
        "rss_ratio": (statistics.median(rss["go"]) / statistics.median(rss["py"])
                      if rss["go"] and rss["py"] else None),
    }


# ---------------------------------------------------------------- report


def append_row(label: str, note: str, lat: dict | None, l1: dict, l2: dict | None) -> None:
    if not BOARD.exists():
        BOARD.write_text(
            "# Optimisation tournament scoreboard\n"
            "\n"
            "Generated by `scripts/scoreboard.py`. Rules are in `port-spec.md` §10.\n"
            "\n"
            "The dimensions are LEXICOGRAPHIC, highest priority first:\n"
            "\n"
            "    L0  correctness      gates 1-7, byte-identical rows. Never traded.\n"
            "    L1  tail latency     p99 / p99.9 / max of per-frame rig.Handle\n"
            "    L2  allocations      allocs/frame, total allocated\n"
            "    L3  throughput       median frames/s\n"
            "    L4  live CPU + RSS   go/python ratio, same window\n"
            "\n"
            "A round is won at the highest level where the challenger improves, and\n"
            "only if no higher level regresses beyond its noise floor. Throughput is\n"
            "third because the live feed runs at ~390 frames/s against a replay that\n"
            "handles 87,000/s — ~220x headroom — while the tail is what Phase 2's\n"
            "quoting bot actually pays for.\n"
            "\n"
            "Latency figures are the MEDIAN across reps, not the best. Allocations\n"
            "have a zero noise floor and corroborate the noisy tail.\n"
            "\n"
            "| round | p99 us | p99.9 us | max us | allocs/frame | gc | f/s | "
            "cpu ratio | rss ratio | note |\n"
            "|---|---|---|---|---|---|---|---|---|---|\n")
    go = l1["go"]
    al = l1["allocs"] or {}

    def lat_cell(key):
        if not lat or key not in lat:
            return "—"
        d = lat[key]
        return f"{d['median']:.2f} ±{d['floor_pct']:.1f}%"

    cells = [
        label,
        lat_cell("p99"),
        lat_cell("p999"),
        lat_cell("max"),
        f"{al.get('allocs_per_frame', 0):.2f}",
        f"{al.get('gc_cycles', 0):,}",
        f"{go['median']:,.0f} ±{go['floor_pct']:.1f}%" if go else "—",
        f"{l2['cpu_ratio']:.3f}" if l2 and l2.get("cpu_ratio") else "—",
        f"{l2['rss_ratio']:.3f}" if l2 and l2.get("rss_ratio") else "—",
        note or "",
    ]
    with BOARD.open("a") as fh:
        fh.write("| " + " | ".join(cells) + " |\n")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tape", type=Path, required=True)
    ap.add_argument("--label", required=True, help="round name, e.g. 'baseline' or 'r1-codex'")
    ap.add_argument("--note", default="", help="what changed this round")
    ap.add_argument("--reps", type=int, default=5)
    # replay.py is FROZEN, so its number cannot move between rounds and
    # re-measuring it is pure waste. It is worth exactly one measurement, at
    # baseline: it fixes the "beat the thing we ported from" reference, and its
    # own spread is an independent estimate of machine noise (if Go and Python
    # both scatter by the same margin, that is the laptop, not the code).
    ap.add_argument("--with-python", action="store_true",
                    help="also replay through Python; only useful at baseline")
    ap.add_argument("--live", type=int, default=0,
                    help="seconds to sample L2; needs both rigs already running")
    ap.add_argument("--go-pid", type=int, default=0)
    ap.add_argument("--py-pid", type=int, default=0)
    a = ap.parse_args()

    binary = Path("/tmp/scoreboard-replay")
    print(f"building {binary}")
    build(binary)

    print(f"\nL1  tail latency  ({a.reps} reps, separate pass)")
    lat = measure_latency(a.tape, a.reps, binary)
    print()
    for k, name in (("p50", "p50"), ("p99", "p99"), ("p999", "p99.9"),
                    ("p9999", "p99.99"), ("max", "max")):
        d = lat[k]
        print(f"  {name:>7}  {d['median']:>8.2f} us   floor ±{d['floor_pct']:.2f}%"
              f"   (min {d['min']:.2f} max {d['max']:.2f})")

    print(f"\nL2/L3  allocations + throughput  ({a.reps} reps)")
    l1 = measure_l1(a.tape, a.reps, binary, a.with_python)
    go, py = l1["go"], l1["py"]
    print(f"\n  go median {go['median']:>10,.0f} f/s   floor ±{go['floor_pct']:.2f}%"
          f"   (min {go['min']:,.0f} max {go['max']:,.0f})")
    if py:
        print(f"  py median {py['median']:>10,.0f} f/s   floor ±{py['floor_pct']:.2f}%")
        print(f"  go/py     {go['median'] / py['median']:>10.3f}")
    al = l1["allocs"] or {}
    print(f"  allocs    {al.get('allocs', 0):>10,}  "
          f"({al.get('allocs_per_frame', 0):.2f}/frame, {al.get('total_mib', 0):,} MiB total)")

    l2 = None
    if a.live:
        go_pid = a.go_pid or (pids("rigbin|cmd/rig") or [0])[0]
        py_pid = a.py_pid or (pids(r"rig\.py") or [0])[0]
        if not go_pid or not py_pid:
            sys.exit(f"L2 needs both rigs running (go_pid={go_pid} py_pid={py_pid})")
        print(f"\nL2  live CPU + RSS  ({a.live}s, go pid {go_pid} vs py pid {py_pid})")
        l2 = measure_l2(go_pid, py_pid, a.live)
        print(f"\n  go  cpu {l2['go_cpu_pct']:.2f}%  rss {l2['go_rss_mib']:.1f} MiB")
        print(f"  py  cpu {l2['py_cpu_pct']:.2f}%  rss {l2['py_rss_mib']:.1f} MiB")
        print(f"  ratio   cpu {l2['cpu_ratio']:.3f}   rss {l2['rss_ratio']:.3f}"
              f"   (lower is better for Go)")

    append_row(a.label, a.note, lat, l1, l2)
    stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    print(f"\nappended round '{a.label}' to {BOARD.relative_to(LIP)}  ({stamp})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
