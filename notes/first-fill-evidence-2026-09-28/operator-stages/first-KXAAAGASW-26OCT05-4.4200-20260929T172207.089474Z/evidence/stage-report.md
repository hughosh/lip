# lip-dwf attended first-fill stage — 2026-09-29, candidate-4, KXAAAGASW-26OCT05-4.4200, S=12

The assistant executed each live step (launch, two SIGTERMs, the restart, disarm) only after Hugh approved its prompt in manual permission mode. Hugh attended throughout and closed the position himself.

## Identity and inputs
- **Binary:** `4d9b8a1a…7771`.
- **Config:** `config.json`; the identity is in `identity.json`.
- **Run 1:** `20260929T17230564MKR`, PID 61891, started 17:23:03Z.
- **Run 2 (retained-latch restart):** `20260929T174616A5YEP`, PID 63470, started 17:46:14Z.
- **Freshness:** at launch the book capture was 57 s old (limit 60), and the preflight account read was complete and flat at $98.5802 (`account-preflight.json`).
- **Market move:** the book had moved to YES 2c / NO 97c, from the 44/54c seen at scope time.

## Observed
1. **Orders placed:** YES 12 @ 2c (`01a0ee31-0c78-70f2-8d7d-6e9583261add`) and NO 12 @ 97c (`01a0ee31-0c78-7333-9ea8-59fdcdd38b71`), both adding.
2. **First owned fill:** at 17:29:01.46Z, NO 12 @ 97c, maker (`is_taker=false`), fee $0.000000, trade `072356e8-c159-ab69-e864-a19875f8ee85`.
3. **Durable first-fill stop:** latch `canary_owned_fill` at ts_ms 1790702942290, RUNNING→WINDING_DOWN (global_stop), market QUOTING→REDUCING.
4. **Adding-order cancellation, verified by the exchange's own record:** DELETE 200 at 17:29:03.03, reduced_by 12. The exchange's terminal time was 17:29:03.027. Six complete lists and two named reads stayed stale for about 1.4 s. The third sweep's named read returned `canceled` and retired the order. That lag produced two false SEV1 SWEEP_INCOMPLETE (a19, a21); details in the sweep-trace lines of harness.log.
5. **Funded reducer:** YES 12 @ 3c (`01a0ee36-82e8-75ed-b1d5-9be14d6ab639`), top of book. **It never filled.**
6. **Operator manual exit:** at 17:44:20Z Hugh sold 12 NO @ 96c (taker, fee $0.0323, order `01a0ee44-7920-77d4-92d2-bae201f4f7c8`); see `operator-manual-close.json`.
   - The harness raised SEV1 FOREIGN_FILL (a23) and SEV1 POSITION_DRIFT (a24), then cancelled its reducer (DELETE 200 at 17:44:25.1, reduced_by 12; one more lag SEV1, a27).
   - It then went REDUCING→IDLE and WINDING_DOWN→DRAINED.
7. **First exit:** SIGTERM 17:45:15Z, exit status **0** (exit-status.txt).
8. **Post-exit account-wide read**, 17:45:26–28Z: complete and flat, zero open orders and zero nonzero positions, $98.4279 (`account-after-drain.json`).
9. **Retained-latch restart**, `-resume 'first-owned-fill stop validation'`: STARTING→WINDING_DOWN (halt_latch)→DRAINED in 0.75 s. It adopted 0 orders, placed 0, raised no SEV1, and saw 1 foreign fill; SEV2 PNL_BASIS_UNAVAILABLE was expected after the manual trade. SIGTERM 17:46:56Z, exit status **0**.
10. **Post-restart account-wide read**, 17:47:03–05Z: complete and flat, $98.4279 (`account-after-restart.json`). `live_ok` removed at 17:47:13Z; the latch is retained.

**Net P&L:** −$0.1523, from buying 12 NO @ 0.97 as a maker with no fee and selling 12 NO @ 0.96 as a taker for a $0.0323 fee.

## Not demonstrated
- **A harness-completed reduction fill.** The funded reducer rested for 15 minutes and did not fill before the operator closed the position manually.
- **Human receipt of ntfy SEV1s.** Hugh saw the 30-second WINDING_DOWN heartbeats; SEV1 receipt timing was not recorded.
- **Nothing here** covers R2, repeated cycles, turnover or profitability.

## lip-kaf production answer
In production the named-order read (`GET /portfolio/orders/{id}`) lags a cancel by about 1.4 s, just as the resting list does. DELETE retries, by contrast, got an immediate 404. The H-ORD-4c fix is safe (it retired the order only on the exchange's terminal record, about 1.5 s after the cancel), but SEV1 still pages falsely during that window. Follow-up is tracked in a new bead.
