#!/usr/bin/env python3
"""V5 of notes/harness-spec.md §17: the negative control -- the gate on the gate.

A gate that has never been shown to fail is not evidence. This deliberately
breaks the harness, one invariant at a time, and checks that a NAMED test
notices. Any mutation that survives every gate is a defect in the verification,
and the harness does not ship until the gate is strengthened -- unless the
mutation is positively established to be behaviourally inert, argued explicitly
and never assumed.

Each mutation is applied to a pristine copy of lip/go, so runs cannot
contaminate each other or the working tree. A replacement that fails to apply is
a hard error: a no-op mutation would otherwise be silently recorded as
"survived", which is the exact false-negative this script exists to prevent.

Mutations must be SEMANTIC. A mutation caught only because it left an import
unused tests the Go compiler, not the gate -- so where a mutation needs a new
import, it patches the import block too, and the resulting tree must COMPILE.
A mutation that fails to build is reported as such and does not count as caught.

Usage:
    python harness_negative_control.py [--out ../notes/harness-negative-control.md]
                                       [--only m01,m14]
"""
from __future__ import annotations

import argparse
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
LIP = HERE.parent
GO_SRC = LIP / "go"
GO_BIN = "/usr/local/bin/go"

PKGS = ["./harness/...", "./cmd/harness/..."]

STRICT_COID_BLOCK = '\techoedRaw, ok := rec["client_order_id"]\n\tif !ok {\n\t\treturn ack{}, fmt.Errorf("2xx with no client_order_id: the measured "+\n\t\t\t"acknowledgement carries one, and without it this response cannot "+\n\t\t\t"be tied to the order we sent (%q)", coid)\n\t}\n\tif isJSONNull(echoedRaw) {\n\t\treturn ack{}, fmt.Errorf("2xx with a null client_order_id (we sent %q)",\n\t\t\tcoid)\n\t}\n\techoed := scalar(echoedRaw)\n\tif echoed == "" {\n\t\treturn ack{}, fmt.Errorf("2xx with an empty client_order_id (we sent %q)",\n\t\t\tcoid)\n\t}\n\tif echoed != coid {\n\t\treturn ack{}, fmt.Errorf("2xx echoes client_order_id %q but we sent "+\n\t\t\t"%q; this response is about a different order", echoed, coid)\n\t}'

# (id, mutation name, [(relpath, old, new), ...], expected catching test)
#
# Every `old` must appear EXACTLY ONCE in its file.
MUTATIONS = [
    ("M1",
     "break the monitor loop when global state leaves RUNNING "
     "-- probebot.py's exact defect",
     [
         ("cmd/harness/monitor.go",
          '\t"lip/harness/risk"\n',
          '\t"lip/harness/quote"\n\t"lip/harness/risk"\n'),
         ("cmd/harness/monitor.go",
          "\t\t\tnow := m.now()\n\t\t\tsnap := m.src.Load()\n",
          "\t\t\tnow := m.now()\n\t\t\tsnap := m.src.Load()\n"
          "\t\t\tif snap != nil && snap.Global != quote.Running {\n"
          "\t\t\t\treturn\n\t\t\t}\n"),
     ],
     "TestM1_MonitorKeepsSamplingAfterGlobalStateLeavesRunning"),

    ("M14",
     "revert A5 to row freshness instead of source advancement "
     "-- fresh rows about a frozen world",
     [
         ("harness/risk/invariant.go",
          "\tif res.Stale {\n\t\treturn\n\t}\n",
          "\t// M14: a row was written, so call it fresh.\n"),
     ],
     "TestM14_FrozenOwnerPublicationIsReportedStale"),

    ("M14b",
     "monitor never marks a frozen source stalled "
     "-- the SEV1 OWNER_STALLED path is removed",
     [
         ("harness/risk/monitor.go",
          "\t} else if now-m.lastAdvance >= stallAfter {\n\t\tm.stalled = true\n",
          "\t} else if false {\n\t\tm.stalled = true\n"),
     ],
     "TestM14_FrozenOwnerPublicationIsReportedStale"),

    ("M7",
     "drop the size_R cap so a reducing fill can overshoot past flat",
     [
         ("harness/num/qty.go",
          "\tif derivedFrom != 0 && count > derivedFrom.Abs() {",
          "\tif false {"),
     ],
     "TestValidateCountRejectsOvershoot"),

    # Expected to SURVIVE, and that is NOT a verification defect. Argued, not
    # assumed, per §17 V5: `float64(q)/100 == 0.0` is equivalent to `q == 0`
    # for every reachable q. Qty is an int64; float64 represents every int64 of
    # magnitude < 2^53 exactly, and the smallest nonzero |Qty| of 1 maps to
    # 0.01 -- eleven orders of magnitude above float64's resolution near zero.
    # Verified exhaustively over q in +/-5,000,000 plus the +/-2^52 and
    # +/-(2^53 - 1) extremes: zero divergences.
    #
    # HR-026's residue arises from ACCUMULATING fractional contracts in
    # float64, not from comparing an already-quantized value. Qty forecloses
    # that by construction: there is no float64 accumulator to leave a residue
    # in, and a mutation that reintroduced one would be a type change that does
    # not compile. That the invariant is enforced by the type system rather
    # than by a test is the stronger outcome, and this row records that it was
    # checked rather than assumed.
    ("M26",
     "compare a quantized quantity as float64 instead of the exact quantum",
     [
         ("harness/num/qty.go",
          "func (q Qty) IsFlat() bool { return q == 0 }",
          "func (q Qty) IsFlat() bool { return q.Float() == 0.0 }"),
     ],
     "inert"),

    ("M26a",
     "quantize by truncation instead of half-away-from-zero "
     "-- a sub-quantum size silently becomes flat",
     [
         ("harness/num/qty.go",
          "\t\treturn Qty(math.Floor(scaled + 0.5))",
          "\t\treturn Qty(math.Floor(scaled))"),
     ],
     "TestQtyRoundTripAndFormat"),

    # ------------------------------------------------------------------
    # harness/rest, round 4. Every entry below was a SURVIVOR or a defect
    # found by the codex red-team pass recorded in loop/out-04-rest.md, and
    # each is here rather than in a shell transcript because a hand-run
    # mutation is evidence today and a ratchet never. Two of them (M-R-POST,
    # M-R-200) survived all 68 tests at the time they were found.
    #
    # The original exported-field form of M-R-POST -- `body.PostOnly = false`
    # in Create -- is NOT listed: CreateOrder no longer has that field, so it
    # does not compile. It is recorded as SUPERSEDED in
    # notes/harness-negative-control.md, not as caught. Its compiling
    # replacement mutates the private wire value after validation and
    # immediately before marshal, which is the same defect expressed where the
    # defect can still exist.
    # ------------------------------------------------------------------

    ("M-R-200",
     "the live transport reports every response as HTTP 200 "
     "-- the H-ORD-2b recovery 409 becomes a fake ack",
     [
         ("harness/rest/client.go",
          "\treturn Response{Status: resp.StatusCode, Body: raw}, nil",
          "\treturn Response{Status: http.StatusOK, Body: raw}, nil"),
     ],
     "TestHTTPDoerPassesEveryStatusThroughUnchanged"),

    ("M-R-REDIR",
     "follow redirects on the live transport "
     "-- a 307 preserves the method, so a redirected create is a second create",
     [
         ("harness/rest/client.go",
          "\t\t\tCheckRedirect: func(*http.Request, []*http.Request) error {\n"
          "\t\t\t\treturn http.ErrUseLastResponse\n\t\t\t},\n",
          ""),
     ],
     "TestHTTPDoerDoesNotFollowRedirects"),

    ("M5a",
     "post_only false on the completed wire body, after validation "
     "-- H-Q-3 with the structural guarantee removed at the last instant",
     [
         ("harness/rest/write.go",
          "\tpayload, err := json.Marshal(w)",
          "\tw.PostOnly = false\n\tpayload, err := json.Marshal(w)"),
     ],
     "TestPostOnlyIsStructural"),

    ("M5b",
     "self-trade prevention `maker` on the completed wire body "
     "-- cancels our RESTING order on a self-match (H-CO-5)",
     [
         ("harness/rest/write.go",
          "\tpayload, err := json.Marshal(w)",
          '\tw.SelfTradePrevention = "maker"\n\tpayload, err := json.Marshal(w)'),
     ],
     "TestPostOnlyIsStructural"),

    ("M-R-COID",
     "dispatch under a foreign coid "
     "-- H-ORD-2b's same-coid recovery becomes unavailable in principle",
     [
         ("harness/rest/write.go",
          "\tpayload, err := json.Marshal(w)",
          '\tw.ClientOrderID = "someone-elses-coid"\n'
          "\tpayload, err := json.Marshal(w)"),
     ],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-COUNT",
     'dispatch "0.00" -- H-CO-4b says it is never sent',
     [
         ("harness/rest/write.go",
          "\tpayload, err := json.Marshal(w)",
          '\tw.Count = "0.00"\n\tpayload, err := json.Marshal(w)'),
     ],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-ACKCOID",
     "accept a create acknowledgement whose echoed client_order_id is not ours "
     "-- another order's fill state is read onto ours, and a lying zero-count "
     "ack erases a live order and licenses a replacement",
     [
         ("harness/rest/write.go",
          "\tif echoed != coid {",
          "\tif false {"),
     ],
     "TestOnlyObservedAckShapesNarrowMaxLive"),

    # Expected to SURVIVE, and argued rather than assumed, per this file's own
    # doctrine. Neutralising the ABSENT-key branch alone changes NO SAFETY
    # BEHAVIOUR -- classification, retry count, MaxLive and reconciliation are
    # all identical with and without it. Only the diagnostic text differs, so
    # this is not completely behaviour-inert, it is safety-inert:
    # with the key absent, `echoedRaw` is the zero RawMessage, `isJSONNull`
    # reports false on it, `scalar` returns "", and the immediately following
    # `echoed == ""` check rejects the acknowledgement anyway. The branch earns
    # its place by naming the failure precisely in the error text, not by being
    # the thing that stops it -- the nonempty and equality checks are, and both
    # are ratcheted (M-R-ACKCOID above, plus the absent/null/empty cases in
    # TestOnlyObservedAckShapesNarrowMaxLive, which pass under this mutation
    # because the outcome is unchanged).
    #
    # Found by this script, not by hand: the hand-run mutation ledger for round
    # 4 never applied this one, and it was reported as a permanent ratchet on
    # the strength of the others. That is exactly the false confidence the
    # negative control exists to strip out.
    ("M-R-ACKCOIDPRESENT",
     "drop the absent-key branch of the acknowledgement coid check",
     [
         ("harness/rest/write.go",
          '\techoedRaw, ok := rec["client_order_id"]\n\tif !ok {',
          '\techoedRaw, ok := rec["client_order_id"]\n\tif !ok && false {'),
     ],
     "inert"),

    ("M-R-NULLTAKER",
     "decode is_taker into a bool instead of a pointer "
     "-- JSON null silently reads as false and H-ORD-8 fails open",
     [
         ("harness/rest/read.go",
          '\tif isTaker == nil {\n'
          '\t\treturn Fill{}, fmt.Errorf("fill %s has a null is_taker; H-ORD-8 must "+\n'
          '\t\t\t"never fail open, and null is not false", f.TradeID)\n\t}',
          "\tif isTaker == nil {\n\t\tvar f bool\n\t\tisTaker = &f\n\t}"),
     ],
     "TestNullIsTakerIsRefused"),

    ("M-R-NULLBAL",
     "treat a null balance as zero "
     "-- a held position is left with no funded reducer (H-CAP-8)",
     [
         ("harness/rest/read.go",
          '\tif cents == nil {\n'
          '\t\treturn Balance{}, fmt.Errorf("balance is null; null is not zero, and " +\n'
          '\t\t\t"a zero balance would leave a held position with no funded reducer")\n\t}',
          "\tif cents == nil {\n\t\tvar z int64\n\t\tcents = &z\n\t}"),
     ],
     "TestNullBalanceIsRefused"),

    ("M-R-SWEEP",
     "filter the verifying read through Ours() before matching requested ids "
     "-- a requested order with an unparseable coid reads as absent",
     [
         ("harness/rest/cancel.go",
          "\t\tfor _, o := range read.Orders {",
          "\t\tfor _, o := range read.Ours() {"),
     ],
     "TestSweepMatchesRequestedIDBeforeOwnership"),

    ("M-R-PAGE",
     "accept a page with no cursor key as a terminal page "
     "-- `200 {}` becomes a complete, empty portfolio truth",
     [
         ("harness/rest/page.go",
          '\t\treturn page{}, fmt.Errorf("%s: cursor field %q is absent; every "+\n'
          '\t\t\t"measured page carries it, present but empty when terminal, so an "+\n'
          '\t\t\t"absent cursor is a malformed response and not an empty account",\n'
          "\t\t\tep.Path, ep.CursorField)",
          '\t\tours = json.RawMessage(`""`)'),
     ],
     "TestMalformedPageIsNeverAnEmptyAccount"),

    ("M-R-WALKZERO",
     "make WalkComplete the zero value again "
     "-- an unpopulated or closed-channel Walk replaces state with nothing",
     [
         ("harness/rest/page.go",
          "func (w Walk) Replaces() bool { return w.Outcome == WalkComplete }",
          "func (w Walk) Replaces() bool {\n"
          "\treturn w.Outcome != WalkRewound && w.Outcome != WalkFailed\n}"),
     ],
     "TestZeroValuedResultsReplaceNothing"),

    # --- audit round 2: the six late-wire companions that were hand-run and
    # reported as permanent, but never installed here. ---------------------

    ("M-R-COUNTNEG",
     "dispatch a negative count -- H-CO-4b requires strictly positive",
     [("harness/rest/write.go", "\tpayload, err := json.Marshal(w)",
       '\tw.Count = "-1.00"\n\tpayload, err := json.Marshal(w)')],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-COUNTOVER",
     "dispatch 99 contracts regardless of the derived bound "
     "-- one order at 99c exposes $98.01 of a $100 account",
     [("harness/rest/write.go", "\tpayload, err := json.Marshal(w)",
       '\tw.Count = "99.00"\n\tpayload, err := json.Marshal(w)')],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-PRICEBAD",
     "dispatch a price outside the tradable range",
     [("harness/rest/write.go", "\tpayload, err := json.Marshal(w)",
       '\tw.Price = "1.9900"\n\tpayload, err := json.Marshal(w)')],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-TIF",
     "dispatch immediate_or_cancel instead of good_till_canceled "
     "-- a post_only IOC order rests for no time at all and scores nothing",
     [("harness/rest/write.go", "\tpayload, err := json.Marshal(w)",
       '\tw.TimeInForce = "immediate_or_cancel"\n\tpayload, err := json.Marshal(w)')],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-SIDEFLIP",
     "flip the wire side after H-CO-1 has been applied "
     "-- the order lands on the opposite leg of the book",
     [("harness/rest/write.go", "\tpayload, err := json.Marshal(w)",
       "\tw.Side = Bid\n\tpayload, err := json.Marshal(w)")],
     "TestCreateOrderPayloadIsByteForByte"),

    ("M-R-TICKER",
     "dispatch against a different market than the one intended",
     [("harness/rest/write.go", "\tpayload, err := json.Marshal(w)",
       '\tw.Ticker = "OTHER-TICKER"\n\tpayload, err := json.Marshal(w)')],
     "TestCreateOrderPayloadIsByteForByte"),

    # --- and the F1 defect itself, which had no permanent mutation at all ---

    ("M-R-ACKMALFORMED",
     "treat a malformed acknowledgement as an ACK with nothing live "
     "-- a live 12-lot reads as MaxLive=0, the requote ladder replaces it, "
     "24 contracts and $23.76",
     [("harness/rest/write.go",
       "\t\t\t\tres.Outcome = CreateUnknown\n"
       "\t\t\t\tres.MaxLive = requested\n"
       "\t\t\t\tres.Err = err\n"
       "\t\t\t\tcontinue // same-coid recoverable",
       "\t\t\t\tres.Outcome = CreateAcked\n"
       "\t\t\t\tres.MaxLive = 0\n"
       "\t\t\t\tres.Err = err\n"
       "\t\t\t\treturn res")],
     "TestOnlyObservedAckShapesNarrowMaxLive"),

    ("M-R-ACKOPTIONAL",
     "revert the acknowledgement coid check to the pre-audit optional form "
     "-- checked only when nonempty, so absent, null and empty all pass",
     [("harness/rest/write.go", STRICT_COID_BLOCK,
       '\tif echoed := scalar(rec["client_order_id"]); echoed != "" && echoed != coid {\n'
       '\t\treturn ack{}, fmt.Errorf("2xx echoes client_order_id %q but we sent "+\n'
       '\t\t\t"%q; this response is about a different order", echoed, coid)\n\t}')],
     "TestOnlyObservedAckShapesNarrowMaxLive"),
    # -----------------------------------------------------------------------
    # lip-fq7 -- harness/wsx and the ownership wiring
    # -----------------------------------------------------------------------

    ("M-W-PONG",
     "ignore the pong timeout and keep the session alive "
     "-- a peer that is gone while the TCP connection is still established",
     [
         ("harness/wsx/session.go",
          "\t\tcase <-pongDue:\n\t\t\tclearPing()\n"
          "\t\t\treturn fmt.Errorf(\"no pong within pong_timeout_s %v: the peer is \"+\n"
          "\t\t\t\t\"gone while the connection is still established, which a read \"+\n"
          "\t\t\t\t\"deadline alone would take read_deadline_s to notice\",\n"
          "\t\t\t\ts.p.PongTimeout)\n",
          "\t\tcase <-pongDue:\n\t\t\tclearPing()\n"),
     ],
     "TestSupervisorPingPongReadDeadlineLadder"),

    ("M-W-CLEAN",
     "return from a CLEAN disconnect before quarantining "
     "-- the shadow rig's correct behaviour, which is fatal in a trader",
     [
         ("harness/wsx/gate.go",
          "\tif !g.connected {\n\t\treturn DisconnectEffects{}\n\t}\n",
          "\tif !g.connected {\n\t\treturn DisconnectEffects{}\n\t}\n"
          "\tif clean {\n\t\treturn DisconnectEffects{}\n\t}\n"),
     ],
     "TestEveryDisconnectQuarantinesUntilFreshSnapshotAndAllPortfolioTruth"),

    ("M-W-RECON",
     "let a fresh snapshot authorise placement without post-disconnect "
     "portfolio reconciliation",
     [
         ("harness/wsx/gate.go",
          "\t\tif !g.truthOK[k] || g.truthGen[k] != g.gen {\n\t\t\treturn false\n\t\t}\n",
          "\t\tif false {\n\t\t\treturn false\n\t\t}\n"),
     ],
     "TestEveryDisconnectQuarantinesUntilFreshSnapshotAndAllPortfolioTruth"),

    ("M-W-GRAN",
     "stop rejecting a fractional-cent BOOK price (H-CO-3a)",
     [
         ("harness/wsx/frame.go",
          "\tif _, exact := rest.CentsExact(p4); !exact {\n",
          "\tif _, exact := rest.CentsExact(p4); !exact && false {\n"),
     ],
     "TestFractionalBookPriceNeverReachesCore"),

    ("M-W-POS",
     "preserve q_local instead of assigning q_exch on a complete poll "
     "-- argue with the exchange (H-POS-1)",
     [
         ("harness/risk/position.go",
          "\t\tif remote == 0 {\n\t\t\tdelete(p.q, t)\n\t\t} else {\n"
          "\t\t\tp.q[t] = remote\n\t\t}\n",
          "\t\t// M-W-POS: keep the local reading.\n"),
     ],
     "TestCompletePositionPollOverwritesLocalAndRecordsAgreements"),

    ("M-W-OWN",
     "bypass the ownership-ledger classification, so every fill is ours "
     "(H-ORD-9)",
     [
         ("harness/risk/position.go",
          "\t\tif owned[i] == OwnershipForeign {\n",
          "\t\tif false && owned[i] == OwnershipForeign {\n"),
     ],
     "TestFillOwnershipUsesOrderIDLedgerAndForeignStopsGlobally"),

    ("M-W-TAKER",
     "disable the is_taker / fee>0 detector on our own fills "
     "(H-ORD-8 and S2 together)",
     [
         ("harness/risk/position.go",
          "\t\tif f.IsTaker || f.Fee > 0 {\n",
          "\t\tif false {\n"),
     ],
     "TestOwnedTakerOrPositiveFeeStopsGlobally"),

    ("M-W-TRUTH",
     "treat positions freshness alone as all portfolio truth "
     "-- orders and fills may be arbitrarily stale (A13)",
     [
         ("harness/wsx/gate.go",
          "\tfor k := Truth(0); k < truthCount; k++ {\n",
          "\tfor k := Truth(0); k <= TruthPositions; k++ {\n"),
     ],
     "TestAnyStalePortfolioEndpointStopsAllPlacementButNotCancel"),

    ("M-W-DISC60",
     "disable the disconnect-duration reduction (F4)",
     [
         ("harness/wsx/supervisor.go",
          "\t\tif !*reduceSent && down >= s.p.DisconnectReduce {\n",
          "\t\tif false && !*reduceSent && down >= s.p.DisconnectReduce {\n"),
     ],
     "TestDisconnectThresholdReducesWithoutStoppingRESTOrSupervisor"),

    ("M-W-QUIET",
     "disable the per-market quiet-feed detector (F5) "
     "-- a wedged single market while the socket stays healthy",
     [
         ("harness/wsx/gate.go",
          "\t\t\tif now.Mono-m.lastFrame <= g.p.Quiet {\n",
          "\t\t\tif now.Mono-m.lastFrame <= g.p.Quiet || true {\n"),
     ],
     "TestQuietMarketAloneReducesAndResnapshots"),
    # -----------------------------------------------------------------------
    # lip-fq7 audit round 2 -- the four reachable safety failures green gates
    # missed, plus the seal on the opening path
    # -----------------------------------------------------------------------

    ("M-W-OUTAGE",
     "return a zero token on disconnect, so every portfolio read taken during "
     "an outage is discarded and position monitoring goes blind",
     [
         ("harness/wsx/gate.go",
          "\treturn DisconnectEffects{\n"
          "\t\tToken:      ReconcileToken{gen: g.gen, valid: true},\n"
          "\t\tResetBooks: !clean,\n",
          "\treturn DisconnectEffects{\n"
          "\t\tToken:      ReconcileToken{},\n"
          "\t\tResetBooks: !clean,\n"),
     ],
     "TestPortfolioTruthAppliesDuringWebsocketOutage"),

    ("M-W-COREACK",
     "ignore core's refusal of a book frame and certify the book anyway",
     [
         ("harness/wsx/gate.go",
          "\tif err := handle(); err != nil {\n",
          "\tif err := handle(); false {\n"),
     ],
     "TestCoreRejectedBookFrameCannotUnlockOrRefreshGate"),

    ("M-W-POLLORDER",
     "read the authoritative position BEFORE the fills",
     [
         ("harness/wsx/portfolio.go",
          "\t\tread.fillsAt = p.clk.Now()\n"
          "\t\tread.fills = p.src.Fills(ctx, \"\", time.Time{})\n"
          "\t\tread.ordersAt = p.clk.Now()\n"
          "\t\tread.orders = p.src.Orders(ctx, \"\", rest.StatusResting)\n"
          "\t\tread.positionsAt = p.clk.Now()\n"
          "\t\tread.positions = p.src.Positions(ctx)\n",
          "\t\tread.positionsAt = p.clk.Now()\n"
          "\t\tread.positions = p.src.Positions(ctx)\n"
          "\t\tread.fillsAt = p.clk.Now()\n"
          "\t\tread.fills = p.src.Fills(ctx, \"\", time.Time{})\n"
          "\t\tread.ordersAt = p.clk.Now()\n"
          "\t\tread.orders = p.src.Orders(ctx, \"\", rest.StatusResting)\n"),
     ],
     "TestPortfolioPollReadsFillsBeforeAuthoritativePosition"),

    ("M-W-POSORDER",
     "apply the authoritative position BEFORE the fills, so a fill already in "
     "q_exch is counted twice and the reducer can flip the sign of q",
     [
         ("harness/wsx/portfolio.go",
          "\tapplyOrders(g, pf, bind, read, &eff)\n"
          "\tapplyFills(g, pf, own, read, mode, &eff)\n"
          "\tapplyPositions(g, pf, read, p, &eff)\n",
          "\tapplyPositions(g, pf, read, p, &eff)\n"
          "\tapplyOrders(g, pf, bind, read, &eff)\n"
          "\tapplyFills(g, pf, own, read, mode, &eff)\n"),
     ],
     "TestAuthoritativePositionIsFinalAfterSameCycleFill"),

    ("M-W-TRUTHSTAMP",
     "stamp every endpoint's freshness with the cycle's COMPLETION time, so a "
     "slow fills walk reads as current for its own duration",
     [
         ("harness/wsx/portfolio.go", "read.fillsAt)", "read.completedAt)"),
         ("harness/wsx/portfolio.go", "read.ordersAt)", "read.completedAt)"),
         ("harness/wsx/portfolio.go", "read.positionsAt)", "read.completedAt)"),
     ],
     "TestEndpointTruthAgeStartsWhenItsWalkStarts"),

    ("M-W-TRUTHFORGE",
     "export noteTruth, so any caller can declare a portfolio endpoint "
     "reconciled without a complete walk -- SetActionable with a longer name",
     [
         ("harness/wsx/gate.go",
          "func (g *Gate) noteTruth(kind Truth, tok ReconcileToken, now Stamp) bool {\n",
          "func (g *Gate) NoteTruth(kind Truth, tok ReconcileToken, now Stamp) bool {\n"
          "\treturn g.noteTruth(kind, tok, now)\n}\n\n"
          "func (g *Gate) noteTruth(kind Truth, tok ReconcileToken, now Stamp) bool {\n"),
     ],
     "TestPortfolioTruthHasNoPublicBypass"),

    ("M-W-READTOKEN",
     "export the reconciliation token on PortfolioRead, so a caller can "
     "assemble a complete-looking reconciliation out of nothing",
     [
         ("harness/wsx/portfolio.go",
          "\ttoken ReconcileToken\n\tseq   uint64\n",
          "\tToken ReconcileToken\n\ttoken ReconcileToken\n\tseq   uint64\n"),
     ],
     "TestPortfolioTruthHasNoPublicBypass"),
    ("M-W-REJECTLIVE",
     "a book frame core never accepted leaves an ALREADY-ACTIONABLE market "
     "licensed to place -- quoting against a book we know is behind",
     [
         ("harness/wsx/gate.go",
          "\tm.quarantined = true\n\tm.snapGen = 0\n}\n",
          "}\n"),
     ],
     "TestRejectedBookFrameImmediatelyQuarantinesAnActionableMarket"),
    # -----------------------------------------------------------------------
    # lip-bw0 — lifecycle safety (pilot-plan §7.5)
    # -----------------------------------------------------------------------
    ("M3",
     "os.Exit(0) on SIGTERM instead of draining "
     "-- H-HALT-3's rule deleted, and invisible to any in-process assertion",
     [
         # Re-anchored by lip-vxo's amendment: `Handle`'s inline switch became
         # `signalTrigger`, so that `Confirm` and `Handle` cannot disagree about
         # which causes may authorise an exit. The mutation is unchanged in
         # meaning -- exit on SIGTERM instead of draining.
         ("harness/lifecycle/drain.go",
          "\tcase syscall.SIGTERM:\n\t\treturn triggerSigterm\n",
          "\tcase syscall.SIGTERM:\n\t\tos.Exit(0)\n\t\treturn triggerSigterm\n"),
     ],
     "TestSIGTERMWithInventoryOutlivesSignalAndDrainTimeout"),

    ("M11",
     "startup is marked complete after the positions walk, before orders, "
     "fills, balance, the adoption policy and the verified sweep",
     [
         ("harness/lifecycle/startup.go",
          "\t// --- Step 2: resting orders, UNFILTERED --------------------------------\n",
          # The REPLACEMENT was re-authored by lip-eyq §3 along with the anchor:
          # `attempt` returns a single `passResult` now, not the four-value
          # tuple this used to build. The anchor audit could not catch that --
          # it validates that `old` still matches, and this `old` did.
          "\treturn passResult{ad: &adoption{\n"
          # AGAIN at lip-da6, the same way: `NewSeededPortfolio` took a second
          # argument and this call did not follow, so the mutation stopped
          # compiling -- and a DID-NOT-BUILD does not count as caught. Twice is
          # a pattern. A `new` that names a production symbol is a second copy
          # of that symbol's signature, and the cheap audit never reads `new`.
          "\t\tportfolio: risk.NewSeededPortfolio(pos.ByTicker, s.baseline),\n"
          "\t\tstates:    map[string]quote.MarketState{},\n"
          "\t}, anoms: anoms}\n\n"
          "\t// --- Step 2: resting orders, UNFILTERED --------------------------------\n"),
     ],
     "TestStartupNeverLicensesBeforeFourReadsPolicyAndCleanSweeps"),

    ("M20",
     "the latch reports Ensure durable without writing anything "
     "-- the stop looks committed and does not survive the restart",
     [
         ("harness/lifecycle/latch.go",
          "\tfh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)\n",
          "\treturn true, nil\n\n"
          "\tfh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)\n"),
     ],
     "TestGlobalStopIsDurableBeforePublicationAndSurvivesRestart"),

    ("M-L-BOOTORDER",
     "the global controller defers the latch read, so the first portfolio "
     "request happens before H-HALT-4's read",
     [
         ("harness/lifecycle/global.go",
          "\t_, present, err := store.Load()\n",
          "\tvar present bool\n\tvar err error\n"),
     ],
     "TestLatchReadPrecedesEveryPortfolioRequest"),

    ("M-L-LATCHFAIL",
     "a present-but-unparseable latch reads as CLEAR "
     "-- HR-009 through a file truncated by the crash that wrote it",
     [
         ("harness/lifecycle/latch.go",
          "\trec, err := decodeLatch(b)\n\tif err != nil {\n\t\treturn rec, true, err\n\t}\n",
          "\trec, err := decodeLatch(b)\n\tif err != nil {\n\t\treturn rec, false, nil\n\t}\n"),
     ],
     "TestMalformedOrUnreadableLatchFailsClosed"),

    ("M-L-ADOPTORDER",
     "the startup walk reads resting orders before positions "
     "-- §7.5's fixed acquisition sequence is violated",
     [
         ("harness/lifecycle/startup.go",
          "\tpos := s.src.Positions(ctx)\n",
          "\torders = s.src.Orders(ctx, \"\", rest.StatusResting)\n"
          "\tpos := s.src.Positions(ctx)\n"),
         ("harness/lifecycle/startup.go",
          "\torders := s.src.Orders(ctx, \"\", rest.StatusResting)\n",
          ""),
         # The declaration `orders` needs, now that patch 1 removed its `:=`.
         # Re-anchored by lip-eyq §3: `attempt` returns a `passResult` instead
         # of a four-value tuple, so the old signature anchor is gone.
         ("harness/lifecycle/startup.go",
          "\tvar anoms []risk.Anomaly\n\tfail := func(err error) passResult {\n",
          "\tvar anoms []risk.Anomaly\n\tvar orders rest.OrdersResult\n"
          "\tfail := func(err error) passResult {\n"),
     ],
     "TestStartupReadsPositionsOrdersFillsBalanceInOrder"),

    ("M-L-RETRYTHRESH",
     "startup gives up after ONE failed reconciliation instead of three",
     [
         ("harness/lifecycle/startup.go",
          "const startupFailureThreshold = 3\n",
          "const startupFailureThreshold = 1\n"),
     ],
     "TestUnknownRiskBeginsOnThirdFailureAndRetriesForever"),

    ("M-L-RETRYEXIT",
     "startup stops retrying once it reaches UNKNOWN_RISK "
     "-- a process sitting next to inventory it decided not to look at again",
     [
         ("harness/lifecycle/startup.go",
          "\tif s.consecutiveFailures >= startupFailureThreshold {\n"
          "\t\tat.Anomalies = append(at.Anomalies, risk.Anomaly{\n",
          "\tif s.consecutiveFailures >= startupFailureThreshold {\n"
          "\t\tat.Retry = false\n"
          "\t\tat.Anomalies = append(at.Anomalies, risk.Anomaly{\n"),
     ],
     "TestUnknownRiskBeginsOnThirdFailureAndRetriesForever"),

    ("M-L-SEEDLIVE",
     "historical startup fills are applied with risk.Live "
     "-- every fill that produced the seeded position is counted twice",
     [
         ("harness/lifecycle/startup.go",
          "\tfx := portfolio.ApplyFills(ownedConverted, s.guard.own, risk.Seed,\n"
          "\t\tnow.UnixMilli())\n",
          "\tfx := portfolio.ApplyFills(ownedConverted, s.guard.own, risk.Live,\n"
          "\t\tnow.UnixMilli())\n"),
     ],
     "TestStartupSeedsExchangePositionWithoutReplayingHistoricalFills"),

    ("M-L-FOREIGNCANCEL",
     "the adoption policy is run over EVERY resting order, so a foreign "
     "order can be swept -- an action on somebody else's risk",
     [
         ("harness/lifecycle/startup.go",
          "\tfor _, o := range ownedResting {\n\t\td, err := s.policy.DecideAdopted(ctx, o, facts.clone())\n",
          "\tfor _, o := range orders.Orders {\n\t\td, err := s.policy.DecideAdopted(ctx, o, facts.clone())\n"),
     ],
     "TestStartupForeignOrderIsExcludedAndNeverCancelled"),

    ("M-L-FOREIGNLIVE",
     "a foreign order appearing AFTER startup is handled as a startup "
     "exclusion -- the harness keeps quoting beside a live third party",
     [
         ("harness/lifecycle/foreign.go",
          "\t\tif phase == PhaseStartup {\n",
          "\t\tif phase == PhaseStartup || phase == PhaseLive {\n"),
     ],
     "TestLiveForeignActivityRequestsDurableGlobalStop"),

    ("M-L-SWEEP",
     "the startup cancel sweep's Clean verdict is ignored "
     "-- H-FAIL-3's cancel-requested orders are treated as off",
     [
         ("harness/lifecycle/startup.go",
          "\t\tif !res.Clean {\n",
          "\t\tif false {\n"),
     ],
     "TestInvalidAdoptedOrdersRequireVerifiedCleanSweep"),

    ("M-L-LOCK",
     "the non-blocking exclusive flock's failure is ignored "
     "-- two harnesses on one account, H-DEP-5's unrecoverable conflict",
     [
         ("harness/lifecycle/lock.go",
          "\tif err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {\n",
          "\tif err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil && false {\n"),
     ],
     "TestInstanceLockExcludesASecondProcessAndAllowsAStalePID"),

    ("M-L-DRAINEXIT",
     "the drain authorises a process exit once drain_timeout_h expires, with "
     "inventory still open -- HR-009's timer doing what the document forbids",
     [
         ("harness/lifecycle/drain.go",
          "\tif d.permit.Valid() && obs.TruthKnown && !obs.AnyInventory && !obs.AnyLiveOrder {\n",
          "\tif d.permit.Valid() && obs.TruthKnown && (mono-d.begin >= d.p.DrainTimeout ||\n"
          "\t\t(!obs.AnyInventory && !obs.AnyLiveOrder)) {\n"),
     ],
     "TestSIGTERMWithInventoryOutlivesSignalAndDrainTimeout"),

    ("M-L-SLEEP",
     "the wall/monotonic divergence response is disabled "
     "-- a host sleep leaves a stale book quoting with no resnapshot",
     [
         ("harness/lifecycle/sleep.go",
          "\tif divergence <= sleepDivergence {\n\t\treturn eff\n\t}\n",
          "\tif divergence <= sleepDivergence || true {\n\t\treturn eff\n\t}\n"),
     ],
     "TestClockDivergenceForcesResnapshotAndReconcile"),

    ("M-L-KEEPALIVE",
     "the launchd plan renders KeepAlive false "
     "-- F18's supervision becomes a one-shot launcher",
     [
         ("harness/lifecycle/launchd.go",
          "\tif err := emitKey(\"KeepAlive\"); err != nil {\n\t\treturn nil, err\n\t}\n"
          "\tif err := emitTrue(); err != nil {\n\t\treturn nil, err\n\t}\n",
          "\tif err := emitKey(\"KeepAlive\"); err != nil {\n\t\treturn nil, err\n\t}\n"
          "\tif err := enc.Encode(plistBool{XMLName: xml.Name{Local: \"false\"}}); err != nil {\n"
          "\t\treturn nil, err\n\t}\n"),
     ],
     "TestLaunchdPlanUsesKeepAliveAndCaffeinateIS"),

    ("M-L-CAFFEINATE",
     "the launchd job execs the harness directly, bypassing "
     "`/usr/bin/caffeinate -is` -- the Mac idle-sleeps under a live position",
     [
         ("harness/lifecycle/launchd.go",
          "\targv := []string{caffeinatePath, caffeinateFlags, p.Executable}\n",
          "\targv := []string{p.Executable}\n"),
     ],
     "TestLaunchdPlanUsesKeepAliveAndCaffeinateIS"),
    # -----------------------------------------------------------------------
    # lip-bw0 v2 — the audit's ten material breaks, ratcheted
    # -----------------------------------------------------------------------
    # RETARGETED by lip-eyq §2. The old anchor was the fused Decide's inline
    # test; the boundary is now split, so this breaks Advance's FIRST refusal --
    # a Stop request with nothing committed to the latch. NextGlobal's RUNNING
    # rule is `if in.Stop { WindingDown }`, so removing the guard publishes a
    # halt with nothing on disk, which a panic and launchd KeepAlive erase.
    ("M-L-CAUSELESS",
     "Advance honours GlobalInput.Stop with nothing committed to the durable "
     "latch -- a halt that a panic and a launchd restart erase completely",
     [
         ("harness/lifecycle/global.go",
          "\t\tif in.Stop {\n",
          "\t\tif false {\n"),
     ],
     "TestCauselessStopCannotEnterWindingDown"),

    # RETARGETED by lip-eyq §3.3. `commit(in, causes)` no longer exists: causes
    # are made durable the MOMENT classification finds them, before the balance
    # error is consulted and before any conversion that can fail. This drops
    # that immediate commitment, so a foreign fill discovered during
    # reconciliation becomes a to-do item the caller is trusted to remember.
    ("M-L-STARTUPCAUSE",
     "startup returns a completed adoption without committing the causes "
     "classification found -- a foreign fill becomes a to-do item",
     [
         ("harness/lifecycle/startup.go",
          "\tfor _, c := range fe.Causes {\n",
          "\tfor _, c := range []StopCause(nil) {\n"),
     ],
     "TestStartupCommitsEveryCauseBeforeReturningDecision"),

    ("M-L-ADOPTIONFORGE",
     "the Adoption interface loses its unexported marker method, so any "
     "package can implement the licence to leave STARTING",
     [
         ("harness/lifecycle/adopt.go",
          "\t// isAdoption cannot be implemented outside this package. It is the whole\n"
          "\t// seal.\n\tisAdoption()\n}\n",
          "}\n"),
     ],
     "TestExternalCodeCannotForgeCompleteAdoption"),

    ("M-L-PLANFORGE",
     "the signal handler issues a planned-drain permit before the latch is "
     "committed -- the process may exit on a stop it failed to record",
     [
         ("harness/lifecycle/drain.go",
          "\tif eff.Decision.Committed {\n\t\teff.Permit = DrainPermit{valid: true, trigger: name, tsMillis: wallMillis}\n\t}\n",
          "\teff.Permit = DrainPermit{valid: true, trigger: name, tsMillis: wallMillis}\n"),
     ],
     "TestSignalCannotAuthoriseExitUntilLatchIsDurable"),

    ("M-L-FOREIGNFEE",
     "startup converts every fill before classifying ownership, so a FOREIGN "
     "fill with an unreadable fee becomes a generic retry and never latches",
     [
         ("harness/lifecycle/startup.go",
          "\townedConverted, err := ConvertFills(fe.OwnedFills)\n",
          "\townedConverted, err := ConvertFills(fills.Fills)\n"),
     ],
     "TestForeignFillWithUnusableFeeStillLatches"),

    ("M-L-SWEEPREPLAY",
     "the adoption is built from the position read that PRECEDED the cancel "
     "sweep -- an order that filled while being cancelled leaves q stale",
     [
         # Re-anchored by lip-eyq §3: the pass returns a `passResult` now, so
         # the old four-value return is gone. The property is unchanged --
         # deleting this is what lets a cancelling pass fall through and build
         # an adoption from the position read that preceded its own sweep.
         ("harness/lifecycle/startup.go",
          "\tif cancelled > 0 {\n"
          "\t\t// The caller rewalks. Nothing below would be built from a current read.\n"
          "\t\treturn passResult{anoms: anoms, cancelled: cancelled}\n\t}\n",
          ""),
     ],
     "TestStartupRewalksAfterEveryCancelSweep"),

    ("M-L-ADOPTRESTING",
     "adopted orders are never installed into the portfolio, so AnyLiveOrder "
     "reads false while an exchange-fillable order of ours rests",
     [
         ("harness/lifecycle/startup.go",
          "\toe := portfolio.ReplaceOrders(keptAsLive(kept), nil, true)\n",
          "\toe := portfolio.ReplaceOrders(nil, nil, true)\n"),
     ],
     "TestStartupPortfolioContainsEveryKeptOrder"),

    ("M-L-WALLCLOCK",
     "the sleep detector uses the monotonic delta as its wall delta -- the "
     "same clock compared against itself, so F7 can never fire",
     [
         ("harness/lifecycle/sleep.go",
          "\twallDelta := time.Duration(wallMillis-s.lastWallMillis) * time.Millisecond\n",
          "\twallDelta := mono - s.lastMono\n"),
     ],
     "TestSleepDetectorUsesIndependentWallTime"),

    ("M-L-EXISTDURABLE",
     "an EEXIST retry reports the latch durable without completing the parent "
     "directory sync the first attempt failed",
     [
         ("harness/lifecycle/latch.go",
          "\t\t\tif serr := f.syncDirFn(f.dir); serr != nil {\n"
          "\t\t\t\treturn false, fmt.Errorf(\"latch %s exists but its parent \"+\n"
          "\t\t\t\t\t\"directory %s could not be synced, so the entry naming it \"+\n"
          "\t\t\t\t\t\"may not survive a power cut: %w\", f.path, f.dir, serr)\n"
          "\t\t\t}\n\t\t\treturn true, nil\n",
          "\t\t\treturn true, nil\n"),
     ],
     "TestExistingLatchRetryResyncsParentBeforeDurable"),

    ("M-L-SUMMARYALIAS",
     "Summary() hands out the internal maps and slices, so a reporting "
     "consumer can rewrite the adoption's managed set",
     [
         ("harness/lifecycle/adopt.go",
          "func (a *adoption) Summary() StartupSummary { return a.summary.clone() }\n",
          "func (a *adoption) Summary() StartupSummary { return a.summary }\n"),
     ],
     "TestStartupSummaryIsADeepCopy"),

    ('M-HS-PERMIT',
     "issue a dispatch permit when the reservation is enqueued -- H-ORD-6's ownership record is no longer durable before dispatch",
     [
         ('harness/hstore/ledger.go',
          '\trec, err := newReservation(h, o, role, reservedMs)\n\tif err != nil {\n\t\treturn Receipt{}, err\n\t}\n\treturn s.submit(&submission{\n\t\tkind:    KindReserveOrder,\n\t\treserve: rec,\n\t\torder:   o,\n\t\trole:    role,\n\t})\n',
          '\trec, err := newReservation(h, o, role, reservedMs)\n\tif err != nil {\n\t\treturn Receipt{}, err\n\t}\n\trcpt, serr := s.submit(&submission{\n\t\tkind:    KindReserveOrder,\n\t\treserve: rec,\n\t\torder:   o,\n\t\trole:    role,\n\t})\n\tif serr == nil {\n\t\ts.mu.Lock()\n\t\ts.publishLocked(Result{Receipt: rcpt, Kind: KindReserveOrder,\n\t\t\tpermit: DispatchPermit{coid: rec.Coid, order: o, role: role,\n\t\t\t\tstore: s}})\n\t\ts.mu.Unlock()\n\t}\n\treturn rcpt, serr\n'),
     ],
     'TestDispatchPermitExistsOnlyAfterCommittedOwnership'),

    ('M-HS-BINDCACHE',
     'update the ownership index before the binding commits, so an intention that never lands reads as a durable fact',
     [
         ('harness/hstore/ledger.go',
          '\ts.own.markPending(orderID)\n\trcpt, err := s.submit(&submission{kind: KindBindOrder, bind: bind})\n',
          '\ts.own.markPending(orderID)\n\ts.own.commitBinding(orderID, coid)\n\trcpt, err := s.submit(&submission{kind: KindBindOrder, bind: bind})\n'),
     ],
     'TestFailedBindingNeverEntersCommittedOwnership'),

    ('M-HS-OWNRUN',
     'recognise only order ids from the current run -- every fill from before the last restart becomes a SEV1 foreign fill (H-ORD-9)',
     [
         ('harness/hstore/sqlite.go',
          '\t\t`SELECT coid, order_id, abandoned_ms FROM owned_order`)\n',
          '\t\t`SELECT coid, order_id, abandoned_ms FROM owned_order\n\t\t  WHERE run_id = (SELECT run_id FROM run\n\t\t                   ORDER BY started_ms DESC LIMIT 1)`)\n'),
     ],
     'TestOwnershipSurvivesRunsAndTerminalOrders'),

    ('M-HS-LOOKUPFAIL',
     'turn an ownership lookup error into all-foreign and keep applying -- a storage outage manufactures a foreign-fill stop on a consumed walk',
     [
         ('harness/risk/position.go',
          '\towned, err := own.OwnsOrders(orderIDs)\n\tif err != nil || len(owned) != len(unseen) {\n',
          '\towned, err := own.OwnsOrders(orderIDs)\n\tif err != nil || len(owned) != len(unseen) {\n\t\towned = make([]Ownership, len(unseen))\n\t\tfor i := range owned {\n\t\t\towned[i] = OwnershipForeign\n\t\t}\n\t}\n\tif false {\n'),
     ],
     'TestOwnershipLookupFailureAppliesNothingAndRefreshesNoTruth'),

    ('M-HS-FILLREPLACE',
     'let the later observer overwrite our_fill observation columns, so a '
     'backfill walk relabels a live observation as history (H-ORD-6)',
     [
         ('harness/hstore/sqlite.go',
          '\t\t\t// H-ORD-6: the first observer and its `backfilled` value win, so an\n'
          '\t\t\t// identical re-observation writes nothing at all.\n'
          '\t\t\treturn nil\n',
          '\t\t\t_, err = tx.Exec(\n'
          '\t\t\t\t`UPDATE our_fill SET first_run_id = ?, first_seen_ms = ?,\n'
          '\t\t\t\t        backfilled = ? WHERE trade_id = ?`,\n'
          '\t\t\t\tf.FirstRunID, f.FirstSeenMs, boolInt(f.Backfilled), f.TradeID)\n'
          '\t\t\treturn err\n'),
     ],
     'TestOurFillFirstObserverWinsAcrossRuns'),

    ('M-HS-AUDITDROP',
     'evict the oldest waiting audit record while the writer is stalled -- the rows describing the incident are deleted by the incident',
     [
         ('harness/hstore/writer.go',
          '\ts.queue = append(s.queue, sub)\n\ts.mu.Unlock()\n',
          '\ts.queue = append(s.queue, sub)\n\tif s.inflight && len(s.queue) > 1 {\n\t\ts.queue = append(s.queue[:1], s.queue[2:]...)\n\t}\n\ts.mu.Unlock()\n'),
     ],
     'TestAuditQueueNeverDropsWhileWriterIsStalled'),

    ('M-HS-ANOMRACE',
     'expose an anomaly to delivery after only ONE journal succeeds, so a record can be pushed once and then lost in a crash (§13.1)',
     [
         ('harness/hstore/sqlite.go',
          '\t\t  WHERE journaled_ms IS NOT NULL AND delivered_ms IS NULL\n',
          '\t\t  WHERE delivered_ms IS NULL\n'),
     ],
     'TestAnomalyIsInvisibleToDeliveryUntilBothJournalsAreDurable'),

    ('M-HS-STOREGATE',
     "permit an ADDING dispatch while store health is false -- H-STORE-3's revocation deleted, and the order's ownership cannot be recorded",
     [
         ('harness/hstore/records.go',
          '\tif p.role == quote.RoleAdding && !p.store.Health().AllowsAdding() {\n',
          '\tif p.role == quote.RoleAdding && false {\n'),
     ],
     'TestStoreFailureBlocksAddingButNotReducerOrMonitorWork'),

    ('M-HS-PRAGMA',
     'set SQLite synchronous mode to OFF -- committed records are lost on the power cut they exist to describe',
     [
         ('harness/hstore/schema.go',
          '\tpragmaSynchronous = "NORMAL"\n',
          '\tpragmaSynchronous = "OFF"\n'),
     ],
     'TestSchemaIsExactlyThePilotFiveAndPragmasArePinned'),

    ('M-HS-STATE',
     "persist a global transition carrying trigger `none` -- A9's reason is lost and a non-transition is recorded as one",
     [
         ('harness/hstore/state.go',
          '\tif trigger == quote.GTNone {\n\t\treturn StateEvent{}, fmt.Errorf("global transition %v -> %v carries "+\n\t\t\t"trigger `none`: A9 requires the reason, and NextGlobal returns "+\n\t\t\t"GTNone only when nothing happened", from, to)\n\t}\n',
          ''),
     ],
     'TestStateEventPersistsScopeStatesAndTrigger'),

    ('M-P-SEV1',
     'send SEV1 through the fifteen-minute bucket -- an unmanaged risk state is reported up to a quarter of an hour late (§13.2)',
     [
         ('harness/ping/service.go',
          '\t\twindow := int64(sev2BucketMs)\n\t\tif g.sev == risk.SEV1 {\n\t\t\twindow = sev1DedupMs\n\t\t}\n',
          '\t\twindow := int64(sev2BucketMs)\n'),
     ],
     'TestSEV1IsUrgentImmediateAndFiveMinuteDeduplicated'),

    ('M-P-SEV2',
     'bypass SEV2 suppression, so every occurrence pushes and the operator learns to ignore the channel (§13.2)',
     [
         ('harness/ping/service.go',
          '\t\tlast, seen := s.bucket(g)\n\t\tif seen && nowMs-last < window {\n',
          '\t\tlast, seen := s.bucket(g)\n\t\tif seen && nowMs-last < window && g.sev == risk.SEV1 {\n'),
     ],
     'TestSEV2IsLimitedPerClassMarketAndReportsSuppression'),

    ('M-P-RESTART',
     'load pending anomalies only from the current run -- every restart discards the backlog the restart is evidence for',
     [
         ('harness/hstore/sqlite.go',
          '\t\t  ORDER BY first_ms, anomaly_id`)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tdefer rows.Close()\n\tvar out []AnomalyRow\n',
          '\t\t    AND run_id = (SELECT run_id FROM run\n\t\t                   ORDER BY started_ms DESC LIMIT 1)\n\t\t  ORDER BY first_ms, anomaly_id`)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tdefer rows.Close()\n\tvar out []AnomalyRow\n'),
     ],
     'TestUndeliveredAlertsSurviveRestartAndDrainOldestFirst'),

    ('M-P-HEALTHBEAT',
     "suppress the heartbeat unless storage is healthy and the state is RUNNING -- the dead man's switch goes quiet exactly when its silence would be read as F18",
     [
         ('harness/ping/service.go',
          '\t"lip/harness/hstore"\n\t"lip/harness/risk"\n)\n',
          '\t"lip/harness/hstore"\n\t"lip/harness/quote"\n\t"lip/harness/risk"\n)\n'),
         ('harness/ping/service.go',
          '\tdue := s.lastHeartbeatMs == 0 || nowMs-s.lastHeartbeatMs >= s.interval.Milliseconds()\n',
          '\tdue := (s.lastHeartbeatMs == 0 ||\n\t\tnowMs-s.lastHeartbeatMs >= s.interval.Milliseconds()) &&\n\t\thb.StoreHealthy && hb.Global == quote.Running\n'),
     ],
     'TestHeartbeatContinuesInEveryStateAndNamesStoreFailure'),

    ('M-P-TOPIC',
     "include the ntfy topic in the message body -- the channel's bearer credential lands in every notification history and lock screen",
     [
         ('harness/ping/ntfy.go',
          '\tdest := s.base + "/" + url.PathEscape(topic)\n\tbody := m.Body\n',
          '\tdest := s.base + "/" + url.PathEscape(topic)\n\tbody := m.Body + "\\n(topic: " + topic + ")"\n'),
     ],
     'TestTopicNeverLeavesDestinationURL'),

    ('M-P-DEADMAN',
     'accept a nil dead-man endpoint -- the only F18 detector that survives this process dying becomes optional (§13.4)',
     [
         ('harness/ping/service.go',
          '\tif !dead.valid() {\n\t\treturn nil, errors.New("no dead-man endpoint: §13.4 requires a " +\n\t\t\t"concrete external check-in, and a harness that cannot be missed " +\n\t\t\t"by anything outside itself is unattended in the only sense that " +\n\t\t\t"matters")\n\t}\n',
          ''),
     ],
     'TestAlertServiceRejectsMissingDeadman'),

    ('M-P-REDIRECT',
     'allow the bearer transports to follow redirects, so one 302 discloses the ntfy topic or the dead-man endpoint to whatever answered',
     [
         ('harness/ping/secret.go',
          '\t\tCheckRedirect: refuseRedirect,\n',
          '\t\tCheckRedirect: nil,\n'),
     ],
     'TestBearerTransportsDoNotFollowRedirects'),


    ('M-HS-PERMHEALTH',
     'a permanently rejected record does not latch the store unhealthy, so adding continues over a hole in the audit trail',
     [
         ('harness/hstore/writer.go',
          '\t\ts.popLocked()\n\t\ts.fault = err.Error()\n\t\ts.healthy = false\n\t\ts.adding = false\n',
          '\t\ts.popLocked()\n'),
     ],
     'TestPermanentRecordFailureRevokesAddingAndCannotHealAcrossTheGap'),

    ('M-HS-ONERUN',
     'disable the single-writer guard, so two Run calls claim one head and each pop -- discarding the record behind it with no error anywhere',
     [
         ('harness/hstore/writer.go',
          '\tif !s.claimWriter() {\n\t\treturn\n\t}\n\tdefer s.releaseWriter()\n',
          '\ts.claimWriter()\n\tdefer s.releaseWriter()\n'),
     ],
     'TestOnlyOneWriterRunCanOwnTheFIFO'),

    # The guard has two halves and M-HS-ONERUN only mutates one of them. Deleting
    # the `s.running` term leaves `Run`'s `if !s.claimWriter()` textually intact
    # while a second writer walks straight through it, which is the same defect
    # reached from the other side. It is carried because the assertion that
    # catches it must observe the second `Run` RETURNING; the older assertion
    # here interrogated `claimWriter` directly, and an interrogation of a
    # predicate cannot see a caller that ignores it (`lip-ke1`).
    ('M-HS-CLAIMRUNNING',
     'drop the running term from the single-writer claim, so a second Run claims the FIFO while the first still owns it',
     [
         ('harness/hstore/writer.go',
          '\tif s.running || s.writerGone || s.closed {\n',
          '\tif s.writerGone || s.closed {\n'),
     ],
     'TestOnlyOneWriterRunCanOwnTheFIFO'),

    ('M-HS-RUNSTOP',
     'a writer that has exited leaves adding enabled, so an outstanding permit dispatches an order nothing can record',
     [
         ('harness/hstore/writer.go',
          '\ts.running = false\n\ts.writerGone = true\n\ts.healthy = false\n\ts.adding = false\n\ts.inflight = false\n',
          '\ts.running = false\n'),
     ],
     'TestStoppedWriterRevokesOutstandingAddingPermits'),

    ('M-HS-CLOSEDROP',
     'Close discards accepted audit records that are still queued',
     [
         ('harness/hstore/writer.go',
          '\tif n := len(s.queue); n > 0 {\n\t\ts.mu.Unlock()\n\t\treturn fmt.Errorf("refusing to close with %d accepted record(s) still "+\n\t\t\t"queued: each was accepted as durable-in-progress, and discarding "+\n\t\t\t"them here loses exactly the evidence a shutdown is most likely to "+\n\t\t\t"be about", n)\n\t}\n',
          ''),
     ],
     'TestCloseCannotDiscardAcceptedAuditRecords'),

    ('M-HS-FOREIGNDB',
     'accept an existing version-zero database that already holds another schema -- a mistyped path writes this schema into rig.db (H-ORD-7)',
     [
         ('harness/hstore/sqlite.go',
          '\t\tif len(tables) > 0 {\n',
          '\t\tif false {\n'),
     ],
     'TestOpenRefusesForeignVersionZeroDatabaseWithoutWritingIt'),

    ('M-HS-SCHEMAEXTRA',
     'accept a version-one database whose user tables are not exactly the pilot five',
     [
         ('harness/hstore/sqlite.go',
          '\t\tif !reflect.DeepEqual(tables, userTables) {\n',
          '\t\tif reflect.DeepEqual(tables, userTables) && false {\n'),
     ],
     'TestOpenRejectsAnySixthOrMissingPilotTable'),

    ('M-HS-PRAGMACONN',
     'apply foreign_keys once to one connection instead of through the DSN, so a replacement connection silently arrives with it OFF',
     [
         ('harness/hstore/sqlite.go',
          '\t\t"&_pragma=foreign_keys(" + pragmaForeignKeys + ")" +\n',
          ''),
         ('harness/hstore/sqlite.go',
          '\tif err := b.applyJournalMode(); err != nil {\n',
          '\tif _, ferr := db.Exec("PRAGMA foreign_keys=" + pragmaForeignKeys); ferr != nil {\n\t\tdb.Close()\n\t\treturn nil, ferr\n\t}\n\tif err := b.applyJournalMode(); err != nil {\n'),
     ],
     'TestWritePragmasSurviveConnectionReplacement'),

    ('M-HS-PATHALIAS',
     'accept the same path as both the database and the anomaly journal, so each destroys the other',
     [
         ('harness/hstore/writer.go',
          '\tif err := distinctArtifacts(c.DBPath, c.AnomalyLogPath); err != nil {\n\t\treturn nil, err\n\t}\n',
          ''),
     ],
     'TestStoreArtifactsMustBeDistinctFiles'),

    ('M-HS-JOURNALGAP',
     "ignore a database row that claims journalled text the JSONL does not hold, so the operator's fallback copy can be erased silently",
     [
         ('harness/hstore/writer.go',
          '\t\tif _, ok := byID[st.rec.AnomalyID]; !ok {\n',
          '\t\tif _, ok := byID[st.rec.AnomalyID]; ok && false {\n'),
     ],
     'TestMissingJournalLineIsAContradiction'),

    ('M-HS-JOURNALDIR',
     'skip the parent-directory sync when creating the anomaly journal, so the entry naming it may not survive a power cut',
     [
         ('harness/hstore/sqlite.go',
          '\t\tif err := syncDirFn(filepath.Dir(path)); err != nil {\n\t\t\treturn fail(fmt.Errorf("the directory entry naming the new anomaly "+\n\t\t\t\t"journal %s could not be synced, so the operator\'s fallback "+\n\t\t\t\t"copy may not survive a power cut: %w", path, err))\n\t\t}\n',
          ''),
     ],
     'TestNewJournalSyncsItsParentBeforeOpenSucceeds'),

    ('M-HS-STALLCLOCK',
     'measure the writer-progress bound in WALL time, so an NTP step declares a healthy writer stalled and a backward step hides a wedged one (F21)',
     [
         ('harness/hstore/writer.go',
          '\ts.inflightSince = s.monoNow()\n',
          '\ts.inflightSince = time.Duration(s.nowMs()) * time.Millisecond\n'),
         ('harness/hstore/writer.go',
          '\tcase s.inflight && s.monoNow()-s.inflightSince >= s.stallBound:\n',
          '\tcase s.inflight && time.Duration(s.nowMs())*time.Millisecond-s.inflightSince >= s.stallBound:\n'),
     ],
     'TestWriterStallUsesMonotonicTime'),

    ('M-P-ERRSECRET',
     "wrap the transport's *url.Error, which carries the whole destination URL -- so the ntfy topic reaches every log that records an error",
     [
         ('harness/ping/ntfy.go',
          '\t\treturn transportError("ntfy delivery", err)\n',
          '\t\treturn fmt.Errorf("ntfy delivery failed in transport: %w", err)\n'),
     ],
     'TestBearerSecretsNeverAppearInAnyError'),

    ('M-P-ZEROSENDER',
     'accept and dereference a forged zero-value ntfy sender, which panics inside the alert loop',
     [
         ('harness/ping/service.go',
          '\tif !sender.valid() {\n',
          '\tif sender == nil {\n'),
         ('harness/ping/ntfy.go',
          '\tif !s.valid() {\n\t\treturn errNoSender\n\t}\n',
          ''),
     ],
     'TestZeroValueSenderIsRejectedWithoutPanic'),

    ('M-P-ZERODEAD',
     "accept and dereference a forged zero-value dead man, so §13.4's only surviving F18 detector is silently absent",
     [
         ('harness/ping/service.go',
          '\tif !dead.valid() {\n',
          '\tif dead == nil {\n'),
         ('harness/ping/deadman.go',
          '\tif !d.valid() {\n\t\treturn errNoDeadman\n\t}\n',
          ''),
     ],
     'TestZeroValueDeadmanIsRejectedWithoutPanic'),

    ('M-P-RETRYWAKE',
     'publish only the next heartbeat deadline, so the private 1-60 second alert ladder and every rate-limit bucket resolve to an hour',
     [
         ('harness/ping/service.go',
          '\tnext := earliest(0, eff.NextHeartbeatMs, nowMs)\n\tnext = earliest(next, nextAlert, nowMs)\n\tif s.healthOwed {\n\t\tnext = earliest(next, s.healthPushDueMs(), nowMs)\n\t}\n\tif s.beatAttempts > 0 {\n\t\tnext = earliest(next, s.beatRetryAt, nowMs)\n\t}\n\tif s.deadAttempts > 0 {\n\t\tnext = earliest(next, s.deadRetryAt, nowMs)\n\t}\n',
          '\tnext := earliest(0, eff.NextHeartbeatMs, nowMs)\n\tif nextAlert < 0 {\n\t\tnext = earliest(next, nextAlert, nowMs)\n\t}\n'),
     ],
     'TestNextStepSchedulesFailedAndSuppressedAlertRetries'),

    ('M-P-INITIALHEALTH',
     'require a prior HEALTHY observation before the urgent health notice, so a harness that started broken never says so',
     [
         ('harness/ping/service.go',
          '\t} else if !s.healthSeen || s.lastHealthy {\n',
          '\t} else if s.healthSeen && s.lastHealthy {\n'),
     ],
     'TestInitiallyUnhealthyStoreAlertsUrgently'),

    ('M-P-STATUSRETRY',
     'discard a failed health, heartbeat or dead-man push instead of retrying it on the 1-60 second ladder, so the watchdog alarms on a live harness',
     [
         ('harness/ping/service.go',
          '\tdue := s.healthRetryAt\n',
          '\tdue := int64(0)\n'),
         ('harness/ping/service.go',
          '\tretryBeat := s.beatAttempts > 0 && nowMs >= s.beatRetryAt\n',
          '\tretryBeat := false\n'),
         ('harness/ping/service.go',
          '\tif eff.Heartbeat || (s.deadAttempts > 0 && nowMs >= s.deadRetryAt) {\n',
          '\tif eff.Heartbeat {\n'),
     ],
     'TestFailedStatusPushesRetryBeforeTheHour'),

    # ---- F2: the unresolved reservation, and the four places it is read ----
    #
    # H-ORD-6 commits the reservation BEFORE the order is dispatched and learns
    # the exchange order id afterwards. Everything below restores some part of
    # the pre-repair behaviour, in which the state between those two commits was
    # unrepresentable and a fill arriving in it was classified FOREIGN -- a
    # SEV1, a global stop and a durable operator-only WINDING_DOWN latch,
    # produced by our own order.

    # ---- lip-da6: the startup baseline -----------------------------------
    #
    # §7.5 step 3 reads fills with `since = now - backfill_h`; the live poller
    # reads them with a ZERO `since`. A fill older than the window is therefore
    # invisible to startup -- never classified, never seeded, never in
    # `seenTrade` -- and brand new to the first live poll. The baseline is the
    # pre-filter trade-id union that closes the gap, and it has FOUR independent
    # halves: q must not move, the row must be labelled backfilled, safety
    # classification must still run, and the thing has to be installed at all.
    # Each fails on its own, so each is anchored on its own.

    ('M-R-BASELINEREPLAY',
     'apply an inherited fill to q in Live mode, so a restart replays the '
     'account history that is older than backfill_h onto the position the '
     'exchange had already reported -- one poll then disagrees by the replayed '
     'quantity, and above pos_drift_hard that is a SEV1 POSITION_DRIFT and a '
     'durable WINDING_DOWN the harness manufactured by starting up',
     [
         ('harness/risk/position.go',
          '\t\tif mode == Seed || inherited {\n',
          '\t\tif mode == Seed {\n'),
     ],
     'TestOutOfWindowOwnedFillIsSeededNotReplayed'),

    ('M-R-BASELINELIVE',
     'report an inherited fill as live, so q stays right and the our_fill row '
     'is written backfilled=false -- H-ORD-6 is first-observer-wins and no '
     'other walk will ever write this row, so an analysis joining our_fill '
     'against rig.db reads every restart as a burst of trading',
     [
         ('harness/risk/position.go',
          '\t\tif mode == Live && inherited {\n'
          '\t\t\teff.OwnedBackfilled = append(eff.OwnedBackfilled, f)\n'
          '\t\t} else {\n'
          '\t\t\teff.Owned = append(eff.Owned, f)\n'
          '\t\t}\n',
          '\t\teff.Owned = append(eff.Owned, f)\n'),
     ],
     'TestOutOfWindowOwnedFillIsSeededNotReplayed'),

    ('M-R-BASELINESILENT',
     'skip inherited fills entirely instead of classifying them -- candidate A '
     'on lip-da6, which keeps q right and silently drops H-ORD-8: a taker fill '
     'in our own history is a fact about the harness whenever it happened, and '
     'this is the version of the fix that loses it',
     [
         ('harness/risk/position.go',
          '\t\tif _, seen := p.seenTrade[f.TradeID]; seen {\n\t\t\tcontinue\n\t\t}\n',
          '\t\tif _, seen := p.seenTrade[f.TradeID]; seen {\n\t\t\tcontinue\n\t\t}\n'
          '\t\tif _, base := p.baseline[f.TradeID]; base {\n\t\t\tcontinue\n\t\t}\n'),
     ],
     'TestOutOfWindowTakerFillStillRaisesHORD8'),

    ('M-L-NOBASELINE',
     'seed the adopted portfolio without the startup baseline, so the boundary '
     'between inherited history and live activity is computed correctly and '
     'then handed to nobody -- the first live poll replays the whole of the '
     'account history older than backfill_h',
     [
         ('harness/lifecycle/startup.go',
          '\tportfolio := risk.NewSeededPortfolio(pos.ByTicker, s.baseline)\n',
          '\tportfolio := risk.NewSeededPortfolio(pos.ByTicker, nil)\n'),
     ],
     'TestTheAdoptedPortfolioCarriesTheBaseline'),

    ('M-OWN-BACKFILLLABEL',
     'record an inherited fill as live in our_fill, mislabelling the one row '
     'that only the live walk ever writes for history outside backfill_h',
     [
         ('cmd/harness/run.go',
          '\tfor _, f := range eff.BackfilledFill {\n'
          '\t\tif _, err := o.r.store.RecordFill(o.r.run, f, o.r.ex.NowMs(), true); err != nil {\n',
          '\tfor _, f := range eff.BackfilledFill {\n'
          '\t\tif _, err := o.r.store.RecordFill(o.r.run, f, o.r.ex.NowMs(), false); err != nil {\n'),
     ],
     'TestStartupHistoryIsNeverALiveCanaryFill'),

    ('M-HS-OWNNULLSKIP',
     'load only BOUND owned_order rows at Open, so a reservation that was '
     'dispatched and never bound is invisible to the rebuilt index and the '
     'fill it produces reads as foreign -- the crash H-ORD-6 exists to survive '
     'becomes a permanent global stop on the next boot',
     [
         ('harness/hstore/sqlite.go',
          '\t\t`SELECT coid, order_id, abandoned_ms FROM owned_order`)\n',
          '\t\t`SELECT coid, order_id, abandoned_ms FROM owned_order\n'
          '\t\t   WHERE order_id IS NOT NULL`)\n'),
     ],
     'TestReservedUnboundCoidSurvivesReopenAsUnresolved'),

    ('M-HS-OWNCONCLUSIVE',
     'answer FOREIGN for an unrecognised order id even while reservations are '
     'outstanding, which is FINDING 2 in one line: the ledger declares a third '
     'party is trading the account on the strength of an order it authorised '
     'itself and has not finished recording',
     [
         ('harness/hstore/ledger.go',
          '\t\tcase len(o.unresolved) > 0:\n',
          '\t\tcase false && len(o.unresolved) > 0:\n'),
     ],
     'TestOwnsOrdersConclusiveForeignRequiresNoUnresolvedReservations'),

    ('M-R-INDETSEEN',
     'mark an unresolved fill as seen while deferring it, so the dedup set '
     'swallows it: the fills walk never offers it again, q never learns about '
     'contracts we hold, and no anomaly is ever raised',
     [
         ('harness/risk/position.go',
          '\t\t\teff.Deferred = append(eff.Deferred, f)\n',
          '\t\t\teff.Deferred = append(eff.Deferred, f)\n'
          '\t\t\tp.seenTrade[f.TradeID] = struct{}{}\n'),
     ],
     'TestUnresolvedFillIsDeferredNotSeenNotForeign'),

    ('M-W-NOBIND',
     'stop binding listed orders in the portfolio walk, so the one endpoint '
     'that reports both halves of a lost binding is read and discarded and the '
     'reservation stays outstanding for the life of the process -- after which '
     'every unrecognised fill on the account defers forever',
     [
         ('harness/wsx/portfolio.go',
          '\tbindListedOrders(bind, read, eff)\n',
          ''),
     ],
     'TestPortfolioPollBindsListedOrdersBeforeClassifyingFills'),

    ('M-L-INDETLATCH',
     'let an unresolved fill fall through to the foreign arm at startup, which '
     'commits a durable foreign_fill stop cause; only an operator can clear it, '
     'so a correct process that crashed between H-ORD-6 two commits takes '
     'itself off the market permanently',
     [
         ('harness/lifecycle/foreign.go',
          '\t\t\teff.Unresolved = append(eff.Unresolved, f)\n\t\t\tcontinue\n',
          ''),
     ],
     'TestStartupDoesNotLatchForeignFillWhileReservationsUnresolved'),

    # --- v3 item 1: the crash-tolerant journal --------------------------------

    ('M-HS-JRNLRESTAMP',
     'stamp the journal line on every attempt instead of once, so a retry after '
     'a post-write sync failure writes a line that DISAGREES with the one '
     'already on the platter -- reconcile tolerates an identical duplicate and '
     'refuses a disagreeing one forever, so a single transient EIO permanently '
     'stops the harness from starting',
     [
         ('harness/hstore/writer.go',
          '\t\tif sub.journalMs == 0 {\n\t\t\tsub.journalMs = s.nowMs()\n\t\t}\n',
          '\t\tsub.journalMs = s.nowMs()\n'),
     ],
     'TestJournalRetryAfterPostWriteSyncFailureDoesNotDuplicate'),

    ('M-HS-JRNLNOTRUNC',
     'report a failed append without returning the file to its last known-good '
     'length, so a short write leaves a partial line and the retry appends '
     'after it -- merging two records into one that parses as neither, which '
     'destroys the operator fallback copy §13.1 exists to be',
     [
         ('harness/hstore/sqlite.go',
          '\tif _, err := journalWriteFn(j.f, b); err != nil {\n\t\treturn j.rollback(err)\n\t}\n',
          '\tif _, err := journalWriteFn(j.f, b); err != nil {\n\t\treturn err\n\t}\n'),
     ],
     'TestJournalRetryAfterPartialWriteDoesNotCorrupt'),

    ('M-HS-JRNLTAILREFUSE',
     'refuse to open over a torn trailing line instead of repairing it, so the '
     'harness declines to start because of its own crash artifact -- a record '
     'that by construction never became delivery-visible and that reconcile '
     're-journals from the database',
     [
         ('harness/hstore/sqlite.go',
          '\tif err := j.f.Truncate(good); err != nil {\n'
          '\t\treturn fmt.Errorf("the anomaly journal %s ends in a partial record "+\n'
          '\t\t\t"and it could not be truncated to the last complete one: %w",\n'
          '\t\t\tj.path, err)\n'
          '\t}\n'
          '\tif err := journalSyncFn(j.f); err != nil {\n'
          '\t\treturn fmt.Errorf("the anomaly journal %s was truncated to its last "+\n'
          '\t\t\t"complete record and the truncation could not be synced: %w",\n'
          '\t\t\tj.path, err)\n'
          '\t}\n'
          '\tj.goodOff = good\n'
          '\treturn nil\n',
          '\treturn fmt.Errorf("the anomaly journal %s ends in a partial record "+\n'
          '\t\t"and this process will not start over it", j.path)\n'),
     ],
     'TestOpenRepairsTornTailFromCrashDuringAppend'),

    # --- v3 item 6: inspect on the writable connection; refuse zero-byte ------

    ('M-HS-EMPTYFRESH',
     'treat an existing zero-byte database file as fresh, so a truncating '
     'redirect or a restore that produced nothing is silently given a new '
     'schema -- an EMPTY ownership ledger, under which no order id is '
     'recognised and every fill on the account classifies as foreign (H-ORD-9)',
     [
         ('harness/hstore/sqlite.go',
          '\tif info.Size() == 0 {\n',
          '\tif info.Size() == 0 {\n\t\treturn true, nil\n\t}\n\tif info.Size() < 0 {\n'),
     ],
     'TestOpenRefusesZeroByteDatabaseFile'),

    ('M-HS-WALBEFOREINSPECT',
     'set journal_mode=WAL before inspecting, so a mistyped absolute path '
     'switches the evidence collectors journal mode underneath two running '
     'writers before the rejection that cannot un-modify it (H-ORD-7)',
     [
         ('harness/hstore/sqlite.go',
          '\tif !fresh {\n'
          '\t\tfresh, err = inspectDatabase(db, path)\n'
          '\t\tif err != nil {\n'
          '\t\t\tdb.Close()\n'
          '\t\t\treturn nil, err\n'
          '\t\t}\n'
          '\t}\n'
          '\tif err := b.applyJournalMode(); err != nil {\n'
          '\t\tdb.Close()\n'
          '\t\treturn nil, err\n'
          '\t}\n',
          '\tif err := b.applyJournalMode(); err != nil {\n'
          '\t\tdb.Close()\n'
          '\t\treturn nil, err\n'
          '\t}\n'
          '\tif !fresh {\n'
          '\t\tfresh, err = inspectDatabase(db, path)\n'
          '\t\tif err != nil {\n'
          '\t\t\tdb.Close()\n'
          '\t\t\treturn nil, err\n'
          '\t\t}\n'
          '\t}\n'),
     ],
     'TestOpenRefusesForeignVersionZeroDatabaseWithoutWritingIt'),

    # --- v3 item 7: a gone writer fails its queue loudly ----------------------

    ('M-HS-GONELIMBO',
     'leave accepted records queued when the writer returns, so each sits in a '
     'third state that is neither durable nor terminally failed: its submitter '
     'waits on a receipt that never resolves, and Close refuses on its account '
     'so the shutdown that stopped the writer cannot finish either',
     [
         ('harness/hstore/writer.go',
          '\tfor _, sub := range s.queue {\n'
          '\t\tif sub.kind == KindBindOrder {\n'
          '\t\t\ts.own.failBinding(sub.bind.OrderID, gone)\n'
          '\t\t}\n'
          '\t\ts.publishLocked(Result{Receipt: sub.receipt, Kind: sub.kind, Err: gone})\n'
          '\t}\n'
          '\ts.queue = nil\n',
          '\tfor _, sub := range s.queue {\n'
          '\t\tif false && sub.kind == KindBindOrder {\n'
          '\t\t\ts.own.failBinding(sub.bind.OrderID, gone)\n'
          '\t\t}\n'
          '\t}\n'),
     ],
     'TestCancelledWriterFailsQueuedRecordsRatherThanLimbo'),

    # --- v3 item 8: fills read-compare ---------------------------------------

    ('M-HS-FILLBLINDDUP',
     'accept a trade id that comes back with different exchange facts as an '
     'already-recorded duplicate, so the ledger goes on asserting one price '
     'while the account holds another and nothing anywhere records that both '
     'were seen',
     [
         ('harness/hstore/sqlite.go',
          '\t\t\tif want := factsOf(f); got != want {\n'
          '\t\t\t\treturn permanent("trade %s is already recorded as %+v and has "+\n'
          '\t\t\t\t\t"now been reported as %+v; two accounts of one trade "+\n'
          '\t\t\t\t\t"cannot both be true, and accepting this one silently "+\n'
          '\t\t\t\t\t"would leave the ledger asserting a figure the account "+\n'
          '\t\t\t\t\t"does not hold", f.TradeID, got, want)\n'
          '\t\t\t}\n',
          ''),
     ],
     'TestDivergentDuplicateTradeIsRefusedNotSwallowed'),

    # --- v3 item 9: the reader pragma in the DSN ------------------------------

    ('M-HS-READERPRAGMA',
     'set query_only once after sql.Open instead of through the DSN, so a '
     'replacement connection from the pool arrives with it OFF and the '
     'read-only view becomes a second writer against a database whose whole '
     'design is that there is exactly one',
     [
         ('harness/hstore/sqlite.go',
          '\tdb, err := sql.Open("sqlite", readerDSN(path))\n',
          '\tdb, err := sql.Open("sqlite", path)\n'),
     ],
     'TestReaderDSNCarriesQueryOnlyPragma'),

    # --- v3 item 10: the dead-man constructor --------------------------------

    ('M-P-CTORURLLEAK',
     "wrap url.Parse's *url.Error, whose URL field is the whole check-in "
     'endpoint, so a misconfigured deployment prints its bearer credential in '
     'the first log line it produces',
     [
         ('harness/ping/deadman.go',
          '\t\treturn nil, parseFailure(err)\n',
          '\t\treturn nil, fmt.Errorf("the dead-man endpoint does not parse as a "+\n'
          '\t\t\t"URL: %w", err)\n'),
     ],
     'TestDeadmanConstructorErrorNeverContainsEndpoint'),

    # --- v3 items 11-13: the ping scheduling repairs --------------------------

    ('M-P-STALEPOLL',
     "evaluate the one-second health poll against the snapshot taken on the way "
     'INTO the Step, so a Step that queued a delivery record against a wedged '
     'writer publishes the hourly heartbeat as the next deadline and the wedge '
     'goes unobserved for an hour',
     [
         ('harness/ping/service.go',
          '\thealth = s.store.Health()\n\tif health.Pending() > 0 || !health.Healthy() {\n',
          '\tif health.Pending() > 0 || !health.Healthy() {\n'),
     ],
     'TestNextStepHonoursHealthPollAfterMidStepSubmission'),

    ('M-P-HEALTHFLAP',
     'send an urgent health notice on every healthy->unhealthy transition, so '
     'one disk that fails and recovers on the retry ladder empties the '
     "operator's battery at the priority that overrides a silenced phone",
     [
         ('harness/ping/service.go',
          '\tif s.healthOwed && nowMs >= s.healthPushDueMs() {\n',
          '\tif s.healthOwed && nowMs >= s.healthRetryAt {\n'),
     ],
     'TestFlappingStoreHealthIsRateLimited'),

    ('M-P-REFUSESILENT',
     'honour a refused delivery record by remembering an hour-long retry, which '
     'dueAt prefers over everything the table says -- so a store that cannot '
     'record deliveries silences for an hour the very alerts it failed to '
     'record',
     # Three edits, because expressing the defect faithfully requires putting it
     # where `dueAt` can see it. `record` writing `s.retryAt` directly is INERT:
     # `pushGroup` deletes those entries on the next line after a delivered push
     # and overwrites them on a failed one. That the obvious mutation is inert is
     # the same observation F5 made about the arm that used to live there.
     [
         ('harness/ping/service.go',
          'func (s *Service) record(ids []string, nowMs int64, delivered bool) {\n',
          'func (s *Service) record(ids []string, nowMs int64, delivered bool) bool {\n'),
         ('harness/ping/service.go',
          '\ts.store.RecordDeliveryAttempt(att)\n}\n',
          '\t_, rerr := s.store.RecordDeliveryAttempt(att)\n\treturn rerr == nil\n}\n'),
         ('harness/ping/service.go',
          '\ts.record(ids, nowMs, err == nil)\n'
          '\n'
          '\tif err == nil {\n'
          '\t\ts.markBucket(g, nowMs)\n'
          '\t\tfor _, id := range ids {\n'
          '\t\t\tdelete(s.retryAt, id)\n'
          '\t\t}\n'
          '\t} else {\n',
          '\tif !s.record(ids, nowMs, err == nil) {\n'
          '\t\tfor _, id := range ids {\n'
          '\t\t\ts.retryAt[id] = nowMs + 3_600_000\n'
          '\t\t}\n'
          '\t} else if err == nil {\n'
          '\t\ts.markBucket(g, nowMs)\n'
          '\t\tfor _, id := range ids {\n'
          '\t\t\tdelete(s.retryAt, id)\n'
          '\t\t}\n'
          '\t} else {\n'),
     ],
     'TestStoreRefusedDeliveryRecordStillRedeliversAtBucketWindow'),

    # --- v3 item 14: the ticker contradiction --------------------------------

    ('M-R-TICKERBLIND',
     'store an order ticker and never compare it, so an order first seen on '
     'market A and then reported filling on market B books the contracts '
     'against A while the exchange holds them on B -- two tickers q wrong at '
     'once, and H-POS-1 reports drift on both with nothing to say which '
     'reading was the mistake',
     [
         ('harness/risk/position.go',
          '\tif st.ticker != ticker {\n',
          '\tif false && st.ticker != ticker {\n'),
     ],
     'TestOrderTickerConflictIsRefusedRatherThanGuessed'),

    # -----------------------------------------------------------------------
    # lip-eyq — transactional startup authority
    #
    # `M-L-CAUSELESS` and `M-L-STARTUPCAUSE` are RETARGETED above rather than
    # added here: the boundary they tested was split into CommitStop/Advance,
    # so their old anchors describe code that no longer exists.
    # -----------------------------------------------------------------------

    # §3.4. The taker flag needs no arithmetic; the fee corroborator does. A
    # commitment that waits for the conversion is a commitment a malformed
    # fee_cost deletes -- on a fill the exchange EXPLICITLY flagged.
    ('M-L-CAUSEFAIL',
     'postpone the known owned-taker commitment until after the fee '
     'conversion, so a taker fill whose fee_cost will not parse latches '
     'nothing at all and stops being reported once backfill_h rolls past it',
     [
         ('harness/lifecycle/startup.go',
          '\t\tif !f.IsTaker {\n',
          '\t\tif true {\n'),
     ],
     'TestStartupCommitsKnownTakerEvidenceBeforeFeeConversion'),

    # §2. DRAINED is the one state reached by two FALSES, so it is the one edge
    # an unpopulated input takes by accident. Dropping RiskKnown makes a
    # GlobalInput nobody filled in read exactly like a flat, quiet account --
    # and DRAINED rests no reducer, so the position it never looked at is now
    # unmanaged.
    ('M-L-FALSEFLAT',
     'ignore RiskKnown when draining, so absence of evidence drains: a '
     'WINDING_DOWN whose risk flags were never established reports the '
     'inventory gone on the strength of a read that did not happen',
     [
         ('harness/quote/machine.go',
          '\t\tif in.RiskKnown && !in.AnyInventory && !in.AnyLiveOrder {\n',
          '\t\tif !in.AnyInventory && !in.AnyLiveOrder {\n'),
     ],
     'TestWindingDownRequiresKnownFlatTruthBeforeDrained'),

    # §3.8 / §2. The coordinator's state is the whole point of the rewrite: a
    # Step that resets to STARTING has no memory, so a process that reconciled
    # and reached RUNNING re-enters adoption on the next pass, and a latched one
    # is handed STARTING by the very procedure H-HALT-4 exists to gate.
    ('M-L-STATERESET',
     'reset the coordinator state to STARTING at the top of every Step, so '
     'the startup transaction has no memory across attempts',
     [
         ('harness/lifecycle/startup.go',
          '\tres := s.walk(ctx, now)\n',
          '\ts.state = quote.Starting\n\tres := s.walk(ctx, now)\n'),
     ],
     'TestStartupOwnsStateAcrossRetrySequence'),

    # §2. The forge. An exported setter puts the global state back under caller
    # control, which is exactly the boundary lip-eyq removed -- and the state a
    # caller is most likely to set is the zero value.
    ('M-L-STATEFORGE',
     'add an exported setter for the coordinator global state, restoring the '
     'caller-injected state the split boundary exists to prevent',
     [
         ('harness/lifecycle/startup.go',
          'func (s *Startup) State() quote.GlobalState { return s.state }\n',
          'func (s *Startup) State() quote.GlobalState { return s.state }\n'
          '\n// SetState re-opens the injected-state hole.\n'
          'func (s *Startup) SetState(st quote.GlobalState) { s.state = st }\n'),
     ],
     'TestStartupPublicSurfaceCannotAcceptOrResetGlobalInput'),

    # §4. Whether we may ADD at all is not a question about the order, so it is
    # not the injected policy's to answer. The policy is lip-3af's and cannot be
    # assumed to know the latch is set or the ticker is excluded; if its `keep`
    # is final, a halted process adopts its own adding orders and resumes
    # quoting under a durable stop. `false &&` keeps both operands used so the
    # mutated tree still compiles.
    ('M-L-STOPKEEP',
     'preserve an adopted ADDING order the lifecycle revoked, so a globally '
     'halted or foreign-excluded market keeps resting our adding quote',
     [
         ('harness/lifecycle/startup.go',
          '\t\t\tif (!addingPermitted || tickerExcluded) &&\n',
          '\t\t\tif false && (!addingPermitted || tickerExcluded) &&\n'),
     ],
     'TestRevokedAddingOrderIsCancelledWhileTheReducerInTheSameExcludedMarketSurvives'),

    # §4. `Selected` handed to the policy must be the EFFECTIVE set. Passing the
    # raw one asks the policy to validate orders against markets startup has
    # already decided are not ours to newly quote -- and F15's exclusion then
    # has no consequence at the only layer that enforces it.
    ('M-L-EXCLUDESELECT',
     'hand the adoption policy the RAW operator selection instead of the '
     'effective set, so a foreign-excluded ticker is still quotable',
     [
         ('harness/lifecycle/startup.go',
          '\t\tSelected:        effective,\n',
          '\t\tSelected:        s.selected,\n'),
     ],
     'TestAdoptionPolicyReceivesEffectiveSelectionAndExclusionsAsDefensiveCopies'),

    # -----------------------------------------------------------------------
    # lip-3af -- the seams. Every component below was already individually
    # guarded here; what was NOT guarded, because it did not exist, is the
    # composition. `quote.Decide` had no caller outside its own tests,
    # `wsx.Poller` was complete and nothing consumed its channel, and all of
    # `harness/lifecycle` was reachable only from `lifecycle_test`. These
    # mutations break the JOINS.
    # -----------------------------------------------------------------------

    # H-ORD-6's whole content is an ordering: the coid reservation is durable
    # BEFORE the order is dispatched. This deletes the barrier while leaving
    # every other line intact -- the reservation is still submitted, so the row
    # still appears, and only the WAIT is gone. That is the realistic form of
    # the defect: not someone deciding to skip the ledger, but someone deciding
    # the await was slow.
    # It must actually SEND. A mutation that merely broke the permit would fail
    # closed -- `DispatchPermit.Order()` refuses a zero permit, so nothing would
    # reach the exchange and a test asserting "no POST" would still pass, which
    # is a mutation recorded as caught for a reason that has nothing to do with
    # the rule. So this one ignores the await's failure AND takes the order from
    # the request instead of from the permit, which is the exact property
    # `records.go` states: "the eventual lip-3af dispatcher consumes permits and
    # never a raw rest.CreateOrder".
    ('M-3AF-NOPERMIT',
     'dispatch the request\'s own order and ignore the reservation outcome, so '
     'an order whose ownership never committed reaches the exchange and the '
     'fill it produces is one H-ORD-9 must classify as foreign',
     [
         ('cmd/harness/dispatch.go',
          '\tpermit, err := awaitPermit(ctx, rcpt, reserves)\n\tif err != nil {\n',
          '\tpermit, err := awaitPermit(ctx, rcpt, reserves)\n\t_ = permit\n\tif false {\n'),
         ('cmd/harness/dispatch.go',
          '\tbody, err := permit.Order()\n\tif err != nil {\n',
          '\tbody, err := req.Order, error(nil)\n\tif err != nil {\n'),
     ],
     'TestNoOrderIsSentWithoutACommittedPermit'),

    # The binding's value is in WHEN it runs. Deferring it to the next poll is
    # not a latency regression: between the ack and the committed binding every
    # fill on our OWN order classifies UNRESOLVED, `risk.ApplyFills` defers it,
    # `q_local` lags the account, and past 120s it escalates to SEV2
    # FILL_UNCLASSIFIABLE per trade. `wsx.bindListedOrders` is the safety net
    # and is one poll late by construction -- it cannot catch an order that
    # filled and went terminal inside one poll interval.
    ('M-3AF-LATEBIND',
     'let the portfolio walk pick the binding up instead of submitting it on '
     'the CreateResult, so our own fills defer for a poll and then escalate',
     [
         ('cmd/harness/dispatch.go',
          '\tif create.OrderID != "" {\n\t\tif _, berr := r.store.BindOrder(create.Coid, create.OrderID,\n',
          '\tif false && create.OrderID != "" {\n\t\tif _, berr := r.store.BindOrder(create.Coid, create.OrderID,\n'),
     ],
     'TestBindingIsSubmittedOnTheAckAndNotAtTheNextPoll'),

    # `Queue.ConfirmAbsent`'s key is (market, side), but `SweepResult.Clean`
    # only covers the order ids the sweep was ASKED about. `OtherOurs` is
    # exactly the gap: another order of ours resting on that side, reported and
    # deliberately not cancelled. Claiming absence from `Clean` alone unlocks a
    # cancel-confirm-place's replacement while one of our orders is still live
    # on that side, which is the aggregate overlap H-Q-5a forbids reached
    # through the confirmation rather than through the sizing.
    ('M-3AF-ABSENT',
     'claim (market, side) absence from the requested orders alone, ignoring '
     'another of ours the verifying read found still resting there',
     [
         ('cmd/harness/dispatch.go',
          'res.Absent = sweep.Clean && !restsOn(sweep.OtherOurs, req.Side)',
          'res.Absent = sweep.Clean'),
     ],
     'TestAbsenceIsClaimedOnlyFromACompleteRead'),

    # The pilot is cancel-confirm-place EVERYWHERE (pilot-plan §2.7, §7.1), and
    # this flag is the single line between that and two of our orders live at
    # one price band at once. Its zero value is already false, so the mutation
    # has to set it explicitly -- which is exactly the one-word edit a later
    # revenue optimisation would make.
    ('M-3AF-PTC',
     'turn H-Q-9 place-then-cancel ON, so a requote rests the replacement while '
     'the order it replaces is still live',
     [
         ('cmd/harness/run.go',
          '\t\tAllowPlaceThenCancel: false,\n',
          '\t\tAllowPlaceThenCancel: true,\n'),
     ],
     'TestRequotingAnExistingOrderIsAlwaysCancelConfirmPlace'),

    # H-DEP-5. Two harnesses on one account do not produce a merge conflict,
    # they produce two `q_local` models that are each individually consistent,
    # each wrong, and each unable to tell that the fills moving the position came
    # from the other one. The lock is taken before the first REST request
    # precisely so the second process is refused before it can send one.
    ('M-3AF-NOLOCK',
     'start without the single-instance lock, so a second harness runs against '
     'the same account and neither can tell whose fills moved the position',
     [
         ('cmd/harness/runtime.go',
          '\tr.lock, err = lifecycle.AcquireInstanceLock(c.Paths.Lock)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n',
          '\tr.lock, err = nil, error(nil)\n'),
     ],
     'TestTheInstanceLockIsHeldForTheWholeRunAndRefusesASecondRig'),

    # H-HALT-4's latch survives the process ON PURPOSE -- "the harness never
    # self-clears it" -- and §10.4 makes clearing it an OPERATOR action. A
    # restart that quietly resumed quoting under a durable stop is the halt
    # erased by a supervisor, which is the failure `launchd KeepAlive` makes
    # automatic.
    ('M-3AF-RESUME',
     'start a latched harness without -resume, so a durable global stop is '
     'cleared by restarting the process',
     [
         ('cmd/harness/runtime.go',
          '\tif r.boot.Latched && !resume {\n',
          '\tif false && r.boot.Latched && !resume {\n'),
     ],
     'TestASetLatchRefusesWithoutResumeAndWindsDownWithIt'),

    # H-TOP-5 / I3. I2 makes the monitor unstoppable but does NOT make it
    # truthful: an owner that stops publishing while continuing to run leaves the
    # monitor re-reading one snapshot forever, stamping fresh rows and pushing
    # hourly heartbeats about a world that has stopped moving. A5 passes at every
    # tick. That is probebot.py's observable -- confident silence about live risk
    # -- reached by a different route than probebot.py's `break`.
    ('M-3AF-NOSNAP',
     'publish a snapshot only when something changed, so a quiet owner looks '
     'identical to a wedged one and the stall detector has nothing to detect',
     [
         ('cmd/harness/run.go',
          '\to.snapSeq++\n',
          '\tif o.snapSeq > 0 && o.market == quote.Idle {\n\t\treturn\n\t}\n\to.snapSeq++\n'),
     ],
     'TestTheSnapshotSequenceAdvancesOnEveryTickIncludingHalted'),

    # `Queue.commit` advances a DEPENDENT intent's first leg to StageFirstSent at
    # DEQUEUE -- "that one line is H-Q-9a: dispatch is not confirmation" -- and
    # `Dispatchable()` is false there while `stillWanted` never drops it. So an
    # intent whose write did not reach a terminal answer occupies its (market,
    # side) forever and `enqueue`'s duplicate check refuses every replacement.
    # The market stops quoting for the life of the process and nothing says so.
    ('M-3AF-WEDGE',
     'keep the intents of a write that never completed, so the side is wedged '
     'at stage-first-sent and can never be quoted again',
     [
         ('cmd/harness/run.go',
          '\tif res.Err != nil {\n\t\tfor _, id := range res.Req.IDs {\n\t\t\to.r.queue.Drop(id)\n\t\t}\n',
          '\tif res.Err != nil {\n'),
     ],
     'TestAnIncompleteWriteReleasesItsIntentsRatherThanWedgingTheSide'),

    # The window this composition creates and nothing else covers.
    # `Portfolio.ReplaceOrders` is wholesale from a complete walk (H-POS-4), so
    # an order just placed is in no aggregate until the next poll -- and §6.5
    # restores presence on an empty side EVERY tick, deliberately without the
    # debounce ("a presence gap is revenue"). Measured before the fix: one +8
    # position produced ten identical 8-contract exits in about three seconds.
    ('M-3AF-ACKGAP',
     'drop an acknowledged order from the aggregate until the next orders walk, '
     'so its side reads empty and §6.5 re-places it on every tick',
     [
         ('cmd/harness/run.go',
          '\tif create.MaxLive > 0 {\n',
          '\tif !create.Outcome.Definite() && create.MaxLive > 0 {\n'),
     ],
     'TestAnAckedOrderOccupiesTheAggregateBeforeAnyOrdersWalk'),

    # H-CLOSE-0's sampling rule applied to the READING rather than only to the
    # configuration. A close_time read once and never refreshed is a fact about
    # the market as of then, and §9 is explicit that "neither is assumed static".
    # HR-017 is that difference costing a close: a close_time that moved from
    # 17:00 to 12:03 was next observed after the close had passed, so neither the
    # close lead nor the final cancel ever ran. A stale schedule is more
    # dangerous than an absent one because it looks exactly like a good one.
    ('M-3AF-STALESCHED',
     'let a schedule read stay authoritative forever, so a close_time that moved '
     'is enforced from a reading nobody refreshed',
     [
         ('cmd/harness/run.go',
          '\tif !o.hasClose || o.scheduleStale() {\n',
          '\tif !o.hasClose {\n'),
     ],
     'TestAnUnknownCloseStopsAddingAndLeavesTheExitAlive'),

    # The operator's H-CLOSE-4 rule. A `can_close_early` market settles on
    # external information at a moment no schedule predicts, and 192 of the 200
    # active LIP programmes carry the flag -- so "prefer against selecting them"
    # does not scale to this universe, and the backoff is what bounds how much
    # inventory is carried into an unpredictable settlement.
    ('M-3AF-EARLYCLOSE',
     'never fire the early-close backoff, so an unpredictably-settling market is '
     'quoted right up to §16 close_lead like any other',
     [
         ('cmd/harness/run.go',
          '\tif !hasClose || !o.canCloseEarly || o.r.cfg.EarlyCloseLead <= 0 {\n',
          '\tif true || !hasClose || !o.canCloseEarly || o.r.cfg.EarlyCloseLead <= 0 {\n'),
     ],
     'TestTheEarlyCloseBackoffStopsAddingOnlyInsideItsOwnWindow'),

    # lip-0qj. Both of these are RELAYS -- `applyEvent` is the only thing that
    # carries the supervisor's observation to the component that acts on it --
    # and both fail SILENTLY, which is why they are worth a mutation each. The
    # relay was wired by lip-3af and, until now, asserted by nothing: the
    # existing disconnect mutations all target `wsx` itself (the gate that
    # computes the effect, the supervisor that detects the threshold) and every
    # one of them stays caught with `cmd/harness` throwing the result away.
    ('M-3AF-NOREDUCERELAY',
     'drop the F4 relay, so the supervisor detects a sustained outage and '
     'nothing ever tells the gate: the socket is down for an hour and no '
     'market reduces',
     [
         ('cmd/harness/run.go',
          '\t\teff := o.r.gate.NoteDisconnectSustained(ev.Down, ev.At)\n',
          '\t\teff := wsx.TickEffects{}\n'),
     ],
     'TestASustainedOutageReachesTheGateAndTheDisconnectTokenReachesThePoller'),

    # A weaker version of the same defect, and the more plausible one to write
    # by hand: the gate is told, so the STICKY flag is set and a later tick
    # stops the market -- but this tick, the one that learned about the outage,
    # quotes on. The gate's own `Reducing()` would hide it from any assertion
    # that only reads the gate.
    ('M-3AF-REDUCENOTTHISTICK',
     'tell the gate about the sustained outage but not this evaluation, so the '
     'market keeps quoting for the tick that learned about it',
     [
         ('cmd/harness/run.go',
          '\t\teff := o.r.gate.NoteDisconnectSustained(ev.Down, ev.At)\n'
          '\t\to.r.anom.raiseAll(eff.Anomalies)\n'
          '\t\to.noteReduce(eff.Reduce)\n',
          '\t\teff := o.r.gate.NoteDisconnectSustained(ev.Down, ev.At)\n'
          '\t\to.r.anom.raiseAll(eff.Anomalies)\n'),
     ],
     'TestASustainedOutageReachesTheGateAndTheDisconnectTokenReachesThePoller'),

    # The token half. `ApplyDisconnect` issues a token for the DISCONNECTED
    # generation precisely so portfolio polling survives the outage; dropping
    # it leaves REST issuing requests whose every answer is discarded as stale,
    # which is the most expensive way to be blind.
    ('M-3AF-NODISCTOKEN',
     'never offer the disconnect generation token, so every portfolio read '
     'taken during an outage is discarded as stale while REST keeps paying '
     'for it',
     [
         ('cmd/harness/run.go',
          '\t\to.offerToken(tokens, eff.Token)\n\n\tcase wsx.EventDisconnectReduce:\n',
          '\n\tcase wsx.EventDisconnectReduce:\n'),
     ],
     'TestASustainedOutageReachesTheGateAndTheDisconnectTokenReachesThePoller'),

    # -----------------------------------------------------------------------
    # lip-vxo -- the global stop's DELIVERY path.
    #
    # `lifecycle` computes two answers on every uncertain stop path,
    # `BlockAdding` and `RetryLatch`, and both are documented as instructions
    # rather than diagnostics. `cmd/harness` used to read NEITHER, at three
    # separate seams. Every mutation below targets a CONSUMER: the producer was
    # never wrong, which is why the existing catalogue stayed fully caught while
    # the answers were being thrown away.
    #
    # There is deliberately no "publish WINDING_DOWN before it is durable"
    # mutation here. That one is refused a layer down: `Advance` returns
    # `Committed: false` for `Stop` with nothing latched, so a consumer that
    # advanced anyway would publish nothing. lip-eyq made that structural and
    # `lifecycle`'s own tests hold it; a mutation here would be inert by
    # construction rather than by argument.

    # The prohibition itself. BlockAdding is honoured in exactly one place --
    # §5.2's own stop term -- so deleting the term is the whole defect: the
    # harness has decided to stop, cannot record it, and adds anyway.
    ('M-VXO-BLOCK',
     'ignore BlockAdding, so a market whose §12 cause could not be made '
     'durable keeps adding into the condition that decided to stop it',
     [
         ('cmd/harness/run.go',
          '\t\tStop: o.stopHeld || o.reduceNoted || o.r.gate.Reducing(ticker) ||\n',
          '\t\tStop: o.reduceNoted || o.r.gate.Reducing(ticker) ||\n'),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # The retry. Driving it from the owner tick is the point: all three callers
    # of `requestStop` are event-driven -- a portfolio read, a gate tick and an
    # ack that carried a fill -- so without this line the only thing that
    # retries a lost stop is the original condition happening to recur, and a
    # taker fill does not recur.
    ('M-VXO-RETRY',
     'never re-drive the held cause from the tick, so a global stop is retried '
     'only if the trigger that produced it happens to fire again',
     [
         # Re-anchored by lip-xdq, which inserted the ordinary global advance
         # between this call and the comment the anchor used to reach.
         ('cmd/harness/run.go',
          '\to.retryStop()\n',
          ''),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # The hold. This is the original defect restored exactly: the cause is
    # OBSERVED -- it is even stored -- and never marked held, so nothing blocks
    # and nothing retries. A signal computed correctly and discarded at the
    # seam, which is lip-0qj's lesson applied to the stop path.
    ('M-VXO-HOLD',
     'record the failed cause without holding it, so BlockAdding and '
     'RetryLatch are stored as diagnostics and acted on by nothing',
     [
         ('cmd/harness/run.go',
          '\tif o.stopHeld {\n\t\treturn\n\t}\n\to.stopCause, o.stopHeld = cause, true\n}\n',
          '\to.stopCause = cause\n}\n'),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # First-writer-wins, at the consumer. `FileLatch.Ensure` refuses to
    # overwrite a latch because "the first durable cause is the one the operator
    # investigates, and a later, more mundane trigger -- a SIGTERM sent while
    # winding down from a taker fill -- must not overwrite the reason the
    # harness stopped". A hold that takes the LAST cause defeats that from
    # above: nothing is on disk yet, so the retry simply writes the wrong one
    # and the operator of §10.4 reads a symptom instead of the cause.
    ('M-VXO-FIRST',
     'let a later trigger displace the held cause, so the retry latches the '
     'most recent symptom rather than the first cause',
     [
         ('cmd/harness/run.go',
          '\t\tcause = o.stopCause\n',
          '\t\to.stopCause = cause\n'),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # A missing `return`, which is the most plausible way to write this wrong.
    # Falling through releases the hold it just took -- and raises the
    # LATCH_WRITE_RECOVERED that says the stop is durable -- one line after
    # discovering that it is not.
    ('M-VXO-FALLTHROUGH',
     'fall through after holding the cause, so the hold is released and '
     'announced recovered on the same tick the write failed',
     [
         ('cmd/harness/run.go',
          '\t\to.holdStop(cause)\n\t\treturn\n\t}\n',
          '\t\to.holdStop(cause)\n\t}\n'),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # The signal seam. A SIGTERM is delivered ONCE, so there is no standing
    # condition left to recur and this is the one trigger a dropped decision
    # loses outright -- while `SignalController.Handle`'s own contract promises
    # that on a failed write "the harness still stops adding and still drains".
    ('M-VXO-SIGNAL',
     'drop a SIGTERM whose latch write failed, so H-HALT-3 stops nothing and '
     'the one trigger that cannot fire twice is lost outright',
     [
         ('cmd/harness/run.go',
          '\t\to.holdStop(eff.Cause)\n\t\to.holdSignal(eff.Cause)\n',
          ''),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # `rig.boot`, whose field comment says outright that its two answers "are
    # answers the run loop must honour on its first tick, not diagnostics".
    # Only `.Latched` and `.Anomalies` were ever read.
    ('M-VXO-BOOT',
     'ignore the bootstrap BlockAdding and RetryLatch, so a latch that could '
     'not be read is never rewritten and the first tick honours neither',
     [
         ('cmd/harness/run.go',
          '\tif r.boot.BlockAdding || r.boot.RetryLatch {\n',
          '\tif false && (r.boot.BlockAdding || r.boot.RetryLatch) {\n'),
     ],
     'TestAnUnreadableLatchAtBootIsHonouredOnTheFirstTick'),

    # The three lip-vxo amendments, from the codex review OF the implementation.

    # `RetryLatch` during STARTING. `BlockAdding` is the benign half there --
    # nothing places before an Adoption exists -- but a cause that is not
    # retained is one the next walk must re-discover, and the next walk
    # recomputes `since := now.Add(-backfill_h)` from a fresh clock. A fill near
    # the edge of that window is simply not reported again, and the harness
    # adopts a clean account next to a stop it had already decided to take.
    ('M-VXO-STARTUPDROP',
     'drop a startup cause whose latch write failed, so it survives only if the '
     'moving backfill window still reports the fill that produced it',
     [
         ('harness/lifecycle/startup.go',
          '\t\t\ts.pendingCause, s.pendingHeld = c, true\n',
          '\t\t\ts.pendingHeld = false\n'),
     ],
     'TestAStartupCauseSurvivesAFailedWriteAndTheBackfillWindowMovingPast'),

    # The same cause, retained but never retried: the walk goes first and the
    # held cause is only ever written if some LATER walk rediscovers it.
    ('M-VXO-STARTUPNORETRY',
     'walk before retrying the retained startup cause, so the retry depends on '
     'the same rediscovery it exists to make unnecessary',
     [
         ('harness/lifecycle/startup.go',
          '\tif anoms, ok := s.retryPending(); !ok {\n\t\treturn s.latchBlocked(walkResult{anoms: anoms})\n\t}\n',
          ''),
     ],
     'TestAStartupCauseSurvivesAFailedWriteAndTheBackfillWindowMovingPast'),

    # H-HALT-3's exit authority. A signal whose write failed drains UNPLANNED,
    # and an unplanned drain can never authorise an exit. Committing the same
    # cause later makes the stop durable and leaves the drain unplanned forever,
    # so the operator's SIGTERM stops the harness, winds it down, reaches flat
    # and then idles rather than finishing.
    ('M-VXO-NOPERMITREISSUE',
     'never re-issue the drain permit once a signal stop becomes durable, so a '
     'SIGTERM whose latch write failed once can never end the process',
     [
         ('cmd/harness/run.go',
          '\t\to.holdSignal(eff.Cause)\n',
          ''),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # The same authority, dropped at the release instead of at the signal.
    ('M-VXO-NOCONFIRMDRAIN',
     'hold the signal intent and never act on it, so the permit is retained as '
     'a diagnostic and the wind-down still cannot end',
     [
         ('cmd/harness/run.go',
          '\to.confirmSignalDrain()\n}\n',
          '}\n'),
     ],
     'TestAStopThatCouldNotBeMadeDurableBlocksAddingAndIsRetriedUntilItIs'),

    # -----------------------------------------------------------------------
    # lip-xdq -- §5.1 was half wired: `advance` had ONE caller and passed no
    # state, so `GTStop` was unreachable and the ordinary edges never fired.
    # -----------------------------------------------------------------------

    # The trigger. `CommitStop` sets the cached latch before `Advance` injects
    # it, so A14 -- checked before every other rule -- fires ahead of the
    # RUNNING+Stop rule on every commit-then-advance. Reporting `halt_latch`
    # there tells the operator of §10.4 that this process INHERITED a halt from
    # a previous incarnation when it had just taken one, and the two call for
    # opposite responses.
    ('M-XDQ-TRIGGER',
     'report a live §12 stop as `halt_latch`, so a taker fill and a restart '
     'into a previous incarnation\'s halt are indistinguishable in A9',
     [
         ('harness/quote/machine.go',
          '\t\t\treturn WindingDown, GTStop\n\t\t}\n\t\treturn WindingDown, GTLatch\n',
          '\t\t\treturn WindingDown, GTLatch\n\t\t}\n\t\treturn WindingDown, GTLatch\n'),
     ],
     'TestEveryGlobalTransitionRecordsItsOwnCause'),

    # The ordinary edges. Without this call `advance` is reachable only from the
    # stop funnel, so WINDING_DOWN -> DRAINED and DRAINED -> WINDING_DOWN are
    # defined in the machine and asked for by nothing: a harness that wound down
    # and reduced to flat reports WINDING_DOWN for the rest of its life.
    ('M-XDQ-NOTICK',
     'never evaluate the global machine on an ordinary tick, so DRAINED is '
     'never published and inventory reappearing under it has no edge to take',
     [
         ('cmd/harness/run.go',
          '\to.advance(o.globalFacts())\n',
          ''),
     ],
     'TestEveryGlobalTransitionRecordsItsOwnCause'),

    # The state. Left at the zero value the machine is told STARTING on every
    # advance, so A14 forces WINDING_DOWN from whatever the process was actually
    # in -- and the drain edge, which is defined only out of WINDING_DOWN, can
    # never be reached from DRAINED or evaluated from the real state.
    ('M-XDQ-NOSTATE',
     'advance without the current state, so the machine adjudicates every tick '
     'from STARTING and A14 overwrites whatever the process was really in',
     [
         ('cmd/harness/run.go',
          '\treturn quote.GlobalInput{\n\t\tState:         o.global,\n',
          '\treturn quote.GlobalInput{\n'),
     ],
     'TestEveryGlobalTransitionRecordsItsOwnCause'),

    # The drain's own evidence. §5.1 requires the flags to be ANSWERS: a
    # WINDING_DOWN harness that reports no inventory because nobody asked drains
    # over a position it is still holding, and DRAINED rests no reducer.
    ('M-XDQ-DRAINBLIND',
     'tell the global machine there is never any inventory, so the drain '
     'completes over a position and nothing re-enters WINDING_DOWN for it',
     [
         ('cmd/harness/run.go',
          '\t\tAnyInventory:  o.anyInventory(),\n\t\tAnyLiveOrder:  o.anyLiveOrder(),\n\t}\n}\n',
          '\t\tAnyInventory:  false,\n\t\tAnyLiveOrder:  o.anyLiveOrder(),\n\t}\n}\n'),
     ],
     'TestEveryGlobalTransitionRecordsItsOwnCause'),

    # -----------------------------------------------------------------------
    # lip-2t6 -- the canary first-fill latch (pilot-plan §7.9)
    #
    # The rule these defend: on the canary rung ONLY, the first directional
    # entry after adoption latches a durable global WINDING_DOWN, at any size
    # down to the 0.01-contract quantum. No assignment of §16's numbers
    # delivers that -- F17 compares with a strict `>` and inv_kill must sit
    # strictly above inv_hard, which is market-scoped and self-clears at flat.
    # -----------------------------------------------------------------------

    # The policy itself. Without the flag the canary is a pilot with a smaller
    # S, which is exactly the configuration the bead exists to say is not
    # enough.
    ('M-2T6-RUNGOFF',
     'take the first-fill bound off the canary rung, leaving it bounded only '
     'by §16 numbers that provably cannot bound it',
     [
         ('cmd/harness/config.go',
          '\t\tstopOnFirstOwnedFill: true,\n',
          ''),
     ],
     'TestTheCanaryLatchesOnAnyPositionItDidNotStartWith'),

    # The other direction: a rule that fires on every rung collapses the whole
    # capital ladder into one step, and the pilot stops at the quantum.
    ('M-2T6-EVERYRUNG',
     'apply the canary first-fill latch on every rung, so the pilot stops at '
     'the 0.01 quantum and the ladder has one step',
     [
         ('cmd/harness/run.go',
          '\tif !o.r.cfg.Rung.stopOnFirstOwnedFill {\n\t\treturn\n\t}\n',
          ''),
     ],
     'TestThePilotRungIgnoresThePositionTheCanaryStopsFor'),

    # §8.2's source. The ack's own count moves q before any fills walk runs, so
    # dropping it leaves the canary up to one position_poll_s late on the one
    # event it exists for -- and it may place another order inside that window.
    ('M-2T6-NOACK',
     'ignore a create acknowledgement that came back carrying a fill, so the '
     'canary waits for the fills walk to corroborate its own first trade',
     [
         ('cmd/harness/run.go',
          '\t\t\to.canaryStop("canary_ack_fill", res.Req.Market)\n',
          ''),
     ],
     'TestTheCanaryLatchesOnAnAcknowledgementThatCarriedAFill'),

    # The fills-walk source: the only one carrying trade identity, and the one
    # the rule is actually written about.
    ('M-2T6-NOOWNEDFILL',
     'ignore a newly classified owned fill, leaving the canary bounded only by '
     'the ack path and the position fallback',
     [
         # Re-anchored at lip-da6: the loop now offers `BackfilledFill` to the
         # same rule, so the range clause it deletes is the concatenation.
         ('cmd/harness/run.go',
          '\tfor _, f := range append(append([]risk.FillEvent{}, eff.OwnedFill...),\n'
          '\t\teff.BackfilledFill...) {\n\n\t\tif o.liveOwnedFill(f) {\n'
          '\t\t\to.canaryStop("canary_owned_fill", f.Ticker)\n\t\t}\n\t}\n',
          ''),
     ],
     'TestTheCanaryLatchesOnANewlyClassifiedOwnedFill'),

    # The entry fallback. It is what catches an entry whose fill record we
    # never saw -- a fill dropped from a walk, a binding that never committed,
    # an exchange that reports the position and not the trade.
    ('M-2T6-NOPOSITION',
     'drop the position entry fallback, so an entry whose fill record never '
     'arrived leaves the canary running',
     [
         ('cmd/harness/run.go',
          '\tif eff.Applied[wsx.TruthPositions] {\n\t\tfor _, rec := range eff.Records {\n'
          '\t\t\tif rec.QLocal == 0 && rec.QExch != 0 {\n'
          '\t\t\t\to.canaryStop("canary_position_nonzero", rec.Ticker)\n'
          '\t\t\t}\n\t\t}\n\t}\n',
          ''),
     ],
     'TestTheCanaryLatchesOnAnyPositionItDidNotStartWith'),

    # The fallback fires on a TRANSITION and not on a reading. Adoption seeds
    # q_local from the exchange, so without the flat test every restart of a
    # canary that holds anything latches immediately -- which is every restart
    # after its first fill, and the position it stopped for is one it was
    # already managing.
    ('M-2T6-POSANY',
     'latch on any nonzero position rather than on the transition from flat, '
     'so a canary restarted while holding inventory stops on its own adoption',
     [
         ('cmd/harness/run.go',
          '\t\t\tif rec.QLocal == 0 && rec.QExch != 0 {\n',
          '\t\t\tif rec.QExch != 0 {\n'),
     ],
     'TestStartupSeededInventoryNeverInvokesTheCanary'),

    # H-PAGE-1 on the fallback. This one is INERT, and it is carried because
    # the argument for why is the reason the guard is written the way it is.
    #
    # `wsx.applyPositions` appends to `eff.Records` and sets
    # `eff.Applied[TruthPositions]` in the same branch, and returns before both
    # when the walk did not replace -- so `len(Records) > 0` already implies
    # `Applied`, and dropping the test changes no behaviour this codebase can
    # produce. The guard earns its place by not RESTING on that: "there are
    # records, so the walk must have completed" is an inference about another
    # package's internals, and this is a safety rule. `wsx` is free to report a
    # record from a read it did not apply -- a partial credit, a diagnostic
    # row -- and the day it does, the version with the test still refuses and
    # the version without it latches the canary from a reading H-PAGE-1 says is
    # stale rather than empty.
    #
    # What DOES ratchet the behaviour is
    # `TestAnIncompletePositionWalkNeverLatchesTheCanary`, which breaks the
    # positions endpoint outright and then repairs it: the first half fails if
    # anything latches from a walk that did not complete, and the second half
    # fails if the fallback has been made dead.
    ('M-2T6-POSINCOMPLETE',
     'read position records without checking that the walk replaced',
     [
         ('cmd/harness/run.go',
          '\tif eff.Applied[wsx.TruthPositions] {\n\t\tfor _, rec := range eff.Records {\n',
          '\tif true {\n\t\tfor _, rec := range eff.Records {\n'),
     ],
     'inert'),

    # First-writer-wins. Both causes are true of a taker fill on the canary and
    # only one of them says H-Q-3 has been violated. Moving the canary block
    # above the stop funnel writes the mundane one.
    ('M-2T6-CANARYFIRST',
     'offer the canary cause before the portfolio read\'s own stop, so a taker '
     'or foreign fill latches as `canary_owned_fill`',
     [
         ('cmd/harness/run.go',
          '\tif eff.Stop {\n\t\to.requestStop("portfolio_read", "")\n\t}\n',
          ''),
         ('cmd/harness/run.go',
          '\tif eff.Applied[wsx.TruthOrders] {\n',
          '\tif eff.Stop {\n\t\to.requestStop("portfolio_read", "")\n\t}\n'
          '\tif eff.Applied[wsx.TruthOrders] {\n'),
     ],
     'TestATakerFillOnTheCanaryKeepsTheStrongerCause'),

    # The history boundary, defeated outright.
    ('M-2T6-NOBASELINE',
     'treat every owned fill as live, so a canary that has ever traded latches '
     'on its own history at every restart',
     [
         ('cmd/harness/run.go',
          '\t_, history := o.startupTrades[f.TradeID]\n\treturn !history\n',
          '\treturn true\n'),
     ],
     'TestStartupHistoryIsNeverALiveCanaryFill'),

    # The boundary, defeated subtly, and this is the one no timestamp can fix.
    # §7.5 asks for backfill_h; the live poll asks for all history with a zero
    # `since`; the filter is client-side after a complete walk. A baseline
    # built from the FILTERED slice omits every trade older than the window,
    # and the first live walk then offers those as brand new.
    ('M-2T6-BASELINEPOSTFILTER',
     'build the startup baseline from the backfill_h slice instead of the '
     'complete walk, so any trade older than the window reads as live',
     [
         ('harness/lifecycle/startup.go',
          '\tfor _, id := range fills.AllTradeIDs {\n\t\ts.baseline[id] = struct{}{}\n\t}\n',
          '\tfor _, f := range fills.Fills {\n\t\ts.baseline[f.TradeID] = struct{}{}\n\t}\n'),
     ],
     'TestStartupHistoryIsNeverALiveCanaryFill'),

    # And the accumulation. §7.5 retries, and a pass that cancels anything
    # discards its adoption and rewalks -- but every one of those passes ran
    # before the licence to leave STARTING existed.
    ('M-2T6-BASELINEPERATTEMPT',
     'reset the startup baseline on every attempt, so a trade only the '
     'discarded pass saw is forgotten and reads as live',
     [
         ('harness/lifecycle/startup.go',
          '\tfor _, id := range fills.AllTradeIDs {\n',
          '\ts.baseline = make(map[string]struct{})\n'
          '\tfor _, id := range fills.AllTradeIDs {\n'),
     ],
     'TestTheStartupBaselineIsPreFilterAndSpansEveryAttempt'),

    # The identity carrier itself. Populated from the filtered slice it is a
    # second copy of `Fills` wearing the name of the complete walk.
    ('M-2T6-ALLIDSFILTERED',
     'record the pre-filter trade ids only for the records that survived the '
     'filter, which is the filtered slice under another name',
     [
         ('harness/rest/read.go',
          '\t\tids = append(ids, f.TradeID)\n\t\tif !since.IsZero() && f.TsMillis != 0 &&\n'
          '\t\t\tf.TsMillis < since.UnixMilli() {\n\t\t\tcontinue\n\t\t}\n',
          '\t\tif !since.IsZero() && f.TsMillis != 0 &&\n'
          '\t\t\tf.TsMillis < since.UnixMilli() {\n\t\t\tcontinue\n\t\t}\n'
          '\t\tids = append(ids, f.TradeID)\n'),
     ],
     'TestFillsTimeFilterIsAppliedAfterTheCompleteWalk'),

    # §7.5's owned history is the ONLY thing that will ever write these rows:
    # `risk.Seed` marks their trade ids seen, so the first live walk
    # deduplicates them away and the other caller of RecordFill never sees
    # them. H-ORD-6 makes our_fill the join against rig.db.
    ('M-2T6-NOBACKFILL',
     'drop the adoption\'s owned history instead of recording it, so our_fill '
     'begins at whichever fill this incarnation happened to watch land',
     [
         ('cmd/harness/run.go',
          '\to.recordBackfilled(a.OwnedFills())\n',
          ''),
     ],
     'TestStartupHistoryIsNeverALiveCanaryFill'),

    # And the flag. These rows were INFERRED from a walk over the past, not
    # observed happening, and an analysis that cannot tell them apart reads
    # every restart as a burst of trading.
    ('M-2T6-BACKFILLFLAG',
     'record the adoption\'s history as though this process watched it happen',
     [
         ('cmd/harness/run.go',
          '\t\tif _, err := o.r.store.RecordFill(o.r.run, f, o.r.ex.NowMs(),\n'
          '\t\t\ttrue); err != nil {\n',
          '\t\tif _, err := o.r.store.RecordFill(o.r.run, f, o.r.ex.NowMs(),\n'
          '\t\t\tfalse); err != nil {\n'),
     ],
     'TestStartupHistoryIsNeverALiveCanaryFill'),

]


# Files OUTSIDE `go/` that the Go tests read, and which the mutation sandbox
# must therefore reproduce.
#
# This exists because of a false-green that very nearly shipped.
# `TestTheShippedExampleConfigLoads` reads `../../../config.example.json` --
# the operator-facing example, which lives at the repo root because that is
# where an operator looks for it. The sandbox copied only `go/`, so in every
# mutated tree that test failed for want of a file. `caught` is computed as
# "the suite went red", so EVERY mutation then looked caught no matter what it
# did, and a genuinely SURVIVING mutation would have been reported as caught.
#
# What surfaced it was the two INERT mutations: an inert mutation is correct to
# survive, so a suite that is red for an unrelated reason flips it to NOT
# INERT and the gate goes red. They are carried as arguments about code that
# cannot matter; here they earned their keep a second way, as canaries for a
# corrupted gate.
ROOT_ARTIFACTS = ("config.example.json",)


def copy_root_artifacts(sandbox: Path) -> None:
    for name in ROOT_ARTIFACTS:
        src = LIP / name
        if not src.exists():
            # Refuse rather than run a suite that will fail for the wrong
            # reason and call the result evidence.
            raise SystemExit(f"{name} is missing from {LIP}; the Go tests read "
                             f"it and the mutation sandbox cannot reproduce it")
        shutil.copy2(src, sandbox / name)


def run(cmd: list[str], cwd: Path) -> tuple[int, str]:
    p = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def failed_tests(output: str) -> list[str]:
    return sorted(set(re.findall(r"^--- FAIL: (\S+)", output, re.M)))


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--out", default=str(LIP / "notes" /
                                         "harness-negative-control.md"))
    ap.add_argument("--only", default="")
    a = ap.parse_args()
    only = {x.strip().upper() for x in a.only.split(",") if x.strip()}

    # An --only value that matches nothing selects ZERO mutations, and a run of
    # zero mutations trivially "passes". That is the same false-green this
    # script exists to detect, reachable by a typo. Reject it rather than
    # reporting success for having checked nothing (H-PAGE-1a's lesson:
    # "absence is not evidence" reappearing as a spelling mistake).
    known = {m[0].upper() for m in MUTATIONS}
    if only:
        unknown = only - known
        if unknown:
            print(f"unknown mutation id(s): {sorted(unknown)}\n"
                  f"known: {sorted(known)}", file=sys.stderr)
            return 2

    # Baseline: the pristine tree must be green, or nothing below means
    # anything. A mutation "caught" by an already-red suite is not evidence.
    code, out = run([GO_BIN, "test", "-count=1", *PKGS], GO_SRC)
    if code != 0:
        print("BASELINE IS RED -- refusing to run mutations.\n" + out[-3000:],
              file=sys.stderr)
        return 1
    print("baseline: green")

    rows = []
    for mid, name, patches, expected in MUTATIONS:
        if only and mid.upper() not in only:
            continue
        with tempfile.TemporaryDirectory() as td:
            dst = Path(td) / "go"
            shutil.copytree(GO_SRC, dst)
            copy_root_artifacts(Path(td))
            for rel, old, new in patches:
                f = dst / rel
                src = f.read_text()
                n = src.count(old)
                if n != 1:
                    print(f"{mid}: anchor appears {n}x in {rel}, need exactly 1",
                          file=sys.stderr)
                    return 1
                f.write_text(src.replace(old, new))

            build, bout = run([GO_BIN, "build", "./..."], dst)
            if build != 0:
                rows.append((mid, name, expected, "DID NOT BUILD",
                             bout.strip().splitlines()[:2], False))
                print(f"!! {mid}: DID NOT BUILD (does not count as caught)")
                continue

            code, out = run([GO_BIN, "test", "-count=1", *PKGS], dst)
            fails = failed_tests(out)
            caught = code != 0
            if expected == "inert":
                # An inert mutation is CORRECT to survive. It is kept in the
                # suite because "we checked, and it genuinely cannot matter" is
                # a finding worth re-verifying whenever the code around it
                # changes -- and because an inert declaration is exactly how a
                # real gate hole would hide.
                ok = not caught
                status = "inert, as expected" if ok else "NOT INERT"
            else:
                ok = caught and expected in fails
                status = "caught" if caught else "SURVIVED"
                if caught and not ok:
                    status = "caught, but NOT by the named test"
            rows.append((mid, name, expected, status, fails, ok))
            print(f"{'ok' if ok else '!!'} {mid}: {status} -> {fails}")

    lines = [
        "# V5 — negative control for the harness",
        "",
        "Generated by `scripts/harness_negative_control.py`. Each row is a",
        "deliberate violation of one invariant from `harness-spec.md` §17 V3,",
        "applied to a pristine copy of `lip/go`, then run through the harness",
        "test suite.",
        "",
        "A mutation that SURVIVES is a defect in the verification, not a",
        "curiosity. A mutation that DID NOT BUILD does not count as caught —",
        "the Go compiler is not a gate.",
        "",
        "| id | mutation | expected catching test | result | tests that failed |",
        "|---|---|---|---|---|",
    ]
    for mid, name, expected, status, fails, ok in rows:
        f = ", ".join(fails) if isinstance(fails, list) else str(fails)
        lines.append(f"| {mid} | {name} | `{expected}` | **{status}** | "
                     f"{f or '—'} |")
    good = sum(1 for r in rows if r[5])
    lines += ["",
              f"**{good} of {len(rows)} mutations produced their expected "
              f"outcome.**",
              "",
              "A mutation that SURVIVES unexpectedly is a defect in the",
              "verification and the harness does not ship until the gate is",
              "strengthened. A mutation declared `inert` must carry a positive",
              "argument in `harness_negative_control.py` — never an assumption,",
              "because an inert declaration is exactly how a real gate hole",
              "would hide.",
              ""]
    # A partial run must not overwrite the canonical report. Otherwise a
    # `--only m01` run silently replaces the record of all 23 mutations with a
    # record of one, and the file that documents the verification becomes the
    # place the verification disappears.
    if only:
        out_path = Path(a.out).with_suffix(".partial.md")
        print(f"\npartial run ({sorted(only)}) -- writing {out_path.name}, "
              f"not the canonical report")
    else:
        out_path = Path(a.out)
    out_path.write_text("\n".join(lines) + "\n")
    print(f"\nwrote {out_path}")

    # THE EXIT CODE. This function previously returned 0 unconditionally: it
    # counted `good`, printed "!! SURVIVED", wrote the report -- and exited
    # success. Every automated caller therefore read a surviving mutation, which
    # is by definition a hole in the verification, as a passing gate.
    #
    # That is this repository's own thesis turned on itself. §17 V5 exists
    # because "a gate that has never been shown to fail is not evidence", and
    # the gate on the gate could not fail. Found by an independent model reading
    # the source, not by any test here.
    bad = [r for r in rows if not r[5]]
    if bad:
        print(f"\n{len(bad)} of {len(rows)} mutations did NOT produce their "
              f"expected outcome:", file=sys.stderr)
        for mid, name, expected, status, fails, _ in bad:
            print(f"  {mid}: {status} (expected to be caught by {expected})",
                  file=sys.stderr)
        print("\nA mutation that SURVIVES is a defect in the VERIFICATION. The "
              "harness does not ship until the gate is strengthened.",
              file=sys.stderr)
        return 1

    # A full run must have exercised every mutation in the catalogue. A
    # mutation that quietly vanishes from MUTATIONS takes its coverage with it
    # and leaves the count looking healthy.
    if not only and len(rows) != len(MUTATIONS):
        print(f"\nexpected {len(MUTATIONS)} mutations, ran {len(rows)}",
              file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
