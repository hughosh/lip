"""Mutation tests for settle.py.

Every assertion here works by breaking a copy of the real probe.db in a
specific way and checking that settle.py notices. A test that passes on
unmutated data proves nothing, so each case states the mutation and the
symptom it must produce.

Run from /Users/hugh/kek/lip:  python scripts/settletest.py
"""
from __future__ import annotations

import os
import shutil
import sqlite3
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
import settle  # noqa: E402

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
                   "probe.db")

fails: list[str] = []
n = 0


def check(name: str, got, want) -> None:
    global n
    n += 1
    ok = got == want
    if isinstance(want, float) and isinstance(got, (int, float)):
        ok = abs(got - want) < 1e-6
    print(f"  {'ok  ' if ok else 'FAIL'}  {name}: got {got!r} want {want!r}")
    if not ok:
        fails.append(name)


def approx(name: str, got: float, want: float, tol: float) -> None:
    global n
    n += 1
    ok = abs(got - want) <= tol
    print(f"  {'ok  ' if ok else 'FAIL'}  {name}: got {got:.6f} "
          f"want {want:.6f} +-{tol}")
    if not ok:
        fails.append(name)


class Fixture:
    """A writable copy of probe.db, plus the run row it belongs to."""

    def __init__(self) -> None:
        self.dir = tempfile.mkdtemp(prefix="settletest-")
        self.path = os.path.join(self.dir, "probe.db")
        # The bot is writing to probe.db at 1 Hz. Use the online backup API
        # rather than a file copy so every fixture is a consistent snapshot
        # and cannot pick up a half-written transaction as a fake gap.
        src = sqlite3.connect(f"file:{SRC}?mode=ro", uri=True)
        dst = sqlite3.connect(self.path)
        src.backup(dst)
        dst.commit()
        src.close()
        self.rid = dst.execute(
            "SELECT run_id FROM run ORDER BY started_ms DESC LIMIT 1"
        ).fetchone()[0]
        dst.close()

    def w(self) -> sqlite3.Connection:
        return sqlite3.connect(self.path)

    def ro(self) -> sqlite3.Connection:
        return settle.connect(self.path)

    def close(self) -> None:
        shutil.rmtree(self.dir, ignore_errors=True)


def snap_span(f: Fixture) -> tuple[int, int]:
    c = f.ro()
    r = c.execute("SELECT MIN(ts_ms), MAX(ts_ms) FROM snap WHERE run_id=?",
                  (f.rid,)).fetchone()
    return r[0], r[1]


# ---------------------------------------------------------------- baseline

print("baseline: unmutated copy agrees with the bot's own accounting")
f = Fixture()
c0 = settle.counters(f.ro(), f.rid)
g0 = settle.classify_gaps(f.ro(), f.rid, settle.find_gaps(f.ro(), f.rid))
# probe.db is live and grows while these tests run, and real sub-second
# scheduling hiccups do occur, so every gap assertion below is relative to
# whatever the baseline happens to be rather than to zero.
BASE = len(g0)
print(f"        baseline gaps in the live db: {BASE} "
      f"({sum(x['seconds'] for x in g0):.1f}s total)")


def gap_near(gaps: list[dict], t_ms: int, tol_ms: int = 5000) -> dict | None:
    """The injected gap, identified by where it starts."""
    near = [g for g in gaps if abs(g["start_ms"] - t_ms) <= tol_ms]
    return max(near, key=lambda g: g["seconds"]) if near else None
# Independent oracle: recompute mean share straight from the rows, no SQL
# aggregates, and compare with the counters() path.
rows = f.ro().execute(
    "SELECT gated, two_sided, share FROM snap WHERE run_id=?", (f.rid,)
).fetchall()
man_gated = sum(1 for r in rows if r["gated"])
man_share = sum(r["share"] for r in rows if r["gated"])
man_present = sum(1 for r in rows if r["gated"] and r["two_sided"])
check("observed_gated matches row-by-row count", c0["observed_gated"], man_gated)
approx("integrated_share matches row-by-row sum",
       c0["integrated_share"], man_share, 1e-6)
check("present matches row-by-row count", c0["present"], man_present)

# ------------------------------------------------------- gap detection

print("\nmutation 1: delete 600s of snaps -> a 600s creditable gap")
f1 = Fixture()
lo, hi = snap_span(f1)
cut_from = lo + 300_000
cut_to = cut_from + 600_000
w = f1.w()
w.execute("DELETE FROM snap WHERE run_id=? AND ts_ms>=? AND ts_ms<?",
          (f1.rid, cut_from, cut_to))
w.commit()
w.close()
g1 = settle.classify_gaps(f1.ro(), f1.rid, settle.find_gaps(f1.ro(), f1.rid))
check("one new gap found", len(g1), BASE + 1)
mine = gap_near(g1, cut_from)
check("the injected gap was located", mine is not None, True)
if mine:
    approx("gap duration ~600s", mine["seconds"], 600.0, 3.0)
    check("gap is creditable (two-sided both sides, no cancel)",
          mine["creditable"], True)
    # the whole point: an uncredited gap shrinks predicted, inflating the ratio
    run = f1.ro().execute("SELECT * FROM run WHERE run_id=?",
                          (f1.rid,)).fetchone()
    cc = settle.counters(f1.ro(), f1.rid)
    p_nocredit = settle.predicted(cc, run, [])
    p_credit = settle.predicted(cc, run, g1)
    print(f"        as_bot ${p_credit['as_bot']:.4f} vs gap_imputed "
          f"${p_credit['gap_imputed']:.4f}")
    check("gap_imputed exceeds as_bot when a gap is credited",
          p_credit["gap_imputed"] > p_credit["as_bot"] + 1e-9, True)
    check("crediting does not change as_bot",
          abs(p_nocredit["as_bot"] - p_credit["as_bot"]) < 1e-12, True)

print("\nmutation 2: same gap, but a cancel inside it -> NOT creditable")
f2 = Fixture()
lo, hi = snap_span(f2)
cut_from = lo + 300_000
cut_to = cut_from + 600_000
w = f2.w()
w.execute("DELETE FROM snap WHERE run_id=? AND ts_ms>=? AND ts_ms<?",
          (f2.rid, cut_from, cut_to))
w.execute("INSERT INTO order_event (run_id, ts_ms, kind, side, action, price, "
          "count, status, reason) VALUES (?,?,?,?,?,?,?,?,?)",
          (f2.rid, cut_from + 60_000, "cancel", "yes", None, 57, 50.0,
           None, "synthetic"))
w.commit()
w.close()
g2 = settle.classify_gaps(f2.ro(), f2.rid, settle.find_gaps(f2.ro(), f2.rid))
check("still one new gap", len(g2), BASE + 1)
mine2 = gap_near(g2, cut_from)
if mine2:
    check("cancel inside was seen", mine2["cancels"], 1)
    check("gap NOT creditable", mine2["creditable"], False)
    check("no share imputed", mine2["impute_share"], 0.0)
else:
    check("the injected gap was located", False, True)

print("\nmutation 3: same gap, but not two-sided before it -> NOT creditable")
f3 = Fixture()
lo, hi = snap_span(f3)
cut_from = lo + 300_000
cut_to = cut_from + 600_000
w = f3.w()
w.execute("DELETE FROM snap WHERE run_id=? AND ts_ms>=? AND ts_ms<?",
          (f3.rid, cut_from, cut_to))
# the row immediately before the gap loses its second side
w.execute("UPDATE snap SET two_sided=0 WHERE run_id=? AND ts_ms=("
          "SELECT MAX(ts_ms) FROM snap WHERE run_id=? AND ts_ms<?)",
          (f3.rid, f3.rid, cut_from))
w.commit()
w.close()
g3 = settle.classify_gaps(f3.ro(), f3.rid, settle.find_gaps(f3.ro(), f3.rid))
mine3 = gap_near(g3, cut_from)
check("the injected gap was located", mine3 is not None, True)
if mine3:
    check("gap NOT creditable when one-sided before it",
          mine3["creditable"], False)

print("\nmutation 4: one dropped snap is 1Hz jitter, not a gap")
f4 = Fixture()
lo, hi = snap_span(f4)
w = f4.w()
one = w.execute("SELECT ts_ms FROM snap WHERE run_id=? AND ts_ms>=? "
                "ORDER BY ts_ms LIMIT 1", (f4.rid, lo + 300_000)).fetchone()[0]
w.execute("DELETE FROM snap WHERE run_id=? AND ts_ms=?", (f4.rid, one))
w.commit()
w.close()
g4 = settle.find_gaps(f4.ro(), f4.rid)
check("single dropped snap adds no new gap", len(g4), BASE)
check("no gap reported at the drop site", gap_near(g4, one) is None, True)
# ...but it must still show up as unaccounted time, or sub-tolerance drops
# would quietly bleed share-seconds out of the numerator over six days.
c4 = settle.counters(f4.ro(), f4.rid)
check("the dropped snap is missing from the row count",
      c4["n"], c0["n"] - 1)

print("\nmutation 4b: three dropped snaps clear the tolerance -> a gap")
f4b = Fixture()
w = f4b.w()
three = [r[0] for r in w.execute(
    "SELECT ts_ms FROM snap WHERE run_id=? AND ts_ms>=? ORDER BY ts_ms LIMIT 3",
    (f4b.rid, lo + 300_000)).fetchall()]
w.execute(f"DELETE FROM snap WHERE run_id=? AND ts_ms IN "
          f"({','.join('?' * len(three))})", (f4b.rid, *three))
w.commit()
w.close()
g4b = settle.find_gaps(f4b.ro(), f4b.rid)
check("three dropped snaps add one gap", len(g4b), BASE + 1)
mine4b = gap_near(g4b, three[0])
check("the injected gap was located", mine4b is not None, True)
if mine4b:
    approx("gap duration ~3s", mine4b["seconds"], 3.0, 1.0)
f4b.close()

# ------------------------------------------------- blind vs ungated split

print("\nmutation 5: blind snaps (disconnected) must not count as gated")
f5 = Fixture()
lo, hi = snap_span(f5)
w = f5.w()
w.execute("UPDATE snap SET gated=0, connected=0, ref_yes=NULL, ref_no=NULL, "
          "share=0 WHERE run_id=? AND ts_ms>=? AND ts_ms<?",
          (f5.rid, lo + 400_000, lo + 400_000 + 100_000))
w.commit()
w.close()
c5 = settle.counters(f5.ro(), f5.rid)
check("blind snaps counted", c5["blind"] > 0, True)
check("blind reduces observed_gated", c5["observed_gated"] < c0["observed_gated"],
      True)
# presence denominator must include blind, so presence must drop
pres0 = c0["present"] / c0["observed"]
pres5 = c5["present"] / c5["observed"]
print(f"        presence {pres0:.4%} -> {pres5:.4%}")
check("blind snaps push Q7 presence down", pres5 < pres0, True)
check("gate rate falls below 100%", c5["gate_rate"] < 1.0, True)

print("\nmutation 6: ungated-but-visible is neither gated nor blind")
f6 = Fixture()
lo, hi = snap_span(f6)
w = f6.w()
w.execute("UPDATE snap SET gated=0 WHERE run_id=? AND ts_ms>=? AND ts_ms<?",
          (f6.rid, lo + 500_000, lo + 500_000 + 50_000))
w.commit()
w.close()
c6 = settle.counters(f6.ro(), f6.rid)
check("classified as ungated_visible", c6["ungated_visible"] > 0, True)
check("not classified as blind", c6["blind"], 0)

# ------------------------------------------------------- decision rule

print("\ndecision rule: thresholds land on the right side")
check("ratio 1.00 -> holds", settle.verdict(1.0)[0], "MODEL HOLDS")
check("ratio 0.70 -> holds (inclusive)", settle.verdict(0.70)[0], "MODEL HOLDS")
check("ratio 0.699 -> partial", settle.verdict(0.699)[0], "PARTIAL EROSION")
check("ratio 0.30 -> partial (inclusive)", settle.verdict(0.30)[0],
      "PARTIAL EROSION")
check("ratio 0.299 -> broken", settle.verdict(0.299)[0], "MODEL BROKEN")
check("ratio 0.0 -> broken", settle.verdict(0.0)[0], "MODEL BROKEN")
# the dollar restatement in the prediction doc must agree with the ratios
approx("holds boundary is $13.58", settle.HOLDS_AT * settle.PREREG_POINT,
       13.58, 0.005)
approx("broken boundary is $5.82", settle.BROKEN_BELOW * settle.PREREG_POINT,
       5.82, 0.005)
check("pre-registered point is 19.40 and was not edited",
      settle.PREREG_POINT, 19.40)
check("pre-registered interval intact",
      (settle.PREREG_LO, settle.PREREG_HI), (17.50, 23.00))

# ------------------------------------------------- end-to-end verdict path

print("\nend-to-end: force period end and a synthetic payout")


def run_settle(path: str, now_ms: int) -> str:
    import io
    import contextlib
    argv = sys.argv
    sys.argv = ["settle.py", "--db", path, "--now-ms", str(now_ms)]
    buf = io.StringIO()
    try:
        with contextlib.redirect_stdout(buf):
            settle.main()
    finally:
        sys.argv = argv
    return buf.getvalue()


f7 = Fixture()
run = f7.ro().execute("SELECT * FROM run WHERE run_id=?", (f7.rid,)).fetchone()
end = run["period_end_ms"]
lo, hi = snap_span(f7)

cases = [
    (10025 + 1940, "MODEL HOLDS"),      # exactly the prediction
    (10025 + 1400, "MODEL HOLDS"),      # $14.00 > $13.58
    (10025 + 1300, "PARTIAL EROSION"),  # $13.00
    (10025 + 600, "PARTIAL EROSION"),   # $6.00 > $5.82
    (10025 + 500, "MODEL BROKEN"),      # $5.00
]
for cents, want in cases:
    fx = Fixture()
    w = fx.w()
    w.execute("INSERT OR REPLACE INTO balance_poll (ts_ms, run_id, "
              "balance_cents, position_fp, realized_pnl, exposure) "
              "VALUES (?,?,?,?,?,?)",
              (end - 1000, fx.rid, cents, 0.0, 0.0, 0.0))
    w.commit()
    w.close()
    out = run_settle(fx.path, end + 1000)
    check(f"payout ${(cents - 10025) / 100:.2f} -> {want}",
          want in out, True)
    if want not in out:
        print(out[-1200:])
    fx.close()

print("\nend-to-end: $0.00 payout is a refutation, not a floor artefact")
fz = Fixture()
out = run_settle(fz.path, end + 1000)
check("zero delta reported as REFUTATION", "REFUTATION" in out, True)
check("COMPLETE label shown", "COMPLETE" in out, True)
fz.close()

print("\nend-to-end: Q7 below 90% forces INCONCLUSIVE, overriding the ratio")
f8 = Fixture()
w = f8.w()
# knock presence down by blinding most of the run, then hand it a good payout
w.execute("UPDATE snap SET gated=0, connected=0, ref_yes=NULL, ref_no=NULL "
          "WHERE run_id=? AND ts_ms > ?", (f8.rid, lo + 100_000))
w.execute("INSERT OR REPLACE INTO balance_poll (ts_ms, run_id, balance_cents, "
          "position_fp, realized_pnl, exposure) VALUES (?,?,?,?,?,?)",
          (end - 1000, f8.rid, 10025 + 1940, 0.0, 0.0, 0.0))
w.commit()
w.close()
out = run_settle(f8.path, end + 1000)
check("INCONCLUSIVE wins over a passing ratio", "INCONCLUSIVE" in out, True)
check("MODEL HOLDS suppressed", "MODEL HOLDS" not in out, True)
f8.close()

print("\nend-to-end: gaps raise the upward-bias warning")
f9 = Fixture()
w = f9.w()
w.execute("DELETE FROM snap WHERE run_id=? AND ts_ms>=? AND ts_ms<?",
          (f9.rid, lo + 300_000, lo + 900_000))
w.execute("INSERT OR REPLACE INTO balance_poll (ts_ms, run_id, balance_cents, "
          "position_fp, realized_pnl, exposure) VALUES (?,?,?,?,?,?)",
          (end - 1000, f9.rid, 10025 + 1940, 0.0, 0.0, 0.0))
w.commit()
w.close()
out = run_settle(f9.path, end + 1000)
check("biased-UP warning present", "biased UP" in out, True)
f9.close()

print("\nend-to-end: fills trigger the trading-P&L netting warning")
f10 = Fixture()
w = f10.w()
w.execute("INSERT OR REPLACE INTO our_fill (fill_id, run_id, ts_ms, exch_ts_ms,"
          " ticker, side, action, price, count, is_taker, order_id) "
          "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
          ("synthetic-1", f10.rid, lo + 10_000, lo + 10_000, run["ticker"],
           "yes", "buy", 57, 50.0, 0, "x"))
w.execute("INSERT OR REPLACE INTO balance_poll (ts_ms, run_id, balance_cents, "
          "position_fp, realized_pnl, exposure) VALUES (?,?,?,?,?,?)",
          (end - 1000, f10.rid, 10025 + 1940, 0.0, 3.50, 0.0))
w.commit()
w.close()
out = run_settle(f10.path, end + 1000)
check("fill warning present", "fills occurred" in out, True)
check("realised is netted of realized_pnl", "delta net of realized_pnl" in out,
      True)
f10.close()

for fx in (f, f1, f2, f3, f4, f5, f6, f7):
    fx.close()

print(f"\n{n - len(fails)}/{n} passed")
if fails:
    print("FAILED: " + ", ".join(fails))
    raise SystemExit(1)
print("all green")
