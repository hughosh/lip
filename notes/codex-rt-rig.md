The YES/NO sign is correct, but the “confirmed trade-through” population is not. Several bugs can silently invert selection or attach the wrong book and horizon to a trade. Existing rows cannot be repaired reliably because raw websocket messages were not retained.

1. **Side semantics — correct.** `rig.py:279-284`  
   A YES/bid taker buys YES by matching a resting NO bid at the complementary price; a NO/ask taker matches a resting YES bid. Therefore `yes → resting no/no_price` and `no → resting yes/yes_price` are right. Kalshi confirms `bid ≡ yes`, `ask ≡ no` in its [direction specification](https://docs.kalshi.com/getting_started/order_direction).  
   Minimal fix: no semantic change; validate `taker_side` against `taker_book_side` instead of silently treating every non-`"yes"` value as NO.

2. **Trade-through evidence is assigned to the wrong price — corrupts data.** `rig.py:286-295`, especially `294`; stored at `303,310`  
   If pre-best is 49 and a print occurs at 47, the evidence concerns an order resting at **49**. The row instead stores/analyses price 47 and depth at 47. Thus “confirmed” markout receives an extra 2¢ and its queue depth is unrelated to the exhausted level.  
   Minimal fix: represent hypothetical quote episodes separately, with candidate price/depth equal to the pre-trade touch; confirm that candidate only upon a later lower print.

3. **“Necessarily exhausted by fills” is false on Kalshi — corrupts data.** `rig.py:292-295`  
   Under an unmodified price-time book, lower execution means higher executable liquidity is gone. But Kalshi supports maker-side self-trade prevention: the matching engine cancels the maker and continues to worse prices. A lower print can therefore occur without that touch order filling. Cancellation/amendment immediately before the trade, whose delta arrives later, produces the same observation. Kalshi’s [order specification](https://docs.kalshi.com/api-reference/orders/create-order-v2) documents both behaviors.  
   Hidden/iceberg orders are not documented; IOC/FOK/post-only do not otherwise skip external liquidity; taker-side STP stops rather than continues; crossed books cannot persist; combo/RFQ orders ultimately enter their ticker’s public book and do not bypass its priority.  
   Minimal fix: stop describing this as assumption-free; exclude causally ambiguous transitions. Public aggregate data cannot prove “filled rather than canceled.”

4. **Cross-channel attribution race — corrupts data.** `rig.py:167-185,286,396-401`  
   Trade first: normally gets the pre-trade checkpoint. Delta first: if its timestamp equals or exceeds the trade timestamp, strict `<` excludes it, so attribution often still works. If the delta timestamp is lower, it is selected and the post-trade depth/touch is used; if no older checkpoint exists, lines 180-184 explicitly return the post-trade state. Strict `<` also discards legitimate unrelated updates in the same millisecond.  
   The 47/835.23 observation shows that one sweep’s timestamps/interleaving selected its pre-state—probably equal `ts_ms`. It does not generalize because the channels have separate sequencing and no documented relative delivery order.  
   Minimal fix: retain raw events, buffer/reorder by exchange time, and mark equal-time or incomplete-watermark cases unusable.

5. **Clock mixing — corrupts data.** `rig.py:382-398`; horizon clock at `331-353`  
   If local minus exchange time is Δ and delivery latency is L, snapshot time is wrong by `Δ+L`. A mere +100 ms makes a snapshot appear 100 ms “future”; because history scanning stops at the first future entry, later valid exchange-timestamped checkpoints may never be considered. Negative offset can make post-event state appear pre-event. `book_lag_ms` inherits the same error. Forward horizons resolve approximately `−Δ + [0,5]s` from their intended time.  
   Minimal fix: use exchange timestamps exclusively; snapshots without one need an explicit unknown-time boundary, not `time.time()`.

6. **Sequence gaps poison every market — corrupts data.** `rig.py:406-413`  
   Per-`sid` tracking is correct even for 200 markets: sequence is subscription-wide. Therefore a gap means an unknown market delta was lost and the whole subscription must be quarantined/resnapshotted. Current code only increments a counter.  
   Minimal fix: detect sequence before mutation, request fresh snapshots for all subscribed tickers or reconnect, and reject trades until each market initializes. Also clear `yes/no`, history, and sequence on reconnect; lines `459-463` clear only history, allowing trades before replacement snapshots to use stale books.

7. **Weighted SE is invalid — corrupts inference.** `analyse.py:65-77`  
   With independent sweep clusters, variance of a weighted mean depends on squared weights: approximately  
   `n/(n−1) * Σ[wᵢ²(vᵢ−mean)²] / (Σwᵢ)²`.  
   The code uses `Σ[wᵢ residual²]/Σwᵢ/n`, generally understating uncertainty when sweep sizes vary.  
   Minimal fix: cluster-sandwich SE or cluster bootstrap.

8. **Sweep clustering is not an independent-event identifier — degrades data.** `analyse.py:86-92,112-115`  
   Second-floor bucketing splits bursts across `xx:xx:00`, while merging unrelated same-side takers within a second. Both change sample size unpredictably.  
   Minimal fix: use exact matching-engine `ts_ms` for atomic sweeps, or gap-sessionize across second boundaries and report sensitivity.

9. **Additional measurement corruption.**
   - `rig.py:331-353`: forward mids are the live book up to five seconds late; after downtime they become restart-time mids, and pending rows are deleted even before fresh snapshots. Store exchange-timestamped mid history and mark downtime horizons missing.
   - `analyse.py:162-166`: “sweep contracts” bins individual print `size`, not total sweep size. Aggregate sweep size first, then bin.
   - `rig.py:254-266` and `analyse.py:180-183`: `qualifies()` itself correctly requires both sides and walks downward (`214-232`), but `AVG(gate)` averages transition events, not one-second LIP snapshots or time. Use 1 Hz sampling/time-weighted durations.
   - `rig.py:277-278,379,393`: rounding prices to integer cents merges fractional Kalshi levels. Preserve fixed-point prices exactly.
   - `rig.py:361`: explicitly set `use_yes_price:false` or migrate consistently; Kalshi documents a future default flip that would otherwise silently reinterpret every NO level.
