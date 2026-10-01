#!/usr/bin/env python3
"""Score a public orderbook sweep under the July 30, 2026 LIP terms (and the February rule
for comparison). Reads programs-active.json, books.jsonl and markets.jsonl (optional).

Usage: analyse_sweep.py --programs programs-active.json --books sweep/books.jsonl
                        [--markets sweep/markets.jsonl] [--out summary.json]

Everything here is a one-shot snapshot of the field: no competitive response, no fills,
no fees, no downtime. It prices a resting quote's SHARE of the reward, not profit.
"""
import argparse, json, re, statistics as st, sys, os
from datetime import datetime, timezone
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lip30score import levels_from_dollars, snapshot, walk, side_total  # noqa: E402


def iso(s):
    s = s.replace("Z", "+00:00")
    s = re.sub(r"\.(\d{1,6})\d*", lambda m: "." + m.group(1).ljust(6, "0"), s)
    return datetime.fromisoformat(s)


def q(a, x):
    a = sorted(a)
    return a[min(len(a) - 1, int(x * len(a)))] if a else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--programs", required=True)
    ap.add_argument("--books", required=True)
    ap.add_argument("--markets")
    ap.add_argument("--out")
    ap.add_argument("--sizes", default="12,100")
    ap.add_argument("--tickers", default="", help="comma list to print in detail")
    a = ap.parse_args()
    sizes = [float(x) for x in a.sizes.split(",")]
    progs = {p["market_ticker"]: p for p in json.load(open(a.programs))["incentive_programs"]}
    status = {}
    if a.markets and os.path.exists(a.markets):
        for line in open(a.markets):
            m = json.loads(line)
            status[m["ticker"]] = m
    books = {}
    for line in open(a.books):
        b = json.loads(line)
        books[b["ticker"]] = b
    now = max(iso(b["ts"]) for b in books.values())

    rows = []
    n_http_fail = n_empty = n_closed = 0
    for t, b in books.items():
        p = progs.get(t)
        if not p:
            continue
        if b["http"] != 200:
            n_http_fail += 1
            continue
        if status and status.get(t, {}).get("status") not in (None, "active", "open"):
            n_closed += 1
            continue
        yes = levels_from_dollars(b["yes"])
        no = levels_from_dollars(b["no"])
        if not yes or not no:
            n_empty += 1
            continue
        T = float(p["target_size_fp"])
        df = p["discount_factor_bps"] / 10000.0
        P = p["period_reward"] / 10000.0
        sd, ed = iso(p["start_date"]), iso(p["end_date"])
        D = (ed - sd).total_seconds() / 86400.0
        R = (ed - now).total_seconds() / 86400.0
        r = {"t": t, "pool": P, "days": D, "rem": R, "target": T, "df": df,
             "touch_yes": yes[0][0], "touch_no": no[0][0]}
        ry, qy, cy = walk(yes, T, "jul30")
        rn, qn, cn = walk(no, T, "jul30")
        r["eligible"] = ry is not None and rn is not None
        r["depth_yes"], r["depth_no"] = cy, cn
        if r["eligible"]:
            r["ref_yes"], r["ref_no"] = ry, rn
            r["ref_gap_yes"], r["ref_gap_no"] = yes[0][0] - ry, no[0][0] - rn
            fy, _, _ = walk(yes, T, "feb")
            fn, _, _ = walk(no, T, "feb")
            r["field_feb"] = side_total(qy, fy, df) + side_total(qn, fn, df)
            r["field_jul"] = side_total(qy, ry, df) + side_total(qn, rn, df)
        for S in sizes:
            for rule, where in (("feb", "touch"), ("jul30", "touch"), ("jul30", "ref")):
                s = snapshot(yes, no, T, df, rule, S, where)
                key = f"{rule}_{where}_S{int(S)}"
                r[key] = s["snapshot_share"]
                r[key + "_elig"] = s["eligible_with_us"]
                r[key + "_coll"] = s["collateral"]
                r[key + "_usd_period"] = s["snapshot_share"] * P
                r[key + "_usd_day"] = s["snapshot_share"] * P / D if D > 0 else 0.0
        rows.append(r)

    out = {"snapshot_time": now.isoformat(), "n_programs": len(progs), "n_books": len(books),
           "n_http_fail": n_http_fail, "n_closed_status": n_closed, "n_empty_side": n_empty,
           "n_scored": len(rows)}
    el = [r for r in rows if r["eligible"]]
    out["n_eligible_now"] = len(el)
    out["eligible_frac_of_scored"] = len(el) / len(rows) if rows else None
    out["ref_below_touch_frac"] = {
        "yes": sum(1 for r in el if r["ref_gap_yes"] > 0) / len(el) if el else None,
        "no": sum(1 for r in el if r["ref_gap_no"] > 0) / len(el) if el else None,
    }
    out["ref_gap_cents_p50_p90"] = {
        "yes": (q([r["ref_gap_yes"] for r in el], .5), q([r["ref_gap_yes"] for r in el], .9)),
        "no": (q([r["ref_gap_no"] for r in el], .5), q([r["ref_gap_no"] for r in el], .9)),
    }
    out["field_total_jul_over_feb"] = {
        "p10": q([r["field_jul"] / r["field_feb"] for r in el if r["field_feb"] > 0], .1),
        "p50": q([r["field_jul"] / r["field_feb"] for r in el if r["field_feb"] > 0], .5),
        "p90": q([r["field_jul"] / r["field_feb"] for r in el if r["field_feb"] > 0], .9),
    }
    out["field_jul_p10_p50_p90"] = [q([r["field_jul"] for r in el], x) for x in (.1, .5, .9)]
    out["total_pool_eligible"] = sum(r["pool"] for r in el)
    out["total_pool_per_day_eligible"] = sum(r["pool"] / r["days"] for r in el if r["days"] > 0)
    per = {}
    for S in sizes:
        for key in (f"feb_touch_S{int(S)}", f"jul30_touch_S{int(S)}", f"jul30_ref_S{int(S)}"):
            sh = [r[key] for r in rows if r[key + "_elig"]]
            usd_day = [r[key + "_usd_day"] for r in rows if r[key + "_elig"]]
            usd_period = [r[key + "_usd_period"] for r in rows if r[key + "_elig"]]
            d = {
                "n_eligible_with_us": len(sh),
                "share_p10_p50_p90": [q(sh, .1), q(sh, .5), q(sh, .9)],
                "usd_day_p10_p50_p90": [q(usd_day, .1), q(usd_day, .5), q(usd_day, .9)],
                "usd_period_p50": q(usd_period, .5),
                "n_clear_floor_full_uptime": sum(1 for x in usd_period if x >= 1.0),
                "n_clear_floor_half_uptime": sum(1 for x in usd_period if x * 0.5 >= 1.0),
                "sum_usd_day_all_markets": sum(usd_day),
                "sum_collateral_all_markets": sum(r[key + "_coll"] for r in rows if r[key + "_elig"]),
            }
            # greedy capacity: rank by $/day per $ collateral, fixed S per market, floor respected
            cand = sorted((r for r in rows if r[key + "_elig"] and r[key + "_coll"] > 0
                           and r[key + "_usd_period"] >= 1.0),
                          key=lambda r: -r[key + "_usd_day"] / r[key + "_coll"])
            cap = {}
            for K in (100, 500, 1000, 5000, 10000, 100000):
                tot = 0.0; used = 0.0; n = 0
                for r in cand:
                    if used + r[key + "_coll"] > K:
                        continue
                    used += r[key + "_coll"]; tot += r[key + "_usd_day"]; n += 1
                cap[str(K)] = {"usd_day": round(tot, 2), "markets": n, "collateral": round(used, 2)}
            d["greedy_capacity_modelled"] = cap
            per[key] = d
    out["per_policy"] = per
    if a.tickers:
        out["detail"] = {r["t"]: r for r in rows if r["t"] in a.tickers.split(",")}
    txt = json.dumps(out, indent=1, default=float)
    print(txt)
    if a.out:
        open(a.out, "w").write(txt)
        json.dump(rows, open(a.out.replace(".json", "-rows.json"), "w"), default=float)


if __name__ == "__main__":
    main()
