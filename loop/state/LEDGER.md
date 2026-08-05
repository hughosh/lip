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

