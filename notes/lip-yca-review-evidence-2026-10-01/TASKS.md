# lip-yca review — working task list (2026-10-01)

Mutable working plan for the review thread. One task in progress at a time.
Status: `[x]` done, `[>]` in progress, `[ ]` todo, `[-]` dropped.

- [x] T1 Orient: bead lip-yca/lip-89x, `notes/reeval-verdict.md`, July 30 terms PDF
      (tracked + clean, read raw), revival terms/fee sections, lip-yca economics evidence
      (historical-cashflow.md, review.md, account-read.json, positions-pnl.json).
- [x] T2 Catalogue scripts and account-read tooling (two delegates); spot-checked
      revmodel.py:18-21, score.py:27, capacity.py:52. No rewards/payout endpoint exists;
      GET-only reader is go/cmd/accountcheck (prebuilt in the candidate-7 stage tools/).
- [x] T3 Stage money verified from account-*.json balances: 98.5802 → 98.4279 → 98.5479
      → 98.5479 → 98.7879 (−0.1523, +0.12, 0, +0.24). funding-arithmetic.json is a
      preflight bound, not a ledger.
- [x] T4 Fees: kalshi.com fee page and PDF answer 429 to non-browser clients; the help
      article defers to the PDF. Verified instead on the account's own fills: taker
      0.07·C·P·(1−P) rounded up to the centi-cent (0.0323 on 12@96c; 0.3968 on 23@44c),
      maker $0 on `quadratic` series. All traded series are `quadratic`, multiplier 1
      (public series endpoint, 2026-10-01). Maker-fee coefficient on
      `quadratic_with_maker_fees` series not verified (third parties say 0.0175); not in scope.
- [x] T5 Universe now: 8,135 active liquidity programs at 2026-10-01T20:02Z, $937,745 pool
      in flight, 4,949 of them $100 pools, median period 7 d, target 1,000 on 8,014.
- [x] T6 July 30 scorer validated: yes share 0.164872 / no 0.164874 / one-tick-below
      0.089843 on the Sep 26 fixture, matching quote-counterfactual.json to 6 figures.
- [x] T7 Sweep done 20:54–21:07Z: 8,135 books, all HTTP 200; 8,067 active; 7,163 eligible.
      Scored (analyse_sweep.py, capacity_cuts.py): S=12 July 30 share p50 0.82%, period $
      p50 $0.84 (< floor); stage markets 1.06–1.20%; ≥1-day/10–90c capacity curve 6–13×
      below the Feb rule. Raw sweep archived as sweep/*.jsonl.gz.
- [x] T7a $7.65 calibration re-run on a scratch copy of probe.db: predicted $7.574 vs
      $7.65 (−1.0%), 37.8% uptime, 2.001% share while up at S=50, pool $1,000, 165.95 h.
- [x] T7b Balance chain: $100.25 at 2026-07-25T04:19Z (probe balance_poll, before any
      ballot fill) − $9.3198 + $7.65 = $98.5802 (2026-09-27 GET) exactly; then the four
      stage deltas to $98.7879 (2026-10-01T10:50Z GET).
- [x] T8 Eligibility fraction from `lip.db` (Aug 13–18, 5-min cadence, immutable read):
      mean 0.848 across 1,271 tickers; 78% of tickers ≥0.95; 8% ≤0.05.
- [x] T9 Harness-envelope arithmetic (`envelope.py`): attended 2 h stages pay $0 by the
      floor; continuous hours to clear $1; round-trip costs; per-market S=12 EV bounds.
- [x] T10 Verified-vs-modelled table and ranked evidence list (review §6–7).
- [-] T11 Authenticated reads: NOT needed and none made. The last authenticated read is the
      candidate-7 accountcheck at 2026-10-01T10:50Z ($98.7879); nothing has traded since;
      no stage program has ended, so no credit can exist yet; no rewards endpoint exists.
      Next informative read: after the stage programs end (Oct 3 / 5 / 12), GET-only.
- [x] T12 `review.md` written (§0–9), sweep section filled from the scored sweep.
- [x] T13 Results comment posted on lip-yca (2026-10-01); status change is Hugh's.
- [x] T14 Memory: new `lip-july30-terms-economics`; supersession pointer added to
      `lip-economics-verdict`; MEMORY.md index updated.
- [ ] T15 Approach fork (handoff step 2): executed the recommended review-and-verdict path
      without a check-in; the separately versioned offline analysis (lip-yca's full
      acceptance set) remains open and its missing inputs are listed in review §7/§9.
