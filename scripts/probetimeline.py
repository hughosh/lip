#!/usr/bin/env python
"""What was the probe DOING while the market moved 13c against it?

The decomposition attributes -$7.38 of -$9.32 trading damage to 'drift while
held'. Drift is only 'adverse selection' if we were quoting into it. If we were
halted and holding, it is an unmanaged-inventory failure instead. Distinguish.
"""
import datetime as dt
import sqlite3

PROBE = "file:/Users/hugh/kek/lip/probe.db?mode=ro"


def u(ms):
    return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime("%m-%d %H:%M:%S")


c = sqlite3.connect(PROBE, uri=True)

print("=" * 88)
print("EVENT TIMELINE")
print("=" * 88)
ev = []
for r in c.execute("SELECT started_ms, run_id, note FROM run"):
    ev.append((r[0], "RUN START", f"{r[1]}  {r[2]}"))
for r in c.execute("SELECT ts_ms, run_id, switch, detail FROM halt"):
    ev.append((r[0], "HALT", f"{r[1]}  {r[2]}: {r[3]}"))
for r in c.execute("SELECT exch_ts_ms, side, action, price, count, is_taker FROM our_fill"):
    ev.append((r[0], "FILL(db)", f"{r[1]} {r[2]} {r[3]}c x{r[4]:.0f} {'T' if r[5] else 'M'}"))
# the 4 fills probe.db never recorded (exchange truth)
for ts, d in [(1785284667, "no sell 44c x1 T"), (1785284667, "no sell 44c x23 T"),
              (1785284667, "no sell 44c x25 T"), (1785284681, "no sell 43c x1 T")]:
    ev.append((ts * 1000, "FILL(exch)", d + "   <-- MISSING FROM probe.db"))
for ms, kind, d in sorted(ev):
    print(f"{u(ms)}  {kind:<11} {d}")

print()
print("=" * 88)
print("BOOK + OUR QUOTE STATE, hourly, across the whole probe")
print("=" * 88)
print(f"{'time':<16}{'run':<14}{'conn':>5}{'gate':>5}{'bid':>5}{'ask':>5}{'mid':>7}"
      f"{'ourbid':>8}{'oursz':>7}{'yes_tot':>9}{'share':>8}")
rows = c.execute(
    """SELECT ts_ms, run_id, connected, gated, ref_yes, ref_no,
              our_yes_price, our_yes_size, yes_total, share
         FROM snap ORDER BY ts_ms"""
).fetchall()
last = 0
for ts, rid, conn, gate, ry, rn, oyp, oys, yt, sh in rows:
    if ts - last < 3600_000:
        continue
    last = ts
    mid = (ry + 100 - rn) / 2 if ry is not None and rn is not None else None
    print(f"{u(ts):<16}{rid:<14}{conn:>5}{gate:>5}"
          f"{(ry if ry is not None else -1):>5}{(100-rn if rn is not None else -1):>5}"
          f"{(mid if mid is not None else -1):>7.1f}"
          f"{(oyp if oyp is not None else -1):>8}{oys:>7.0f}{yt:>9.0f}{sh:>8.4f}")

print()
print("=" * 88)
print("THE 6-HOUR WINDOW: 3rd maker fill (17:52:07) -> flatten (00:00:48)")
print("=" * 88)
A, B = 1785261127000, 1785283248000
n = c.execute("SELECT count(*) FROM snap WHERE ts_ms BETWEEN ? AND ?", (A, B)).fetchone()[0]
print(f"snaps recorded in window: {n}   (window = {(B-A)/3600000:.2f} h)")
q = c.execute(
    """SELECT count(*), sum(our_yes_size>0), sum(connected), min(ref_yes), max(ref_yes)
         FROM snap WHERE ts_ms BETWEEN ? AND ?""", (A, B)).fetchone()
print(f"  snaps with our quote resting: {q[1]} / {q[0]}")
print(f"  snaps connected:              {q[2]} / {q[0]}")
print(f"  ref_yes range in window:      {q[3]} -> {q[4]}")
print()
print("  last 3 snaps before the halt, first 3 after:")
for ts, oyp, oys, ry, rn, conn in c.execute(
    """SELECT ts_ms, our_yes_price, our_yes_size, ref_yes, ref_no, connected
         FROM snap WHERE ts_ms <= ? ORDER BY ts_ms DESC LIMIT 3""", (A + 5000,)):
    print(f"    {u(ts)}  ourbid={oyp} oursz={oys:.0f} bid={ry} conn={conn}")
for ts, oyp, oys, ry, rn, conn in c.execute(
    """SELECT ts_ms, our_yes_price, our_yes_size, ref_yes, ref_no, connected
         FROM snap WHERE ts_ms > ? ORDER BY ts_ms LIMIT 3""", (A + 5000,)):
    print(f"    {u(ts)}  ourbid={oyp} oursz={oys:.0f} bid={ry} conn={conn}")

print()
print("=" * 88)
print("IS OUR OWN SIZE THE TOUCH?  (does mid-from-snap overstate maker edge?)")
print("=" * 88)
print("  our resting size vs total yes depth at each maker-fill instant:")
for ts in (1785001178553, 1785211011000, 1785243914000, 1785261127000, 1785284503000):
    r = c.execute(
        """SELECT ts_ms, our_yes_size, yes_total, ref_yes, our_yes_price
             FROM snap WHERE abs(ts_ms-?)<120000 ORDER BY abs(ts_ms-?) LIMIT 1""",
        (ts, ts)).fetchone()
    if r:
        print(f"    {u(r[0])}  our={r[1]:>5.0f}  yes_total={r[2]:>8.0f}  "
              f"our share of book={r[1]/r[2]*100 if r[2] else 0:5.2f}%  "
              f"ourbid={r[4]} ref_yes={r[3]}")
