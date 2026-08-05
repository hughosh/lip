#!/usr/bin/env python
"""Decompose the live probe's realized P&L into spread / drift / maker-edge.

Ledger is rebuilt from the EXCHANGE (/portfolio/fills), not probe.db --
probe.db.our_fill is missing 4 fills and mis-attributes run_id via a
startup backfill that stamps historical fills with the current run.

Three-way split, per round trip, in yes-equivalent cents:

    exit - entry  =  (mid_entry - entry)      maker edge captured at entry
                  +  (mid_exit  - mid_entry)  drift / adverse selection
                  +  (exit - mid_exit)        spread paid crossing out

Mid comes from probe.db.snap (the probe's own 1Hz book record):
    yes_bid = ref_yes, yes_ask = 100 - ref_no, mid = (yes_bid + yes_ask)/2
Caveat tracked below: ref_* include our own resting size.
"""
import json
import sqlite3
import sys
import urllib.parse
import urllib.request

sys.path.insert(0, "/Users/hugh/kek/lip")
from auth import Signer  # noqa: E402

TICKER = "KXGENERICBALLOTVOTEHUB-26JUL31-T5.7"
PROBE = "file:/Users/hugh/kek/lip/probe.db?mode=ro"
RIG = "file:/Users/hugh/kek/lip/rig.db?mode=ro"


def fills():
    s = Signer()
    path = "/trade-api/v2/portfolio/fills"
    url = "https://api.elections.kalshi.com" + path + "?" + urllib.parse.urlencode({"limit": 200})
    req = urllib.request.Request(url, headers=s.headers("GET", path))
    raw = json.load(urllib.request.urlopen(req))["fills"]
    out = []
    for f in raw:
        if f["ticker"] != TICKER:
            continue
        yp = float(f["yes_price_dollars"]) * 100.0  # yes-equivalent price, cents
        n = float(f["count_fp"])
        # side/action encode the leg; economically every fill here is either
        # +n yes-equivalent (yes buy) or -n (no sell == shed yes exposure).
        signed = n if f["action"] == "buy" else -n
        out.append(
            dict(
                ts=f["ts"] * 1000,
                iso=f["created_time"][:19],
                n=n,
                signed=signed,
                px=yp,
                taker=bool(f["is_taker"]),
                fee=float(f["fee_cost"]),
            )
        )
    out.sort(key=lambda x: (x["ts"], -x["signed"]))
    return out


def mid_at(con, ts, window_ms=120_000):
    """Nearest snap within window. Returns (mid_c, bid, ask, lag_ms, ourbid, oursz)."""
    row = con.execute(
        """SELECT ts_ms, ref_yes, ref_no, our_yes_price, our_yes_size,
                  abs(ts_ms - ?) AS d
             FROM snap
            WHERE ref_yes IS NOT NULL AND ref_no IS NOT NULL
              AND abs(ts_ms - ?) <= ?
         ORDER BY d LIMIT 1""",
        (ts, ts, window_ms),
    ).fetchone()
    if not row:
        return None
    ts_s, ry, rn, oyp, oys, d = row
    bid, ask = ry, 100 - rn
    return dict(mid=(bid + ask) / 2.0, bid=bid, ask=ask, lag=d, ourbid=oyp, oursz=oys)


def main():
    fs = fills()
    pc = sqlite3.connect(PROBE, uri=True)

    print("=" * 96)
    print("LEDGER  (yes-equivalent; +n = acquired yes exposure, -n = shed it)")
    print("=" * 96)
    print(f"{'time (UTC)':<21}{'n':>6}{'px':>7}{'liq':>5}{'fee':>8}"
          f"{'bid':>5}{'ask':>5}{'mid':>7}{'lag_s':>7}{'ourbid':>8}{'pos':>7}")
    pos = 0.0
    for f in fs:
        m = mid_at(pc, f["ts"])
        pos += f["signed"]
        f["m"] = m
        b = f"{m['bid']:>5}" if m else "    ?"
        a = f"{m['ask']:>5}" if m else "    ?"
        md = f"{m['mid']:>7.1f}" if m else "      ?"
        lg = f"{m['lag']/1000:>7.1f}" if m else "      ?"
        ob = (f"{m['ourbid']:>8}" if m and m["ourbid"] is not None else "       -")
        print(f"{f['iso']:<21}{f['signed']:>6.0f}{f['px']:>7.2f}"
              f"{('T' if f['taker'] else 'M'):>5}{f['fee']:>8.4f}{b}{a}{md}{lg}{ob}{pos:>7.0f}")

    # ---- round trips: entry = maker buys, exit = the taker flatten that follows
    trips = []
    cur = []
    for f in fs:
        if f["signed"] > 0:
            if cur and cur[-1]["signed"] < 0:
                trips.append(cur)
                cur = []
            cur.append(f)
        else:
            cur.append(f)
    if cur:
        trips.append(cur)

    print()
    print("=" * 96)
    print("DECOMPOSITION  (cents per contract, and dollars)")
    print("=" * 96)
    tot = dict(edge=0.0, drift=0.0, spread=0.0, fee=0.0, real=0.0)
    for i, t in enumerate(trips, 1):
        ent = [f for f in t if f["signed"] > 0]
        ext = [f for f in t if f["signed"] < 0]
        if not ent or not ext:
            print(f"\ntrip {i}: incomplete ({len(ent)} entries, {len(ext)} exits) -- skipped")
            continue
        qe = sum(f["n"] for f in ent)
        qx = sum(f["n"] for f in ext)
        pe = sum(f["px"] * f["n"] for f in ent) / qe
        px = sum(f["px"] * f["n"] for f in ext) / qx
        me = sum(f["m"]["mid"] * f["n"] for f in ent) / qe
        mx = sum(f["m"]["mid"] * f["n"] for f in ext) / qx
        q = min(qe, qx)
        edge = me - pe          # maker edge captured at entry
        drift = mx - me         # market moved against/for us while we held
        spread = px - mx        # cost of crossing out
        fee = sum(f["fee"] for f in t)
        real = (px - pe) * q / 100.0
        hold = (ext[0]["ts"] - ent[0]["ts"]) / 60000.0
        print(f"\ntrip {i}:  {ent[0]['iso']} -> {ext[-1]['iso']}   ({hold:.1f} min held, q={q:.0f})")
        print(f"   entry  {pe:6.2f}c vs mid {me:6.2f}c    exit {px:6.2f}c vs mid {mx:6.2f}c")
        print(f"   {'maker edge at entry':<28}{edge:+7.2f}c  {edge*q/100:+8.2f}")
        print(f"   {'drift while held':<28}{drift:+7.2f}c  {drift*q/100:+8.2f}")
        print(f"   {'spread paid crossing out':<28}{spread:+7.2f}c  {spread*q/100:+8.2f}")
        print(f"   {'-'*28}{'':>7}   {'-'*8}")
        print(f"   {'realized (sum)':<28}{px-pe:+7.2f}c  {real:+8.2f}")
        print(f"   {'taker fees':<28}{'':>7}   {-fee:+8.2f}")
        print(f"   {'NET':<28}{'':>7}   {real-fee:+8.2f}")
        tot["edge"] += edge * q / 100
        tot["drift"] += drift * q / 100
        tot["spread"] += spread * q / 100
        tot["fee"] += fee
        tot["real"] += real

    print()
    print("=" * 96)
    print("TOTAL")
    print("=" * 96)
    print(f"   {'maker edge at entry':<28}{tot['edge']:+9.2f}")
    print(f"   {'drift while held':<28}{tot['drift']:+9.2f}")
    print(f"   {'spread paid crossing out':<28}{tot['spread']:+9.2f}")
    print(f"   {'realized':<28}{tot['real']:+9.2f}")
    print(f"   {'taker fees':<28}{-tot['fee']:+9.2f}")
    print(f"   {'trading damage':<28}{tot['real']-tot['fee']:+9.2f}")
    print(f"   {'LIP reward':<28}{7.65:+9.2f}")
    print(f"   {'NET':<28}{tot['real']-tot['fee']+7.65:+9.2f}")
    print()
    harness = -(tot["spread"]) + tot["fee"]
    print(f"   harness-attributable (spread crossed out + taker fees): {harness:.2f}")
    print(f"   strategy-attributable (drift + maker edge):             "
          f"{tot['edge']+tot['drift']:+.2f}")


if __name__ == "__main__":
    main()
