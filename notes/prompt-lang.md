You are choosing the implementation language for a system that is about to be ported off
Python. You OWN this decision — give your own research and rationale, and either sign off on
a recommendation or reject the premise. Do not hedge across options; pick one and defend it.

## What the system does

Two components, one codebase, running against Kalshi (a CFTC-regulated prediction market):

1. MEASUREMENT RIG (built, running in Python today, ~450 lines)
   - One authenticated websocket connection (RSA-PSS SHA-256 signed handshake).
   - Subscribes `orderbook_delta` for ~200 markets plus the whole-exchange `trade` tape.
   - Maintains a live in-memory orderbook per market from snapshot + incremental deltas.
   - Keeps a 5-second ring buffer of book checkpoints per market so each incoming trade can
     be attributed to the book state immediately BEFORE it.
   - On each trade writes a row to SQLite: pre-trade book, size resting at the fill price,
     and a `trade_through` flag.
   - A deferred task resolves forward mid-prices at +1m/+5m/+30m per trade.
   - Observed load today: ~120 trades/minute and a few hundred book deltas/second across
     200 markets. Not high-frequency by equities standards.

2. QUOTING BOT (not yet built)
   - Maintains two-sided resting limit orders at the touch across several markets.
   - Re-posts on fill, re-prices when the reference price drifts, enforces hard inventory
     and capital caps, and has a kill switch that cancels everything.
   - Latency matters only in the sense that a quote stranded N ticks behind the reference
     scores 0.5^N of a liquidity reward. The binding constraint is REACTION TIME TO DRIFT
     measured in seconds, not microseconds. There is no latency race against other
     participants for queue position that we can win.

## Hard requirements
- RSA-PSS SHA-256 signing with MGF1 and salt_length = digest length.
- Mature websocket client with control-frame/ping handling and clean reconnect.
- Embedded persistent store (SQLite is the incumbent; alternatives acceptable).
- Decimal-exact or integer-exact price handling. Prices are cents; sizes arrive as
  fixed-point strings with 2 decimals. Silent float drift in a size accumulator would
  corrupt the orderbook.
- Runs unattended for days on a Mac laptop (darwin/arm64) without leaking memory.
- Single-binary or otherwise trivial deploy is a nice-to-have.

## The question

The stated motive for porting is that Python is "an inefficient language." Interrogate that.

1. Is the port justified AT ALL for this workload? Give the actual bottleneck analysis:
   at ~120 trades/min and a few hundred deltas/sec across 200 markets, is CPython the
   constraint, or is it network I/O and the exchange's own snapshot cadence? If the port is
   not justified on performance grounds, say so bluntly — but then say whether there is a
   DIFFERENT justification that does hold (correctness, deployment, long-running stability,
   concurrency model, type safety over a 24/7 process handling money).
2. If porting: pick ONE language. Rust and Go are the obvious candidates; consider others
   only if you can justify them. Evaluate against the hard requirements above, naming
   specific libraries you would actually use for websockets, RSA-PSS signing, and the
   embedded store, and flag any that are immature or unmaintained.
3. Name the single biggest RISK of the port. The two pieces that must stay semantically
   identical are (a) a "qualifying walk" that accumulates order-book size down from the best
   bid until a target is met, clearing the set if the target is never reached, and (b) the
   `trade_through` inference. A subtle behaviour change in either silently corrupts the
   measurement rather than crashing.
4. Recommend a migration strategy: big-bang rewrite, or port the rig first and keep the
   bot in Python, or a shared-core approach. Justify.
5. State what you would need to see to change your mind.

Be concrete and opinionated. Under 800 words.
