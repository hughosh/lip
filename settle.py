"""Phase 2 settlement analysis — realised vs pre-registered predicted payout.

Read-only. Opens `probe.db` with `mode=ro` because the bot is still writing to
it, touches no frozen artifact, and by default makes no network call (the
balance series is already in `balance_poll`). `--live` adds one authenticated
GET for a fresh balance.

Runs mid-period too: before `period_end` it labels itself IN PROGRESS and
projects to the end instead of pronouncing a verdict.

Three things here are deliberate and worth reading before trusting the output.

1. `predicted` is reconstructed two ways. The bot's own accumulator only
   advances while the process is running, but our orders rest on the exchange
   whether or not we are watching them, and Kalshi's 1 Hz scorer keeps crediting
   them. So an observation gap shrinks `predicted` while leaving `realised`
   untouched, which pushes `realised / predicted` UP -- toward "model holds".
   `as_bot` is therefore a LOWER bound on predicted and an UPPER bound on the
   ratio. `gap_imputed` credits gaps we can show we were resting across.

2. Q7 presence is likewise reported over the observed sample (what was
   pre-registered) and over wall-clock since entry (which counts gaps against
   us). A high observed figure next to a low wall-clock figure means the sample
   is biased, not that presence was good.

3. The headline ratio is against the pre-registered $19.40, per §5 and the
   prediction doc's dollar restatement -- NOT against the figure this script
   measures. The measured-vs-predicted discrepancy is reported separately
   because the prediction doc calls that discrepancy a finding in its own right.
"""
from __future__ import annotations

import argparse
import datetime as dt
import sqlite3
import sys

# --- the pre-registration, transcribed from notes/phase2-prediction.md §2/§3.
# These are the locked numbers. Do not recompute them from anything.
PREREG_POINT = 19.40
PREREG_LO, PREREG_HI = 17.50, 23.00
PREREG_PRESENCE = 0.98
HOLDS_AT = 0.7            # ratio >= 0.7  -> model holds
BROKEN_BELOW = 0.3        # ratio <  0.3  -> model broken
Q7_GATE = 0.90            # below this the run is inconclusive, not evidence
FLOOR = 1.00              # payouts below $1.00 are not paid at all

GAP_TOL_MS = 2500         # snaps are 1 Hz; >2.5s apart is a real gap


def u(ms: float | None) -> str:
    if not ms:
        return "-"
    return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).strftime(
        "%Y-%m-%d %H:%M:%SZ")


def dur(s: float) -> str:
    s = int(s)
    if s < 60:
        return f"{s}s"
    if s < 3600:
        return f"{s // 60}m{s % 60:02d}s"
    if s < 86400:
        return f"{s // 3600}h{(s % 3600) // 60:02d}m"
    return f"{s // 86400}d{(s % 86400) // 3600:02d}h"


def connect(path: str) -> sqlite3.Connection:
    c = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    c.row_factory = sqlite3.Row
    return c


# ------------------------------------------------------------------ coverage

def find_gaps(conn: sqlite3.Connection, run_id: str) -> list[dict]:
    """Consecutive-snapshot gaps wider than one 1 Hz tick.

    A gap is where the process was not running (machine asleep, killed,
    starved). It is NOT the same as a blind snapshot: blind means the process
    ran and wrote a row saying it could not see the book. Blind is disclosed by
    the bot; a gap is invisible unless you look for it, which is why this
    function exists.
    """
    rows = conn.execute(
        "SELECT ts_ms, two_sided, share, our_yes_size, our_no_size "
        "FROM snap WHERE run_id=? ORDER BY ts_ms", (run_id,)).fetchall()
    gaps = []
    for a, b in zip(rows, rows[1:]):
        delta = b["ts_ms"] - a["ts_ms"]
        if delta <= GAP_TOL_MS:
            continue
        gaps.append({
            "start_ms": a["ts_ms"], "end_ms": b["ts_ms"],
            "seconds": (delta - 1000) / 1000.0,   # one tick is expected
            "before_two_sided": bool(a["two_sided"]),
            "after_two_sided": bool(b["two_sided"]),
            "before_share": a["share"] or 0.0,
            "after_share": b["share"] or 0.0,
        })
    return gaps


def cancels_in(conn: sqlite3.Connection, run_id: str,
               t0: int, t1: int) -> int:
    return conn.execute(
        "SELECT COUNT(*) FROM order_event WHERE run_id=? AND ts_ms>? AND "
        "ts_ms<? AND kind IN ('cancel','cancel_ack','reject')",
        (run_id, t0, t1)).fetchone()[0]


def classify_gaps(conn: sqlite3.Connection, run_id: str,
                  gaps: list[dict]) -> list[dict]:
    """Decide, per gap, whether we can credit ourselves for resting across it.

    Creditable requires all of: a two-sided quote on the snapshot before AND
    after, and no cancel/reject logged inside the window. That is not proof --
    only the exchange knows -- but the orders are good-till-cancelled and
    nothing in the record cancelled them, so resting throughout is much the
    likelier reading than a coincidental re-establishment.
    """
    for g in gaps:
        g["cancels"] = cancels_in(conn, run_id, g["start_ms"], g["end_ms"])
        g["creditable"] = (g["before_two_sided"] and g["after_two_sided"]
                           and g["cancels"] == 0)
        g["impute_share"] = ((g["before_share"] + g["after_share"]) / 2.0
                             if g["creditable"] else 0.0)
    return gaps


# ------------------------------------------------------------- reconstruction

def counters(conn: sqlite3.Connection, run_id: str) -> dict:
    """Rebuild the bot's Q8.2 / Q7 accumulators from the stored snapshots.

    Mirrors probebot.py: a snap row's `gated` column is already
    `gated AND NOT blind`, so blind rows land in the same `gated=0` bucket as
    genuinely-ungated ones. They are separated here by the book: going stale
    clears it, so a blind row has NULL reference prices.
    """
    r = conn.execute(
        "SELECT COUNT(*) n, "
        "SUM(gated) observed_gated, "
        "SUM(CASE WHEN gated=1 THEN share ELSE 0 END) integrated_share, "
        "SUM(CASE WHEN gated=1 AND two_sided=1 THEN 1 ELSE 0 END) present, "
        "SUM(CASE WHEN gated=0 AND (connected=0 OR (ref_yes IS NULL AND "
        "     ref_no IS NULL)) THEN 1 ELSE 0 END) blind, "
        "SUM(CASE WHEN gated=0 AND connected=1 AND (ref_yes IS NOT NULL OR "
        "     ref_no IS NOT NULL) THEN 1 ELSE 0 END) ungated_visible, "
        "MIN(ts_ms) first_ms, MAX(ts_ms) last_ms "
        "FROM snap WHERE run_id=?", (run_id,)).fetchone()
    d = {k: (r[k] or 0) for k in r.keys()}
    d["observed"] = d["observed_gated"] + d["blind"]
    d["gate_rate"] = (d["observed_gated"] / d["observed"]) if d["observed"] else 0.0
    d["mean_share"] = ((d["integrated_share"] / d["observed_gated"])
                       if d["observed_gated"] else 0.0)
    return d


def predicted(c: dict, run: sqlite3.Row, gaps: list[dict]) -> dict:
    """Q8.2 both ways: as the bot accumulates it, and crediting gaps."""
    period_s = max(1.0, (run["period_end_ms"] - run["period_start_ms"]) / 1000.0)
    total_gated = period_s * c["gate_rate"]

    imputed = sum(g["impute_share"] * g["seconds"]
                  for g in gaps if g["creditable"])
    return {
        "period_s": period_s,
        "total_gated_estimate": total_gated,
        "as_bot": ((c["integrated_share"] / total_gated) * run["pool"]
                   if total_gated > 0 else 0.0),
        "gap_imputed": (((c["integrated_share"] + imputed) / total_gated)
                        * run["pool"] if total_gated > 0 else 0.0),
        "imputed_share_seconds": imputed,
    }


# -------------------------------------------------------------------- payout

def realised(conn: sqlite3.Connection, run_id: str, live: bool) -> dict:
    rows = conn.execute(
        "SELECT ts_ms, balance_cents, realized_pnl, position_fp FROM "
        "balance_poll WHERE run_id=? ORDER BY ts_ms", (run_id,)).fetchall()
    if not rows:
        return {"ok": False, "why": "no balance_poll rows"}
    first, last = rows[0], rows[-1]
    live_cents = None
    if live:
        sys.path.insert(0, "/Users/hugh/kek/lip")
        from kalshi import Client
        live_cents = Client().balance()["balance"]

    final_cents = live_cents if live_cents is not None else last["balance_cents"]
    fills = conn.execute(
        "SELECT COUNT(*), COALESCE(SUM(count),0) FROM our_fill WHERE run_id=?",
        (run_id,)).fetchone()
    return {
        "ok": True,
        "first_ms": first["ts_ms"], "last_ms": last["ts_ms"],
        "opening_cents": first["balance_cents"],
        "final_cents": final_cents,
        "live_used": live_cents is not None,
        "delta": (final_cents - first["balance_cents"]) / 100.0,
        "realized_pnl": last["realized_pnl"] or 0.0,
        "position_fp": last["position_fp"] or 0.0,
        "n_fills": fills[0], "filled_contracts": fills[1],
        "polls": len(rows),
    }


def exchange_state(ticker: str) -> dict:
    """Read-only look at what the exchange thinks we have resting.

    Matters most after a halt: Q6.3 cancels on the way out, but if the halt was
    caused by an outage those cancels fail too (probebot.py logs
    'post-cancel sweep failed'), and orders can still be resting -- earning,
    unmanaged, with our numerator stopped. Reads only; never cancels.
    """
    sys.path.insert(0, "/Users/hugh/kek/lip")
    from kalshi import Client
    k = Client()
    out: dict = {}
    try:
        r = k.orders(ticker=ticker, status="resting")
        out["resting"] = r.get("orders") or []
    except Exception as e:
        out["resting_err"] = repr(e)
    try:
        pos = k.positions(ticker)
        out["positions"] = pos.get("market_positions") or []
    except Exception as e:
        out["positions_err"] = repr(e)
    try:
        # the fixed-point book: levels are [price_dollars, size], best last
        ob = k.orderbook(ticker)["orderbook_fp"]
        out["book"] = {leg: (ob.get(f"{leg}_dollars") or [])[-1:]
                       for leg in ("yes", "no")}
    except Exception as e:
        out["book_err"] = repr(e)
    return out


def verdict(ratio: float) -> tuple[str, str]:
    if ratio >= HOLDS_AT:
        return ("MODEL HOLDS",
                "competition did not materially erode share -> proceed to a "
                "multi-market scale-up SPEC (not a scale-up)")
    if ratio >= BROKEN_BELOW:
        return ("PARTIAL EROSION",
                "re-measure in a different market family before scaling; do "
                "not scale on one observation")
    return ("MODEL BROKEN",
            "stop and diagnose: (a) participants respond to our presence, "
            "(b) the visible book is not the scoring book, (c) our presence "
            "fraction was overstated")


# ---------------------------------------------------------------------- main

def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default="probe.db")
    ap.add_argument("--run", default=None, help="run_id (default: latest)")
    ap.add_argument("--live", action="store_true",
                    help="one authenticated GET for a fresh balance")
    ap.add_argument("--now-ms", type=int, default=None,
                    help="override wall clock (testing)")
    a = ap.parse_args()

    conn = connect(a.db)
    run = (conn.execute("SELECT * FROM run WHERE run_id=?", (a.run,)).fetchone()
           if a.run else
           conn.execute("SELECT * FROM run ORDER BY started_ms DESC "
                        "LIMIT 1").fetchone())
    if run is None:
        print("no run found", file=sys.stderr)
        return 2
    rid = run["run_id"]
    now = a.now_ms or int(dt.datetime.now(dt.timezone.utc).timestamp() * 1000)
    done = now >= run["period_end_ms"]

    c = counters(conn, rid)
    gaps = classify_gaps(conn, rid, find_gaps(conn, rid))
    p = predicted(c, run, gaps)
    money = realised(conn, rid, a.live)
    halts = conn.execute("SELECT * FROM halt WHERE run_id=? ORDER BY ts_ms",
                         (rid,)).fetchall()

    W = 74
    print("=" * W)
    print(f"  PHASE 2 SETTLEMENT  --  {'COMPLETE' if done else 'IN PROGRESS'}")
    print("=" * W)
    print(f"  run          {rid}   {'DRY RUN' if run['dry_run'] else 'LIVE'}")
    print(f"  market       {run['ticker']}")
    print(f"  program      {run['program_id']}")
    print(f"  pool ${run['pool']:.0f}   target {run['target']:.0f}   "
          f"df {run['df']}   size {run['size']}")
    print(f"  period       {u(run['period_start_ms'])} -> "
          f"{u(run['period_end_ms'])}   ({dur(p['period_s'])})")
    print(f"  entry        {u(run['started_ms'])}")
    entry_frac = ((run["started_ms"] - run["period_start_ms"])
                  / (run["period_end_ms"] - run["period_start_ms"]))
    print(f"  entered at   {entry_frac:.1%} elapsed   "
          f"(max achievable share fraction {1 - entry_frac:.3f})")
    if not done:
        print(f"  remaining    {dur((run['period_end_ms'] - now) / 1000)}")

    # ---- coverage
    since_entry_s = (min(now, run["period_end_ms"]) - run["started_ms"]) / 1000.0
    gap_s = sum(g["seconds"] for g in gaps)
    cov = 1 - (gap_s / since_entry_s) if since_entry_s > 0 else 0.0
    print("-" * W)
    print("  COVERAGE")
    print(f"    snapshots written      {c['n']}")
    print(f"    wall clock since entry {dur(since_entry_s)} "
          f"({since_entry_s:.0f}s)")
    print(f"    observation gaps       {len(gaps)}  totalling {dur(gap_s)}")
    print(f"    coverage               {cov:.2%}")
    # Gaps only catch holes wider than one tick. This catches every second we
    # have no row for, including single dropped snaps that never cluster into a
    # reportable gap -- they lose share-seconds from the numerator just the same.
    unaccounted = max(0.0, since_entry_s - c["n"])
    print(f"    unaccounted seconds    {unaccounted:.0f}s "
          f"({unaccounted / since_entry_s:.2%} of wall clock)"
          + ("" if unaccounted <= gap_s + 2 else
             f"  -- {unaccounted - gap_s:.0f}s outside reported gaps"))
    if gaps:
        cred = [g for g in gaps if g["creditable"]]
        print(f"    creditable gaps        {len(cred)} "
              f"({dur(sum(g['seconds'] for g in cred))}) "
              f"-- two-sided either side, no cancel inside")
        for g in sorted(gaps, key=lambda x: -x["seconds"])[:8]:
            flag = "credit" if g["creditable"] else "NO-CREDIT"
            note = "" if g["cancels"] == 0 else f"  cancels={g['cancels']}"
            print(f"      {u(g['start_ms'])} +{dur(g['seconds']):>8}  "
                  f"{flag}{note}")

    # ---- Q7
    obs_presence = (c["present"] / c["observed"]) if c["observed"] else 0.0
    wall_presence = (c["present"] / since_entry_s) if since_entry_s > 0 else 0.0
    print("-" * W)
    print("  Q7 PRESENCE   (gate: >= 90% or the run is inconclusive)")
    print(f"    over observed snaps    {obs_presence:.2%}   "
          f"[{c['present']} / {c['observed']}]   <- the pre-registered basis")
    print(f"    over wall clock        {wall_presence:.2%}   "
          f"[{c['present']} / {since_entry_s:.0f}s]   <- counts gaps against us")
    print(f"    blind snaps            {c['blind']}    "
          f"ungated-but-visible {c['ungated_visible']}")
    print(f"    gate rate              {c['gate_rate']:.2%}")
    print(f"    predicted was          >= {PREREG_PRESENCE:.0%}")
    q7_basis = obs_presence
    print(f"    -> Q7 {'PASS' if q7_basis >= Q7_GATE else 'FAIL -> INCONCLUSIVE'}"
          f" on the observed basis"
          + ("" if wall_presence >= Q7_GATE else
             f";  wall-clock basis would {'PASS' if wall_presence >= Q7_GATE else 'FAIL'}"))

    # ---- Q8.2
    print("-" * W)
    print("  Q8.2 PREDICTED PAYOUT   (measured, for comparison with prereg)")
    print(f"    mean share while present   {c['mean_share']:.4%}")
    print(f"    integrated share-seconds   {c['integrated_share']:.2f}")
    print(f"    total gated estimate       {p['total_gated_estimate']:.0f}s "
          f"(whole period x gate rate)")
    print(f"    predicted  as_bot          ${p['as_bot']:.2f}   "
          f"<- lower bound (gaps uncredited)")
    print(f"    predicted  gap_imputed     ${p['gap_imputed']:.2f}   "
          f"<- credits {p['imputed_share_seconds']:.2f} share-seconds")
    if not done and since_entry_s > 0:
        # project the observed mean share forward over the rest of the period
        remaining_s = (run["period_end_ms"] - now) / 1000.0
        proj = ((c["integrated_share"]
                 + sum(g["impute_share"] * g["seconds"] for g in gaps
                       if g["creditable"])
                 + c["mean_share"] * remaining_s * c["gate_rate"])
                / p["total_gated_estimate"]) * run["pool"]
        print(f"    PROJECTED to period end    ${proj:.2f}   "
              f"(current share held for {dur(remaining_s)} more)")

    # ---- realised
    print("-" * W)
    print("  REALISED   (Q8.3: observable only as a /portfolio/balance delta)")
    if not money["ok"]:
        print(f"    unavailable: {money['why']}")
    else:
        print(f"    opening balance   ${money['opening_cents'] / 100:.2f}   "
              f"@ {u(money['first_ms'])}")
        print(f"    final balance     ${money['final_cents'] / 100:.2f}   "
              f"@ {'live' if money['live_used'] else u(money['last_ms'])}"
              f"   ({money['polls']} polls)")
        print(f"    delta             ${money['delta']:+.2f}")
        print(f"    fills {money['n_fills']}  contracts "
              f"{money['filled_contracts']:.0f}   "
              f"realized_pnl ${money['realized_pnl']:+.2f}   "
              f"position {money['position_fp']:+.0f}")
        if money["n_fills"]:
            print("    !! fills occurred: the balance delta mixes trading P&L "
                  "with the LIP payout.")
            print("       Net out realized_pnl (and any settlement P&L) before "
                  "reading the delta as a payout.")

    # ---- halts
    if halts:
        print("-" * W)
        print("  HALTS")
        for h in halts:
            print(f"    {u(h['ts_ms'])}  {h['switch']}  {h['detail']}")
        print("    !! a halt is permanent by design (Q6). Check with --live "
              "whether orders are")
        print("       still resting: if the halt was an outage, its cancels "
              "failed too.")

    # ---- exchange cross-check
    if a.live:
        x = exchange_state(run["ticker"])
        print("-" * W)
        print("  EXCHANGE (read-only)")
        if "resting_err" in x:
            print(f"    resting orders: ERROR {x['resting_err']}")
        else:
            print(f"    resting orders: {len(x['resting'])}")
            for o in x["resting"]:
                print(f"      {o.get('order_id')}  {o.get('side')}/"
                      f"{o.get('action')}  {o.get('status')}  "
                      f"created {o.get('created_time')}")
        if "positions_err" in x:
            print(f"    positions: ERROR {x['positions_err']}")
        else:
            print(f"    positions: {x['positions'] or 'flat'}")
        if "book_err" in x:
            print(f"    book: ERROR {x['book_err']}")
        else:
            for leg, lv in x["book"].items():
                print(f"    book {leg} touch: {lv[0] if lv else 'empty'}")
        expected = 0 if (halts or done) else 2
        got = len(x.get("resting", []))
        if got != expected:
            print(f"    !! expected {expected} resting orders, found {got}")

    # ---- verdict
    print("=" * W)
    if not done:
        print("  NO VERDICT -- period still open. Nothing to decide yet, and")
        print("  changing anything now would void the pre-registration (§5).")
        print("=" * W)
        return 0
    if not money["ok"]:
        print("  NO VERDICT -- no balance series.")
        print("=" * W)
        return 1

    obs = money["delta"] - (money["realized_pnl"] or 0.0)
    print(f"  DECISION RULE (§5)   predicted = ${PREREG_POINT:.2f} "
          f"(pre-registered, locked)")
    print(f"    realised LIP payout   ${obs:.2f}"
          + ("" if not money["n_fills"] else "   (delta net of realized_pnl)"))
    if q7_basis < Q7_GATE:
        print("    -> INCONCLUSIVE: Q7 below 90%. Per §5/Q7 this is reported "
              "as absence-confounded,")
        print("       not as evidence about competition.")
    elif abs(obs) < 1e-9:
        print(f"    -> $0.00 exactly. Floor check: predicted ${PREREG_POINT:.2f} "
              f"clears the ${FLOOR:.2f} floor {PREREG_POINT / FLOOR:.0f}x,")
        print("       so this is a REFUTATION, not a floor artefact.")
    else:
        ratio = obs / PREREG_POINT
        name, action = verdict(ratio)
        print(f"    ratio = {obs:.2f} / {PREREG_POINT:.2f} = {ratio:.3f}")
        print(f"    -> {name}")
        print(f"       {action}")
        print(f"    within prereg interval ${PREREG_LO:.2f}-${PREREG_HI:.2f}? "
              f"{'yes' if PREREG_LO <= obs <= PREREG_HI else 'NO'}")
        if p["as_bot"] > 0:
            m = obs / p["as_bot"]
            print(f"    vs MEASURED predicted ${p['as_bot']:.2f}: "
                  f"ratio {m:.3f}")
            drift = abs(p["as_bot"] - PREREG_POINT) / PREREG_POINT
            if drift > 0.15:
                print(f"    !! measured predicted differs from pre-registered "
                      f"by {drift:.0%}.")
                print("       The prediction doc calls that discrepancy a "
                      "finding in its own right.")
        if gap_s > 0:
            print(f"    !! {dur(gap_s)} of gaps ({1 - cov:.1%}) -- our orders "
                  "rested through them and were")
            print("       scored, but our numerator did not accumulate, so "
                  "this ratio is biased UP.")
    print("=" * W)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
