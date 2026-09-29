"""Bounded public-only LIP screen; no account routes, credentials, or writes."""

import concurrent.futures
import datetime as dt
import json
import pathlib
import time
import urllib.error
import urllib.request


ROOT = pathlib.Path(__file__).parent
BASE = "https://external-api.kalshi.com/trade-api/v2"
PREFIXES = [
    ("KXTRUMPAPPROVE", 2),
    ("KXMLBSEASONGAMES", 2),
    ("KXNFLESCALATORREC", 1),
    ("KXNFLESCALATORRECYDS", 1),
    ("KXCPIYOY", 1),
    ("KXTTELITEMATCH", 1),
    ("KXRAIN", 1),
    ("KXGRAMMY", 1),
    ("KXFEATURE", 1),
]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


OPENER = urllib.request.build_opener(NoRedirect)


def get(path):
    started = dt.datetime.now(dt.timezone.utc).isoformat()
    t0 = time.monotonic()
    req = urllib.request.Request(BASE + path, headers={"User-Agent": "lip-public-economic-screen/1"})
    try:
        with OPENER.open(req, timeout=12) as response:
            body = response.read(2_000_001)
            if len(body) > 2_000_000:
                raise ValueError("response over 2MB")
            return {"path": path, "at_utc": started, "latency_ms": round((time.monotonic()-t0)*1000),
                    "status": response.status, "body": json.loads(body)}
    except (urllib.error.URLError, ValueError, TimeoutError) as e:
        return {"path": path, "at_utc": started, "latency_ms": round((time.monotonic()-t0)*1000),
                "error": str(e)}


def read_one(program):
    ticker = program["market_ticker"]
    calls = {"market": get("/markets/" + ticker), "orderbook": get("/markets/" + ticker + "/orderbook")}
    market = calls["market"].get("body", {}).get("market", {})
    event_ticker = market.get("event_ticker")
    if event_ticker:
        calls["event"] = get("/events/" + event_ticker)
        series_ticker = calls["event"].get("body", {}).get("event", {}).get("series_ticker")
        if series_ticker:
            calls["series"] = get("/series/" + series_ticker)
    return {"ticker": ticker, "program": program, "calls": calls}


def main():
    initial = json.loads((ROOT / "programs-initial.json").read_text())
    now = dt.datetime.now(dt.timezone.utc)
    active = [p for p in initial["programs"] if
              dt.datetime.fromisoformat(p["start_date"].replace("Z", "+00:00")) <= now <
              dt.datetime.fromisoformat(p["end_date"].replace("Z", "+00:00"))]
    selected = []
    for prefix, count in PREFIXES:
        choices = [p for p in active if p["market_ticker"].split("-")[0] == prefix]
        choices.sort(key=lambda p: (float(p["period_reward"])/float(p["target_size_fp"]), p["end_date"]), reverse=True)
        selected.extend(choices[:count])
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        rows = list(pool.map(read_one, selected))
    out = {"started_at_utc": now.isoformat(), "completed_at_utc": dt.datetime.now(dt.timezone.utc).isoformat(),
           "source_walk": "programs-initial.json", "selection_rule": "highest period_reward/target_size_fp per listed series prefix, with fixed caps; diverse diagnostic sample, not an optimizer",
           "selected_count": len(rows), "rows": rows}
    (ROOT / "economic-screen-snapshot.json").write_text(json.dumps(out, indent=2) + "\n")
    print(out["completed_at_utc"], len(rows), "rows", sum(bool(v.get("error")) for r in rows for v in r["calls"].values()), "errors")


if __name__ == "__main__":
    main()
