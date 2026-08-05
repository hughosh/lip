#!/usr/bin/env python3
"""Gate 7 of notes/port-spec.md: the gate on the gate.

There is no human code review on this port, so gates 1-6 are the entire safety
argument -- and a gate that has never been shown to fail is not evidence. This
deliberately breaks the Go, one preservation rule at a time, and checks that the
differential gate NOTICES.

Any mutation that survives every gate is a defect in the ORACLE, not a curiosity.
The port does not proceed until the gate is strengthened to catch it.

Each mutation is applied to a pristine copy of lip/go, so runs cannot
contaminate each other or the working tree. A replacement that fails to apply is
a hard error: a no-op mutation would otherwise be silently recorded as
"survived", which is the exact false-negative this script exists to prevent.

Usage:
    python negative_control.py [--out ../notes/negative-control.md]
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIP = HERE.parent
GO_SRC = LIP / "go"
GO_BIN = "/usr/local/bin/go"

spec = importlib.util.spec_from_file_location("diffdb", HERE / "diffdb.py")
diffdb = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diffdb)


# (id, rule, relative path, old, new, expected gate)
#
# `old` must appear EXACTLY ONCE in the file.
MUTATIONS = [
    ("m01", "P1  banker's rounding", "core/num.go",
     "return int(math.RoundToEven(f * 100)), nil",
     "return int(math.Round(f * 100)), nil",
     "gate 2"),

    # Expected to SURVIVE, and that is not an oracle defect. P24's compensated
    # summation is order-independent for every input reachable here, and Sum is
    # the only consumer of insertion order, so this mutation is behaviourally
    # inert. Kept in the suite because "we checked, and it genuinely cannot
    # matter" is a finding worth re-verifying whenever the summation changes.
    ("m02", "P2  insertion-ordered book", "core/levels.go",
     "\tfor _, x := range l.vals {\n\t\tt := f + x",
     "\tfor i := len(l.vals) - 1; i >= 0; i-- {\n\t\tx := l.vals[i]\n\t\tt := f + x",
     "inert"),

    # Written to keep `math` used, so this tests the SEMANTICS rather than
    # tripping the compiler over an unused import.
    ("m03", "P24 compensated summation", "core/levels.go",
     "\t\tt := f + x\n\t\tif math.Abs(f) >= math.Abs(x) {\n"
     "\t\t\tc += (f - t) + x\n\t\t} else {\n\t\t\tc += (x - t) + f\n\t\t}\n\t\tf = t",
     "\t\t_ = math.Abs(x)\n\t\tf = f + x",
     "gate 3"),

    ("m04", "P3  state_before tie-break", "core/book.go",
     "if h.ts < ts && (chosen == nil || h.ts >= chosen.ts) {",
     "if h.ts < ts && (chosen == nil || h.ts > chosen.ts) {",
     "gate 4"),

    ("m05", "P4  state_before early break", "core/book.go",
     "\t\tif h.ts < ts && (chosen == nil || h.ts >= chosen.ts) {\n\t\t\tchosen = h\n\t\t}",
     "\t\tif h.ts >= ts {\n\t\t\tbreak\n\t\t}\n\t\tif chosen == nil || h.ts >= chosen.ts {\n\t\t\tchosen = h\n\t\t}",
     "gate 3"),

    # Floor rather than truncate-toward-zero, expressed without importing math
    # so the mutation is semantic rather than a compile error.
    ("m06", "P7  book_lag_ms truncation", "core/rig.go",
     "v := int64(*lag * 1000)",
     "v := int64(*lag * 1000)\n\t\tif float64(v) > *lag*1000 {\n\t\t\tv--\n\t\t}",
     "gate 4"),

    ("m07", "P9  qualifying walk clearing", "core/book.go",
     "\t\tif !reached {\n\t\t\treturn 0\n\t\t}",
     "\t\t_ = reached",
     "gate 3"),

    ("m08", "P10 delta epsilon strictness", "core/book.go",
     "if new > 1e-9 {",
     "if new >= 1e-9 {",
     "gate 4"),

    ("m09", "P12 fill conflict clause", "store/store.go",
     "`INSERT OR IGNORE INTO fill (trade_id, ts_ms, ticker, resting_side,",
     "`INSERT OR REPLACE INTO fill (trade_id, ts_ms, ticker, resting_side,",
     "gate 4"),

    ("m10", "P15 watermark/stale asymmetry", "core/rig.go",
     "\tif _, quarantined := r.stale[m.MarketTicker]; quarantined {\n\t\treturn nil\n\t}",
     "\tr.watermark(m.TsMs)\n\tif _, quarantined := r.stale[m.MarketTicker]; quarantined {\n\t\treturn nil\n\t}",
     "gate 4"),

    # --- P2 bookkeeping, as opposed to m02's Sum-consumption ----------------
    #
    # m02 leaves the ordered bookkeeping intact and only changes which order Sum
    # reads, which is inert. These two break the bookkeeping ITSELF, and they
    # compile, so they test whether the ordering is genuinely defended or was
    # only ever protected by a test that would fail to build.

    ("m11", "P2  overwrite keeps position", "core/levels.go",
     "\tif i := l.index(price); i >= 0 {\n"
     "\t\t// Existing key: the value is replaced and the POSITION IS UNCHANGED.\n"
     "\t\tl.vals[i] = size\n\t\treturn\n\t}",
     "\tl.Delete(price)",
     "gate 3"),

    ("m12", "P2  delete preserves order", "core/levels.go",
     "\tl.keys = append(l.keys[:i], l.keys[i+1:]...)\n"
     "\tl.vals = append(l.vals[:i], l.vals[i+1:]...)",
     "\tlast := len(l.keys) - 1\n"
     "\tl.keys[i], l.vals[i] = l.keys[last], l.vals[last]\n"
     "\tl.keys, l.vals = l.keys[:last], l.vals[:last]",
     "gate 3"),

    # NEW with move A1. Two parallel slices can desync, which a slice-plus-maps
    # representation could not do: the old Delete removed from one map or the
    # other and a mismatch was structurally impossible. Deleting the key but not
    # the value leaves Sum totalling a level that is no longer in the book.
    ("m24", "A1 parallel slices stay in step", "core/levels.go",
     "\tl.keys = append(l.keys[:i], l.keys[i+1:]...)\n"
     "\tl.vals = append(l.vals[:i], l.vals[i+1:]...)",
     "\tl.keys = append(l.keys[:i], l.keys[i+1:]...)",
     "gate 3"),

    # --- P25 reconnect reset -----------------------------------------------
    #
    # No tape contains a reconnect, so the differential replay is structurally
    # blind to both of these. If gate 3 does not catch them, nothing does.

    ("m13", "P25 seq map cleared on reconnect", "core/rig.go",
     "\tr.seq = map[int64]int64{}\n\tr.seqOrder = nil\n\tr.NeedsResnapshot = false",
     "\tr.NeedsResnapshot = false",
     "gate 3"),

    ("m14", "P25 ref/gate reset on reconnect", "core/rig.go",
     "\t\tb.RefYes, b.RefNo, b.Gate = nil, nil, nil\n",
     "",
     "gate 3"),

    # --- P26 tape format ----------------------------------------------------

    ("m15", "P26 frame bytes unmodified", "tape/write.go",
     "\tb.Write(pyStrip(frame))",
     "\tb.Write(append([]byte(\" \"), pyStrip(frame)...))",
     "gate 3"),

    ("m16", "P26 trailing newline stripped", "tape/write.go",
     "\tb.Write(pyStrip(frame))",
     "\tb.Write(frame)",
     "gate 3"),

    # str.strip() strips U+001C-U+001F; bytes.TrimSpace does not.
    ("m25", "P26 Python strip character set", "tape/write.go",
     "\t\tcase '\\t', '\\n', '\\v', '\\f', '\\r', ' ',\n\t\t\t0x1c, 0x1d, 0x1e, 0x1f:",
     "\t\tcase '\\t', '\\n', '\\v', '\\f', '\\r', ' ':",
     "gate 3"),

    # --- P27 subscription shape ---------------------------------------------
    #
    # This one changes NO fill row, which is the whole reason it is dangerous:
    # only a named test can object to it.

    ("m17", "P27 trade tape unfiltered", "feed/feed.go",
     "\t\t\"params\": map[string]any{\"channels\": []string{\"trade\"}},",
     "\t\t\"params\": map[string]any{\"channels\": []string{\"trade\"},\n"
     "\t\t\t\"market_tickers\": []string{}},",
     "gate 3"),

    # --- P28 resolver quirks -------------------------------------------------

    ("m18", "P28 pending deleted unconditionally", "store/store.go",
     "\t\tif m := mid(d.Ticker); m != nil {\n"
     "\t\t\tif err := s.SetMid(d.TradeID, d.Horizon, *m); err != nil {\n"
     "\t\t\t\treturn 0, err\n\t\t\t}\n\t\t}\n"
     "\t\tif err := s.DeletePending(d.TradeID, d.Horizon); err != nil {\n"
     "\t\t\treturn 0, err\n\t\t}",
     "\t\tif m := mid(d.Ticker); m != nil {\n"
     "\t\t\tif err := s.SetMid(d.TradeID, d.Horizon, *m); err != nil {\n"
     "\t\t\t\treturn 0, err\n\t\t\t}\n"
     "\t\t\tif err := s.DeletePending(d.TradeID, d.Horizon); err != nil {\n"
     "\t\t\t\treturn 0, err\n\t\t\t}\n\t\t}",
     "gate 3"),

    ("m19", "P28 no commit when nothing due", "store/store.go",
     "\tif len(due) == 0 {\n\t\treturn 0, nil\n\t}",
     "\tif len(due) == 0 {\n\t\treturn 0, s.Commit()\n\t}",
     "gate 3"),

    # --- rules added by the 2026-07-24 codex round -------------------------
    #
    # All four are invisible to the differential replay: no tape contains a
    # close, a signal, or a wall clock. They exist because codex found the
    # originals wrong, so they are exactly the rules most worth defending.

    ("m20", "P25a clean close resets state", "feed/feed.go",
     "\tswitch websocket.CloseStatus(err) {\n"
     "\tcase websocket.StatusNormalClosure, websocket.StatusGoingAway:\n"
     "\t\treturn true\n\t}\n\treturn false",
     "\treturn false",
     "gate 3"),

    ("m21", "clock: Python float ms conversion", "cmd/rig/main.go",
     "\treturn int64(pyTimeSeconds(sec*1_000_000_000+int64(nsec)) * 1000)",
     "\treturn (sec*1_000_000_000 + int64(nsec)) / 1_000_000",
     "gate 3"),

    # CPython converts ONE int64 ns to double; converting seconds and nanoseconds
    # separately and adding them disagrees in the last ulp, in both directions.
    ("m26", "clock: single-int64 conversion", "cmd/rig/main.go",
     "\tif ns%secToNs == 0 {\n\t\treturn float64(ns / secToNs)\n\t}\n"
     "\treturn float64(ns) / float64(secToNs)",
     "\treturn float64(ns/secToNs) + float64(ns%secToNs)/float64(secToNs)",
     "gate 3"),

    # A credentials file with CR line endings must still parse.
    ("m27", "auth: Python splitlines boundaries", "feed/auth.go",
     "\tfor _, line := range pySplitlines(string(raw)) {",
     "\tfor _, line := range strings.Split(string(raw), \"\\n\") {",
     "gate 3"),

    ("m22", "tape header is Python json.dumps", "tape/write.go",
     '\tb.WriteString(`{"universe": {`)',
     '\tb.WriteString(`{"universe":{`)',
     "gate 3"),

    # A2's hazards. The qualifying walk now sorts (price,size) pairs into a
    # REUSED buffer, so two new ways to be wrong exist that did not before:
    # forgetting to truncate it, and sorting the wrong way. P9's running total is
    # naive addition, so the walk order is load-bearing.
    ("m29", "A2 qualifying walk is descending", "core/levels.go",
     "\tslices.SortFunc(buf, func(a, b level) int { return cmp.Compare(b.price, a.price) })",
     "\tslices.SortFunc(buf, func(a, b level) int { return cmp.Compare(a.price, b.price) })",
     "gate 3"),

    ("m30", "A2 scratch buffer is truncated", "core/levels.go",
     "\tbuf = buf[:0]\n\tfor i, p := range l.keys {",
     "\tfor i, p := range l.keys {",
     "gate 3"),

    # P29, added during the §10 tournament. Move B1 recycled checkpoint storage
    # and returned pointers INTO the ring, so a book already handed to a caller
    # mutated once the ring wrapped. It passed the differential gate
    # byte-identically: recordTrade consumes StateBefore's result inside the same
    # Handle call, so no tape can reach the aliasing. Found by reading the diff.
    ("m28", "P29 StateBefore result is detached", "core/book.go",
     "\tl := ts - chosen.ts\n\treturn chosen.yes.Clone(), chosen.no.Clone(), &l",
     "\tl := ts - chosen.ts\n\treturn &chosen.yes, &chosen.no, &l",
     "gate 3"),

    ("m23", "SIGTERM discards the open transaction", "store/store.go",
     "func (s *Store) Discard() error {\n\tif s.tx != nil {\n\t\ts.tx.Rollback()\n\t}\n\treturn s.db.Close()\n}",
     "func (s *Store) Discard() error {\n\treturn s.Close()\n}",
     "gate 3"),
]


def run(cmd, cwd=None, timeout=900):
    return subprocess.run(cmd, cwd=cwd, capture_output=True, text=True,
                          timeout=timeout, env={"CGO_ENABLED": "0",
                                                "PATH": "/usr/local/bin:/usr/bin:/bin",
                                                "HOME": str(Path.home())})


def stage(work: Path) -> Path:
    """Copy the source tree into an isolated dir and return the Go root.

    testdata/ must come too: the fixture tests resolve `../../testdata`
    relative to the package directory, and without it EVERY fixture test fails
    in EVERY copy -- which would make gate 2/3 appear to catch every mutation
    while actually catching none. Only the .tsv fixtures are copied; the gate
    tape is 12MB and is passed in by path.
    """
    tree = work / "go"
    shutil.copytree(GO_SRC, tree)
    td = work / "testdata"
    td.mkdir()
    for f in (LIP / "testdata").glob("*.tsv"):
        shutil.copy2(f, td / f.name)
    return tree


def apply_mutation(tree: Path, rel: str, old: str, new: str) -> None:
    path = tree / rel
    text = path.read_text()
    n = text.count(old)
    if n != 1:
        raise SystemExit(
            f"FATAL: anchor for {rel} matched {n} times, expected exactly 1.\n"
            f"  A mutation that does not apply would be recorded as 'survived',\n"
            f"  which is precisely the false negative this script prevents.\n"
            f"  anchor: {old!r}")
    path.write_text(text.replace(old, new))


def evaluate(tree: Path, tape: Path, ref_db: Path, work: Path) -> tuple[str | None, str]:
    """Run gates 1-6 against a mutated tree. Returns (catching gate, detail)."""
    binary = work / "replay"

    r = run([GO_BIN, "build", "-o", str(binary), "./cmd/replay"], cwd=tree)
    if r.returncode != 0:
        return "gate 1", "build failed: " + r.stderr.strip().splitlines()[-1][:160]

    r = run([GO_BIN, "vet", "./..."], cwd=tree)
    if r.returncode != 0:
        return "gate 1", "vet failed: " + r.stderr.strip().splitlines()[-1][:160]

    # The whole module, not just ./core/: the conflict-clause rules live in
    # store/ and would go unchecked otherwise.
    r = run([GO_BIN, "test", "./..."], cwd=tree)
    if r.returncode != 0:
        failed = [ln.split()[2].rstrip(":") for ln in r.stdout.splitlines()
                  if ln.strip().startswith("--- FAIL:")]
        return "gate 2/3", f"{len(failed)} unit test(s) failed: {', '.join(failed[:3])}"

    go_db = work / "go.db"
    if go_db.exists():
        go_db.unlink()
    r = run([str(binary), "--out", str(go_db), str(tape)])
    if r.returncode != 0:
        return "gate 4", "replay crashed: " + r.stderr.strip().splitlines()[-1][:160]

    bad, detail = diff_counts(ref_db, go_db)
    if bad:
        return ("gate 5" if detail.startswith("reference") else "gate 4"), detail
    return None, "SURVIVED every gate"


def diff_counts(ref_db: Path, go_db: Path) -> tuple[int, str]:
    """Compare without printing; returns (discrepancies, human summary)."""
    parts = []
    total = 0
    for table, key, cols in (
        ("fill", [diffdb.FILL_KEY], diffdb.FILL_COLS),
        ("reference", diffdb.REF_KEY, diffdb.REF_COLS),
    ):
        a = diffdb.load(str(ref_db), table, key, cols)
        b = diffdb.load(str(go_db), table, key, cols)
        shared = a.keys() & b.keys()
        per_col = {}
        for k in shared:
            for c in cols:
                if a[k][c] != b[k][c]:
                    per_col[c] = per_col.get(c, 0) + 1
        missing = len(a.keys() ^ b.keys())
        n = sum(per_col.values()) + missing
        total += n
        if n:
            bits = [f"{c}={v}" for c, v in sorted(per_col.items(),
                                                  key=lambda kv: -kv[1])[:4]]
            if missing:
                bits.append(f"row-count-delta={missing}")
            parts.append(f"{table}: {n} rows differ ({', '.join(bits)})")
    return total, "; ".join(parts)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tape", type=Path, required=True,
                    help="clean tape WITH a universe header, so targets are non-zero")
    ap.add_argument("--ref-db", type=Path, required=True,
                    help="Python replay DB for that tape (the oracle)")
    ap.add_argument("--out", type=Path, default=LIP / "notes" / "negative-control.md")
    a = ap.parse_args()

    print(f"tape   {a.tape}\noracle {a.ref_db}\n")

    # Control for the control. An UNMUTATED copy must pass every gate. If it
    # does not, the harness itself is broken and every "caught" below would be a
    # false positive -- which is exactly what a missing testdata/ copy produced
    # on the first run of this script.
    with tempfile.TemporaryDirectory(prefix="negctl-control-") as td:
        work = Path(td)
        caught, detail = evaluate(stage(work), a.tape, a.ref_db, work)
    if caught is not None:
        raise SystemExit(
            f"FATAL: the UNMUTATED tree failed {caught}: {detail}\n"
            f"  Every mutation would appear 'caught' for this reason rather than\n"
            f"  its own. Fix the harness before trusting any result below.")
    print("  control  unmutated tree passes every gate\n")

    results = []
    for mid, rule, rel, old, new, expected in MUTATIONS:
        with tempfile.TemporaryDirectory(prefix=f"negctl-{mid}-") as td:
            work = Path(td)
            tree = stage(work)
            apply_mutation(tree, rel, old, new)
            caught, detail = evaluate(tree, a.tape, a.ref_db, work)

        inert = expected == "inert"
        if caught is not None:
            mark = "caught"
        elif inert:
            mark = "inert (expected)"
        else:
            mark = "*** SURVIVED ***"
        print(f"  {mid}  {rule:34s} {mark:16s} {caught or '-':9s} {detail[:90]}")
        results.append({"id": mid, "rule": rule, "file": rel, "expected": expected,
                        "caught_by": caught, "detail": detail, "inert": inert})

    # A mutation that survives is blocking UNLESS it is declared inert -- i.e.
    # we have positively established it cannot change any output. That
    # distinction has to be argued in the spec, never assumed here.
    survived = [r for r in results if r["caught_by"] is None and not r["inert"]]
    inert_ok = [r for r in results if r["caught_by"] is None and r["inert"]]
    inert_caught = [r for r in results if r["caught_by"] is not None and r["inert"]]

    lines = [
        "# Gate 7 — negative control",
        "",
        "Generated by `scripts/negative_control.py`. Each row is a deliberate",
        "violation of one preservation rule from `port-spec.md` §4, applied to a",
        "pristine copy of `lip/go`, then run through gates 1-6.",
        "",
        f"Tape: `{a.tape.name}`  ·  oracle: `{a.ref_db.name}`",
        "",
        f"**{len([r for r in results if r['caught_by']])} of {len(results)} mutations caught"
        + (f", {len(inert_ok)} declared behaviourally inert.**" if inert_ok else ".**"),
        "",
        "| id | rule violated | file | expected | caught by | what the gate saw |",
        "|---|---|---|---|---|---|",
    ]
    for r in results:
        if r["caught_by"]:
            got = r["caught_by"]
        elif r["inert"]:
            got = "_inert, as expected_"
        else:
            got = "**SURVIVED**"
        lines.append(f"| {r['id']} | {r['rule']} | `{r['file']}` | {r['expected']} | "
                     f"{got} | {r['detail'][:120]} |")
    lines += ["", "## Reading this table", "",
              "A mutation caught by an *earlier* gate than expected is fine — the gates",
              "are ordered cheapest-first and catching a fault sooner is strictly better.",
              "A mutation caught by a *later* gate than expected means the cheap gate has",
              "a hole worth closing.", ""]
    if survived:
        lines += ["## BLOCKING", "",
                  "These mutations survived every gate. The oracle cannot detect them, so",
                  "passing gates 1-6 does not rule them out and the port is NOT ready:", ""]
        lines += [f"- **{r['id']} {r['rule']}** (`{r['file']}`)" for r in survived]
        lines.append("")
    else:
        lines += ["## Result", "",
                  "Every non-inert mutation was caught. Gates 1-6 have demonstrated",
                  "failure modes, so passing them is evidence rather than an absence of",
                  "evidence.", ""]
    if inert_ok:
        lines += ["## Declared inert", "",
                  "These mutations survived every gate, and that is the CORRECT outcome:",
                  "they cannot change any output, so there is nothing for the oracle to",
                  "detect. Each needs a positive argument in `port-spec.md`, never an",
                  "assumption — an inert declaration is how a real oracle hole would hide.",
                  ""]
        lines += [f"- **{r['id']} {r['rule']}** (`{r['file']}`)" for r in inert_ok]
        lines.append("")
    if inert_caught:
        lines += ["## Inert declarations that turned out to be wrong", "",
                  "These were declared inert but the gate caught them anyway, so they DO",
                  "affect output. Remove the inert declaration and treat the rule as",
                  "load-bearing.", ""]
        lines += [f"- **{r['id']} {r['rule']}**: {r['detail'][:160]}" for r in inert_caught]
        lines.append("")

    a.out.write_text("\n".join(lines))
    print(f"\nwrote {a.out}")
    print(json.dumps({"caught": len(results) - len(survived), "total": len(results)}))
    return 1 if survived else 0


if __name__ == "__main__":
    raise SystemExit(main())
