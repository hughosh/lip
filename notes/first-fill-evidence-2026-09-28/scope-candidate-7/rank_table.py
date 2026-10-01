"""Join shortlist.json, fill-odds.json, side-flow.json and touch-moves.json into one ranked table.
Ranking (candidate-7 handoff section 1 step 3): taker trades per hour on the WORSE side, descending,
then touch moves per hour, descending. The join-the-touch wait is shown but does not rank: a tiny
touch queue makes a thin market look fast (candidate-6 handoff, "Market choice"). Tickers that
already carry a retained-latch runtime from an earlier stage are marked; a fresh store on one is
permitted by operator_stage.py but muddies the evidence trail. Reads only files next to this
script; writes ranked-table.json and ranked-table.txt beside them. Diagnostic, not launch authority.
"""
import json
import sys

SP = sys.argv[0].rsplit("/", 1)[0]
LATCHED = ("KXEPLRELEGATION-27-MCI", "KXAAAGASW-26OCT05-4.4200", "KXDANCINGWITHTHESTARS-26DEC31-EFRE", "BROSFT")

shortlist = json.load(open(f"{SP}/shortlist.json"))
odds = {r["ticker"]: r for r in json.load(open(f"{SP}/fill-odds.json"))["rows"]}
flow = {r["ticker"]: r for r in json.load(open(f"{SP}/side-flow.json"))["rows"]}
touch = {r["ticker"]: r for r in json.load(open(f"{SP}/touch-moves.json"))["rows"]}

rows = []
for s in shortlist["shortlist"]:
    t = s["ticker"]
    o, f, m = odds[t], flow[t], touch.get(t, {})
    yb, nb = o["yes_bid"], o["no_bid"]
    y, n = f["yes_bid"], f["no_bid"]
    rows.append({
        "ticker": t,
        "title": s["title"],
        "worse_side_trades_per_h": min(y["trades_per_h"], n["trades_per_h"]),
        "yes_bid_trades_per_h": y["trades_per_h"], "no_bid_trades_per_h": n["trades_per_h"],
        "yes_bid_contracts_per_h": y["contracts_per_h"], "no_bid_contracts_per_h": n["contracts_per_h"],
        "yes_bid_ge12_per_h": y["ge12_per_h"], "no_bid_ge12_per_h": n["ge12_per_h"],
        "yes_last3h_trades": y["last3h_trades"], "no_last3h_trades": n["last3h_trades"],
        "yes_last_hit_min_ago": y["last_hit_min_ago"], "no_last_hit_min_ago": n["last_hit_min_ago"],
        "yes_bid": yb, "no_bid": nb, "mid_c": (yb + 100 - nb) / 2, "spread_c": 100 - yb - nb,
        "yes_bid_queue": o["yes_bid_queue"], "no_bid_queue": o["no_bid_queue"],
        "yes_wait_min": s["yes_wait_min"], "no_wait_min": s["no_wait_min"], "worse_wait_min": s["worse_wait_min"],
        "touch_moves_per_h": m.get("moves_per_h"), "touch_moves_3h": m.get("moves_3h"),
        "px_range_3h_c": m.get("px_range_3h_c"), "last_move_age_min": m.get("last_move_age_min"),
        "flow_window_h": f["window_h"], "flow_trades": f["trades"],
        "fee_type": s["fee_type"], "market_close": s["market_close"], "program_end": s["program_end"],
        "period_reward": s["period_reward"], "target_size": s["target_size"],
        "note": "retained-latch runtime on this ticker" if any(k in t for k in LATCHED) else "",
    })
rows.sort(key=lambda r: (-r["worse_side_trades_per_h"], -(r["touch_moves_per_h"] or 0)))

hdr = (f"{'#':>2} {'ticker':<38} {'worse/h':>7} {'yes/h':>6} {'no/h':>6} {'ge12 y/n':>9} {'3h y/n':>7} "
       f"{'YES/NO bid':>10} {'spr':>3} {'q y/n':>11} {'wait y/n':>11} {'mv/h':>5} {'mv3h':>4} {'rng3h':>5} "
       f"{'prog_end':<16} {'close':<10} note")
lines = [hdr]
for i, r in enumerate(rows, 1):
    lines.append(
        f"{i:>2} {r['ticker']:<38} {r['worse_side_trades_per_h']:>7.2f} {r['yes_bid_trades_per_h']:>6.2f} "
        f"{r['no_bid_trades_per_h']:>6.2f} {r['yes_bid_ge12_per_h']:>4.2f}/{r['no_bid_ge12_per_h']:<4.2f} "
        f"{r['yes_last3h_trades']:>3}/{r['no_last3h_trades']:<3} {r['yes_bid']:>4}/{r['no_bid']:<5} {r['spread_c']:>3} "
        f"{r['yes_bid_queue']:>5g}/{r['no_bid_queue']:<5g} {r['yes_wait_min']:>5}/{r['no_wait_min']:<5} "
        f"{(r['touch_moves_per_h'] if r['touch_moves_per_h'] is not None else -1):>5.1f} {r['touch_moves_3h'] if r['touch_moves_3h'] is not None else '-':>4} "
        f"{r['px_range_3h_c'] if r['px_range_3h_c'] is not None else '-':>5} "
        f"{str(r['program_end'])[:16]:<16} {str(r['market_close'])[:10]:<10} {r['note']}")
text = "\n".join(lines)
print(text)
open(f"{SP}/ranked-table.txt", "w").write(text + "\n")
json.dump({"observed_utc": shortlist["observed_utc"], "side_flow_observed_utc": json.load(open(f"{SP}/side-flow.json"))["observed_at_utc"],
           "touch_observed_utc": json.load(open(f"{SP}/touch-moves.json"))["observed_at_utc"],
           "rank": "worse-side taker trades per hour desc, then touch moves per hour desc",
           "rows": rows}, open(f"{SP}/ranked-table.json", "w"), indent=1)
