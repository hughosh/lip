# Finding ledger

Append-only. Conductor-owned. A finding already disposed of here is closed on sight.

## 2026-08-05T16:12:15Z — lip-2bz scope violation ['loop/state/HEARTBEAT', 'loop/state/STATE.json', 'loop/state/LOCK'] — discarded

## 2026-08-05T16:41:54Z — lip-gmf scope violation ['.claude/settings.json'] — discarded

## 2026-08-05T17:02:21Z — it1 lip-gmf — Per-market q copied from market zero
- **admitted**: SURVIVED every existing gate
- reachability: Under §10.3, six markets quote independently with S=100. A normal one-sided fill in a non-first market can leave market A flat while market B carries -61 contracts and enters REDUCING or SETTLING. This mutation emits B's current ticker, state, and SourceSeq but reports A's q=0, reproducing confident silence about B's inventory. Every driven market in the changed test has the identical +61 q, so its exact-q assertion cannot detect the wrong market index.
- mutation: `harness/risk/monitor.go`

## 2026-08-05T17:02:24Z — it1 lip-gmf — WINDING_DOWN plus SETTLING is untested
- **admitted**: SURVIVED every existing gate
- reachability: With §10.3's six live markets, a one-sided fill can leave q nonzero. H-HALT-3 then permits a concrete deployed sequence: SIGTERM latches global WINDING_DOWN while the process, monitor, and reducers remain alive; at close_time-close_lead (1h), that market transitions REDUCING to SETTLING and retains its reducer until final_lead. The mutation suppresses observation exactly then. The global-state test uses only REDUCING markets, while both new SETTLING transitions use global RUNNING, so neither exercises this reachable conjunction.
- mutation: `harness/risk/monitor.go`

## 2026-08-05T17:44:58Z — lip-4yy ALREADY_SATISFIED, verified by `TestMonitorSamplesInEveryMarketState`

## 2026-08-05T17:48:39Z — lip-52l OPERATOR_ONLY — parked, still owed

## 2026-08-05T17:50:45Z — lip-9r3 OPERATOR_ONLY — parked, still owed

## 2026-08-05T17:53:09Z — lip-afr SPEC_CONFLICT — needs a human

## 2026-08-05T17:55:11Z — lip-428 OPERATOR_ONLY — parked, still owed

## 2026-08-05T17:57:31Z — lip-8a8 SPEC_CONFLICT — needs a human

## 2026-08-05T18:04:33Z — lip-428 OPERATOR_ONLY — parked, still owed

## 2026-08-05T18:36:17Z — it13 lip-ogc — Ceiling parsed quantities causes reducer sign flips
- **admitted**: SURVIVED every existing gate
- reachability: Under §10.3, each of six markets posts 12-contract orders, and H-CO-4 explicitly permits fractional partial fills. The measured size corpus includes the exchange-representable value 0.07. IEEE-754 parsing gives 0.07*100 as 7.000000000000001, so this mutation records a +0.07 YES fill as q=+0.08. The resulting 0.08 NO reducer passes validation against that incorrect local q; if fully filled, the exchange position moves from +0.07 to -0.01. A reducing order has therefore changed q's sign, exactly the H-Q-5a failure mode.
- mutation: `harness/num/qty.go`

## 2026-08-05T18:41:53Z — lip-ogc ALREADY_SATISFIED, verified by `TestEligibleAddingCancelPrecedesFreshRequote`

## 2026-08-05T19:13:02Z — it17 lip-d50 — One-quantum positions become signless
- **admitted**: SURVIVED every existing gate
- reachability: Under §10.3, six markets each rest 12.00-contract orders. The captured exchange data in rig.db contains 1,473 fills of exactly 0.01 contracts, so a one-quantum partial fill is observed rather than hypothetical. From flat, a +0.01 YES fill produces Qty(1), but this mutation makes Sign return 0 while IsFlat correctly remains false. A subsequent ordinary SIGTERM enters WINDING_DOWN; §6.2 can no longer select NO as the reducing side, so no 0.01 reducer is emitted. The process remains alive but cannot drain, violating H-HALT-3 and A4 while leaving inventory unmanaged. The -0.01 path is symmetric.
- mutation: `harness/num/qty.go`

## 2026-08-05T19:26:41Z — it18 lip-xyg — Narrowed sign reverses a deployed full-fill position
- **admitted**: SURVIVED every existing gate
- reachability: Section 10.3 deploys six markets posting S=12.00 contracts per side, and V2-FILL explicitly includes a legal full fill. From flat, one full YES fill therefore produces q=Qty(1200). The mutation narrows the fixed-point quantum count before comparing it; int8(1200) is -80, so Sign reports -1 and section 6.2 selects YES instead of the required NO reducer. The market is already beyond inv_hard=7 and enters REDUCING, yet its supposed reducer is an adding-side order: A4 has no true reducer, A8 is violated, and a fill grows q from +12 to +24 instead of strictly decreasing |q| as H-Q-5a requires. The symmetric -12.00 fill is reversed likewise. The new test's 0 and plus-or-minus-one-quanta inputs, and the older incidental plus-or-minus-seven-quanta inputs, all survive int8 conversion, so they do not expose this reachable fixed-point-width defect.
- mutation: `harness/num/qty.go`

