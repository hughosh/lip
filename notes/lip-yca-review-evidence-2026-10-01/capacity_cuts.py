#!/usr/bin/env python3
"""Cuts of the scored sweep (sweep-summary-rows.json): what the greedy allocation picks, and
the capacity curve restricted to programs with periods >= 1 day and >= 0.5 day remaining.
Usage: capacity_cuts.py sweep-summary-rows.json
"""
import json, sys, statistics as st


def q(a, x):
    a = sorted(a)
    return a[min(len(a) - 1, int(x * len(a)))] if a else None


def greedy(rows, key, K):
    cand = sorted((r for r in rows if r[key + "_elig"] and r[key + "_coll"] > 0
                   and r[key + "_usd_period"] >= 1.0),
                  key=lambda r: -r[key + "_usd_day"] / r[key + "_coll"])
    used = 0.0; tot = 0.0; picks = []
    for r in cand:
        if used + r[key + "_coll"] > K:
            continue
        used += r[key + "_coll"]; tot += r[key + "_usd_day"]; picks.append(r)
    return tot, used, picks


def main():
    rows = json.load(open(sys.argv[1]))
    out = {}
    for key in ("jul30_touch_S12", "jul30_touch_S100", "feb_touch_S12"):
        print(f"\n### {key}: top greedy picks at $100 and $1,000 (all periods)")
        for K in (100, 1000):
            tot, used, picks = greedy(rows, key, K)
            print(f"  K=${K}: ${tot:,.0f}/day over {len(picks)} markets, collateral ${used:,.0f}")
            for r in picks[:8]:
                print(f"    {r['t']:40s} pool {r['pool']:6.0f} days {r['days']:6.2f} rem {r['rem']:5.2f} "
                      f"touch {r['touch_yes']:4.1f}/{r['touch_no']:4.1f} share {r[key]:.3f} "
                      f"$/day {r[key+'_usd_day']:7.2f} coll {r[key+'_coll']:6.2f}")
        for label, flt in (("periods >= 1 d and >= 0.5 d remaining", lambda r: r["days"] >= 1 and r["rem"] >= 0.5),
                           ("periods >= 1 d, remaining >= 0.5 d, touch within 10..90c both sides",
                            lambda r: r["days"] >= 1 and r["rem"] >= 0.5 and 10 <= r["touch_yes"] <= 90 and 10 <= r["touch_no"] <= 90)):
            sub = [r for r in rows if flt(r)]
            el = [r for r in sub if r[key + "_elig"]]
            cap = {}
            for K in (100, 500, 1000, 5000, 10000, 100000):
                tot, used, picks = greedy(sub, key, K)
                cap[K] = (round(tot, 2), len(picks), round(used, 2))
            usd_day = [r[key + "_usd_day"] for r in el]
            per = [r[key + "_usd_period"] for r in el]
            d = {
                "n": len(sub), "n_elig": len(el),
                "share_p50": q([r[key] for r in el], .5),
                "usd_day_p10_p50_p90": [q(usd_day, .1), q(usd_day, .5), q(usd_day, .9)],
                "usd_period_p50": q(per, .5),
                "n_clear_floor_full": sum(1 for x in per if x >= 1),
                "n_clear_floor_half": sum(1 for x in per if x * 0.5 >= 1),
                "sum_usd_day": round(sum(usd_day), 2),
                "greedy": cap,
            }
            out[f"{key} | {label}"] = d
            print(f"  [{label}] n={d['n']} elig={d['n_elig']} share p50 {d['share_p50']:.4f} "
                  f"$/day p10/p50/p90 {d['usd_day_p10_p50_p90']} period$ p50 {d['usd_period_p50']:.2f} "
                  f"floor full/half {d['n_clear_floor_full']}/{d['n_clear_floor_half']} sum$/day {d['sum_usd_day']}")
            print("   greedy:", cap)
    json.dump(out, open(sys.argv[1].replace("-rows.json", "-cuts.json"), "w"), indent=1)


if __name__ == "__main__":
    main()
