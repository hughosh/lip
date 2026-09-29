# Attended candidate comparison — public snapshot, 2026-09-26

**Narrow preference: `KXFEATURE-26GTA6-DON` for an attended first-fill diagnostic, if a fresh read still supports it.** Its smaller queue makes a fill more plausible during a bounded attended window than the initial `KXTRUMPAPPROVE-26SEP27-E39.0` diagnostic. Neither snapshot establishes a fill probability, a safe future exit, a reward payment, or positive expected value. For read-only book-continuity and displayed exit-depth observation, E39.0 remains the steadier baseline. This is a candidate comparison, not authority to trade.

At 23:50:22–25 UTC both markets were active, had `exchange_index=0`, a 1¢ spread, `quadratic` series fees with multiplier 1, and more than the 1,000-contract target on each full book side. E39.0's $600 period ended September 27 at 14:02 UTC (about 14h 12m after the snapshot); DON's $100 period ended October 1 at 16:47 UTC (about 4d 17h after it). The pool is shared and conditional, not a payout. The [initial screen](economic-screen.md) estimates only $0.87 for a favorable static 12-per-side E39.0 thought experiment, below the $1 payment threshold. There is no comparable measured payout estimate for DON.

| Snapshot measure | E39.0 | DON |
|---|---:|---:|
| YES / NO best bids | 4¢ / 95¢ | 72¢ / 27¢ |
| Displayed YES / NO size at best bid | 8,798.50 / 7,795.23 | 660.45 / 37.00 |
| Displayed 12-contract YES / NO exit VWAP | 4¢ / 95¢ | 72¢ / 27¢ |
| Next NO bid below touch | 80¢, 301 contracts | 23¢, 75 contracts |

A 12-contract quote joining DON's NO touch would sit behind the displayed 37 contracts; on its YES touch it would sit behind 660.45. E39.0 has roughly 7,795–8,799 displayed ahead at its two touches. Queue rank and sell flow were not observed, so these figures do not predict a fill. Both books displayed 12 contracts at each best bid at that instant, but those bids could cancel or be consumed before an owned position is reduced. DON's NO side is particularly fragile: if its 27¢ touch vanished, the next observed bid was 23¢, a 4¢ price step before fees. A 12-contract YES fill at 72¢ also carries $8.64 of entry notional, versus $3.24 for NO at 27¢. A directional position could persist until resolution if reduction fails; DON's expected expiration was December 31, far beyond its program end. E39.0 expected expiration was September 27.

The later [official browser fee observation](browser-fee-observation.json) resolves the screen's direct-fetch HTTP 429 limit for the **numerical schedule**: the official July 2026 PDF displayed general taker fee `0.07 × C × P × (1−P)` at multiplier 1, with fee plus position cost rounded up to a centicent; default maker multiplier is zero. No actual fill debit was observed. Applying that rule solely to selling 12 owned contracts at the *observed* best bids gives illustrative exit proceeds:

| Market / exit side | Gross bid proceeds | Taker fee | Net proceeds |
|---|---:|---:|---:|
| E39.0 YES, 4¢ | $0.4800 | $0.0323 | $0.4477 |
| E39.0 NO, 95¢ | $11.4000 | $0.0399 | $11.3601 |
| DON YES, 72¢ | $8.6400 | $0.1694 | $8.4706 |
| DON NO, 27¢ | $3.2400 | $0.1656 | $3.0744 |

These are hypothetical 12-contract taker exits, not actual fills or expected values. They omit any entry price change, queue outcome, future book movement, reward allocation, and other realized debits. If a zero-maker-fee entry occurred at the same price, the exit fee alone would reduce the proceeds by the displayed amount; an adverse price move could dominate it.

The two NFL subcent books in the wider screen are **not candidates for this runtime**: the current REST book parser explicitly rejects non-integer-cent resting prices (`BOOK_PRICE_GRANULARITY`), while the quoting path represents integer cents. The historical §10.1 selector filters also would not endorse either compared market as a general strategy pick at this snapshot: both programs had elapsed more than 25%; E39.0's roughly 4–5¢ YES midpoint was below the 10¢ floor, and DON's December resolution exceeded the 30-day tenor. The current pilot instead accepts one operator-chosen ticker. This narrow diagnostic preference cannot be read as an optimal market ranking or as qualification for continuous participation.

Evidence: [market and book calculations](economic-screen-analysis.json), [raw public responses](economic-screen-snapshot.json), [program refresh](economic-screen-programs-refresh.json), [official fee observation](browser-fee-observation.json), and local `go/harness/rest/orderbook.go` / `go/harness/num/money.go`. All depth, membership, schedule, and fee classification need fresh verification for any later attended stage.
