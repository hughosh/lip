# A1–A14 enforcement audit

This is a source/behavior map for the one-market runtime, not trading authority.
The default gate and selected controls are recorded in the session verification
receipt. Named controls below are experiments, not claims that every defect in
a class is impossible. Existing structural enforcement is retained; it is not
reimplemented as a second every-tick rule engine.

| Row | Last production enforcement seam | Composed evidence / discriminating control |
|---|---|---|
| A1 maker-only | `rest.CreateOrder` private payload; `Create` serialization | `TestAFullTickPlacesTheExitAtTheExternalTouch`; `M5a`, `M5b` |
| A2 no taker fees | `Portfolio.ApplyFills` → owner portfolio stop before canary | `TestATakerFillOnTheCanaryKeepsTheStrongerCause`; `M-2T6-CANARYFIRST` |
| A3 no self-cross | `quote.CheckPlacement` and owner dispatch repricing against every possibly live opposite order | `TestFinalBuildCannotCrossAnyPossiblyLiveOppositeOrder` covers in-flight, UNKNOWN and unconfirmed cancel; `M-DGG-A3-FINAL-PRICE` |
| A4 funded exit, including SETTLING | owner sizing/recap → cancel confirmation → fresh portfolio read → reserved dispatch; F5 retained REST authority | `TestTheSettlingMarketRecapsAnOversizedReducerWithoutAGateFailure`, `TestConfirmedAbsentReducerWaitsForFreshPosition`, `TestOwnerF5ReducerSweepResizeRefreshAndRecovery`; `M-Y3Q-GATESTOPCAP`, `M-F5-STALE-WS` |
| A5 observation survives stop | independent monitor, source sequence and `A5Tracker` | `TestM1_MonitorKeepsSamplingAfterGlobalStateLeavesRunning`, `TestM14_FrozenOwnerPublicationIsReportedStale`; `M1`, `M14` |
| A6 attributable position disagreement | complete current-generation positions → `PollRecord` → owner `recordPositionDisagreements` → durable anomaly journal | `TestComposedToleranceDriftReducesOnlyAfterTwoCompletePolls`, `TestComposedHardDriftLatchesSpecificCauseAndKeepsReducer`, `TestFirstCompletePositionDisagreementIsDurablyAttributable`; `M-DGG-A6-JOURNAL` |
| A7 capital at dispatch | aggregate `exposures()` includes every in-flight and unresolved request; `checkPlacementCapital` admits the final body against the remaining role-specific budget | `TestCapitalDispatchSecondOppositeBuildCannotSpendInflightBudget`, late-price and same-coid retry controls; `M-DGG-A7-CANDIDATE`, `M-DGG-A7-RETRY-ONCE` |
| A8 adding retired on REDUCING | owner `evaluate`, role-aware cancel/sweep, retry authority | `TestInventoryHardBreachSweepsAddingAndKeepsCappedReducerAndMonitor`, F5 reducer scenario, stopped-retry test |
| A9 durable, truthful state causes | `Controller.CommitStop` before `Advance`; `shutdown.mirrorGlobal/mirrorMarket` | `TestEveryGlobalTransitionRecordsItsOwnCause`, hard-drift and signal composition; `M-XDQ-TRIGGER`, `M-SD-ORDERING` |
| A10 bounded same-coid recovery | REST bounded idempotent attempts; owner UNKNOWN maximum prevents a fresh coid | `TestUnknownCreateComposesThroughDispatcherStoreAndOwner`, `TestScenarioExchangeCoidIdempotencySurvivesClientRestart`; `M-F12-ATTEMPT-BOUND`, `M-F12-OWNER-ESCALATION` |
| A11 aggregate caps | `restingOn`, `exposures`, pending ledger, every in-flight request; positive coid identity deduplicates a listed order before its worker response | `TestAnAckedOrderOccupiesTheAggregateBeforeAnyOrdersWalk`, UNKNOWN composition, unconfirmed reducer tests; `M-3AF-ACKGAP`, `M-DGG-A11-LISTED-INFLIGHT` |
| A12 reducer cannot flip sign | `targetSize` subtracts aggregate; same-coid retry counts once; post-cancel newer position truth | `TestConfirmedAbsentReducerWaitsForFreshPosition`, F5 resize, `TestReducerRetryCountsItsOwnUnknownOnceAndKeepsReservedCancel`; `M-Y3Q-GATESTOPCAP` |
| A13 current independent authority | opaque generation tokens, Gate freshness, owner role-specific pricing at decision and dispatch | disconnect and F5 composition, `TestOwnerF5SnapshotRequiresEveryNewPortfolioWalk`; `M-W-TRUTH`, `M-F5-OLD-TRUTH`, `M-F5-ALLOW-ADDING` |
| A14 latch monotonicity | startup reads latch before exchange; held stop blocks adding before retrying persistence | `TestASetLatchRefusesWithoutResumeAndWindsDownWithIt`, `TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs`; `M-3AF-RESUME`, `M-VXO-HOLD` |

A4's real selected-shard spendable funds cannot be established by local
arithmetic or aggregate account balance. `lip-wif` remains a real account and
current-market conformance prerequisite; no local matrix or test waives it.
During an outage the safe response can preserve an already resting reducer
while withholding a new price until independent authority returns. An intended
but unfundable order never counts as a working exit.

A8 is a cancel-and-confirm obligation, not instantaneous erasure of an order
which the exchange may still fill. A10 follows §7.2: the bounded idempotent
same-coid recovery sequence is allowed; starting a new coid for UNKNOWN is not.
The source wording must be read with those lifecycle clauses rather than as a
claim that a network response can retroactively remove exposure.
