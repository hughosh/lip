package wsx

import (
	"testing"
	"time"

	"lip/core"
	"lip/harness/risk"
)

// recorder is a core.Sink that counts rows. It exists so a test can assert
// what did and did not reach `core`, rather than asserting on this package's
// own opinion of what it forwarded.
type recorder struct {
	fills int
	refs  int
}

func (r *recorder) Fill(core.FillRow)                  { r.fills++ }
func (r *recorder) PendingMid(_, _, _ string, _ int64) {}
func (r *recorder) Reference(core.ReferenceRow)        { r.refs++ }

// TestFractionalBookPriceNeverReachesCore is H-CO-3a's assertion, tested
// against `core` itself rather than against this package's return value.
//
// The distinction matters. An InspectFrame that returned Deliver=false while
// the caller handed the frame to core anyway would pass any test that only
// read the verdict. What must be true is that the LEVEL never enters a Book:
// once it has, the touch the book implies is already wrong, every requote
// decision is taken against it, and quarantining afterwards only stops us
// compounding the error.
//
// The response is `SEV2 BOOK_PRICE_GRANULARITY` and that market to REDUCING. It
// is not a rounding problem. Every level the exchange has ever published has
// been an integer cent, and the harness's prices, caps and self-cross checks
// are integer-cent arithmetic throughout; a fractional level means the tick
// size changed, which is the exchange's to change and ours to detect.
func TestFractionalBookPriceNeverReachesCore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"snapshot", fxSnapshotFractional},
		{"delta", fxDeltaFractional},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewGate([]string{fxTicker}, testParams())
			if err != nil {
				t.Fatal(err)
			}
			now := at(0)
			g.OnConnect(now)

			info := InspectFrame([]byte(tc.frame))
			if !info.Granularity {
				t.Fatalf("a fractional book price was not detected: %+v", info)
			}
			if info.Deliver {
				t.Fatal("a fractional book price was marked deliverable")
			}
			sev, ok := sevOf(info.Anomalies, "BOOK_PRICE_GRANULARITY")
			if !ok || sev != risk.SEV2 {
				t.Fatalf("anomalies = %v, want SEV2 BOOK_PRICE_GRANULARITY",
					info.Anomalies)
			}

			// ApplyFrame OWNS the delivery, so the handler below is the only
			// route to core and the assertion is that it was never called.
			var rec recorder
			rig := core.NewRig(&rec, map[string]float64{fxTicker: 100})
			handled := 0
			eff := g.ApplyFrame(info, func() error {
				handled++
				return rig.Handle([]byte(tc.frame))
			}, now)

			if handled != 0 {
				t.Fatal("a fractional book frame was handed to core; H-CO-3a's " +
					"whole point is that the level must never enter a Book")
			}
			if eff.Delivered {
				t.Fatal("the gate reported delivering a fractional book frame")
			}
			if len(eff.Reduce) != 1 || eff.Reduce[0] != fxTicker {
				t.Fatalf("reduce = %v, want the offending market", eff.Reduce)
			}
			if g.Actionable(fxTicker, now) {
				t.Fatal("the market is still actionable after a granularity " +
					"violation")
			}
			if b := rig.Book(fxTicker); b != nil {
				if _, _, ok := b.BestYes(); ok {
					t.Fatal("a fractional level reached core and produced a touch")
				}
			}
		})
	}
}

// TestCleanBookFramesStillReachCore is the other half of the detector: it must
// not reject the ordinary case, or the harness never quotes at all.
func TestCleanBookFramesStillReachCore(t *testing.T) {
	var rec recorder
	rig := core.NewRig(&rec, map[string]float64{fxTicker: 100})

	for _, frame := range []string{fxSnapshot, fxDelta} {
		info := InspectFrame([]byte(frame))
		if info.Granularity || !info.Deliver {
			t.Fatalf("an integer-cent frame was rejected: %+v", info)
		}
		if len(info.Anomalies) != 0 {
			t.Fatalf("an ordinary frame raised %v", classesOf(info.Anomalies))
		}
		if err := rig.Handle([]byte(frame)); err != nil {
			t.Fatalf("core rejected a frame this package passed: %v", err)
		}
	}
	b := rig.Book(fxTicker)
	p, _, ok := b.BestYes()
	if !ok || p != 42 {
		t.Fatalf("best yes = %d (present=%v), want 42c from the fixture", p, ok)
	}
}

// TestFractionalTradePriceRemainsLegal is the boundary of H-CO-3a.
//
// The assertion is about the RESTING BOOK, not the tape. 17.08% of prints are
// fractional cents, `core.ParsePriceCents` exists precisely to reproduce
// CPython's banker's rounding over them, and a granularity check applied to the
// trade stream would reject a sixth of the exchange's prints as an anomaly --
// turning the cheapest detector in the system into noise nobody reads.
func TestFractionalTradePriceRemainsLegal(t *testing.T) {
	info := InspectFrame([]byte(fxTrade))
	if info.Kind != FrameTrade {
		t.Fatalf("kind = %v, want trade", info.Kind)
	}
	if info.Granularity {
		t.Fatal("a fractional PRINT was treated as a book granularity " +
			"violation; the assertion is about the resting book")
	}
	if !info.Deliver {
		t.Fatal("a fractional print was withheld from core")
	}
	if len(info.Anomalies) != 0 {
		t.Fatalf("a fractional print raised %v", classesOf(info.Anomalies))
	}

	g, err := NewGate([]string{fxTicker}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	now := at(0)
	connectAndReconcile(g, fxTicker, now)
	eff := g.ApplyFrame(info, okHandle, now)
	if !eff.Delivered {
		t.Fatal("a fractional print was withheld from core")
	}
	if len(eff.Reduce) != 0 {
		t.Fatalf("a fractional print reduced %v", eff.Reduce)
	}
	if !g.Actionable(fxTicker, now) {
		t.Fatal("a fractional print made the market non-actionable")
	}
}

// TestCoreRejectedBookFrameCannotUnlockOrRefreshGate is the second half of the
// delivery transaction, and it is a different failure from H-CO-3a's.
//
// A frame can pass every check this package makes -- whole-cent prices, a
// decodable envelope -- and still be refused by `core`, which rejects the WHOLE
// frame and leaves the book exactly as it was. If the gate recorded the market
// as snapshotted anyway, the sequence is:
//
//	clean disconnect, so the old book is RETAINED for diagnostics
//	 -> reconnect -> a snapshot arrives whose sizes do not parse
//	 -> core refuses it; the book is still the pre-disconnect one
//	 -> the gate marks snapGen current and lifts the quarantine
//	 -> we quote against a book from the far side of the gap
//
// So freshness commits only after the handler returns nil.
func TestCoreRejectedBookFrameCannotUnlockOrRefreshGate(t *testing.T) {
	// Whole-cent prices, and a size core cannot parse. Nothing this package
	// checks can see the problem. `seq` is 2 so each fixture follows the good
	// snapshot below without tripping core's own gap detector.
	const badSizeSnapshot = `{"type":"orderbook_snapshot","sid":7,"seq":2,"msg":{` +
		`"market_ticker":"` + fxTicker + `","ts_ms":1754400000000,` +
		`"yes_dollars_fp":[["0.4200","not-a-size"]],` +
		`"no_dollars_fp":[["0.5700","88.00"]]}}`
	const badSizeDelta = `{"type":"orderbook_delta","sid":7,"seq":2,"msg":{` +
		`"market_ticker":"` + fxTicker + `","ts_ms":1754400001000,` +
		`"side":"yes","price_dollars":"0.4200","delta_fp":"not-a-size"}}`

	for _, tc := range []struct {
		name  string
		frame string
	}{
		{"snapshot", badSizeSnapshot},
		{"delta", badSizeDelta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			var rec recorder
			rig := core.NewRig(&rec, map[string]float64{fxTicker: 100})

			info := InspectFrame([]byte(tc.frame))
			if info.Granularity || !info.Deliver {
				t.Fatalf("this fixture must pass THIS package's checks so the "+
					"test is about core's refusal: %+v", info)
			}
			// Prime the book: core drops deltas for a market it has never
			// snapshotted, so without this the delta case would be refused for
			// the wrong reason.
			if err := rig.Handle([]byte(fxSnapshot)); err != nil {
				t.Fatal(err)
			}
			if err := rig.Handle([]byte(tc.frame)); err == nil {
				t.Fatal("core accepted the fixture; the test proves nothing")
			}

			g, err := NewGate([]string{fxTicker}, p)
			if err != nil {
				t.Fatal(err)
			}
			// A market that WAS current, then a clean disconnect that keeps the
			// old book, then a reconnect.
			connectAndReconcile(g, fxTicker, at(0))
			g.ApplyDisconnect(at(10), true)
			ce := g.OnConnect(at(20))
			for k := Truth(0); k < truthCount; k++ {
				g.noteTruth(k, ce.Token, at(20))
			}

			eff := g.ApplyFrame(info, rejectHandle("sizes do not parse"), at(21))

			if eff.Delivered {
				t.Fatal("a frame core refused was reported as delivered")
			}
			if !hasClass(eff.Anomalies, "CORE_BOOK_FRAME_REJECTED") {
				t.Fatalf("core's refusal was silent: %v", classesOf(eff.Anomalies))
			}
			if !eff.Resnapshot {
				t.Fatal("a refused book frame did not request a resnapshot")
			}
			if g.Actionable(fxTicker, at(21)) {
				t.Fatal("a book frame core REFUSED certified the market as " +
					"current; the book is still the pre-disconnect one")
			}

			// The silence clock must not restart either, or a market whose
			// every frame core refuses looks busy forever and F5 never fires.
			quiet := time.Duration(p.Quiet)
			tick := g.Tick(Stamp{Mono: at(21).Mono + quiet + time.Second})
			if len(tick.Reduce) != 1 || tick.Reduce[0] != fxTicker {
				t.Fatalf("reduce = %v; a market whose frames core keeps "+
					"refusing is not publishing, and its quiet clock must not "+
					"have been refreshed by the refusal", tick.Reduce)
			}

			// An accepted frame afterwards does unlock it, so the assertion
			// above is about the refusal and not about something else.
			good := InspectFrame([]byte(fxSnapshot))
			g2, err := NewGate([]string{fxTicker}, p)
			if err != nil {
				t.Fatal(err)
			}
			ce2 := g2.OnConnect(at(30))
			for k := Truth(0); k < truthCount; k++ {
				g2.noteTruth(k, ce2.Token, at(30))
			}
			if e := g2.ApplyFrame(good, okHandle, at(31)); !e.Delivered {
				t.Fatalf("an acceptable snapshot was not delivered: %+v", e)
			}
			if !g2.Actionable(fxTicker, at(31)) {
				t.Fatal("an accepted snapshot did not unlock the market")
			}
		})
	}
}

// TestDeliverableFrameWithNoHandlerFailsClosed covers the wiring defect.
func TestDeliverableFrameWithNoHandlerFailsClosed(t *testing.T) {
	g, err := NewGate([]string{fxTicker}, testParams())
	if err != nil {
		t.Fatal(err)
	}
	g.OnConnect(at(0))
	eff := g.ApplyFrame(InspectFrame([]byte(fxSnapshot)), nil, at(1))
	if eff.Delivered {
		t.Fatal("a frame was reported delivered with no handler")
	}
	if !hasClass(eff.Anomalies, "FRAME_HANDLER_MISSING") {
		t.Fatalf("anomalies = %v", classesOf(eff.Anomalies))
	}
	if g.Actionable(fxTicker, at(1)) {
		t.Fatal("a frame nothing consumed certified the market")
	}
}

// TestUndecodableFrameIsNotDelivered covers the frame we cannot classify.
//
// A frame that does not decode as an envelope might be a book frame, so we
// cannot certify it as book-safe. Undeliverable and loud beats passed through
// on the assumption that core will cope.
func TestUndecodableFrameIsNotDelivered(t *testing.T) {
	info := InspectFrame([]byte(`{"type":`))
	if info.Deliver {
		t.Fatal("a frame that did not decode was marked deliverable")
	}
	if info.Kind != FrameUnknown {
		t.Fatalf("kind = %v, want unknown", info.Kind)
	}
	if !hasClass(info.Anomalies, "FRAME_UNDECODABLE") {
		t.Fatalf("anomalies = %v", classesOf(info.Anomalies))
	}
}

// TestZeroFrameInfoIsNotDeliverable protects the zero value.
//
// A FrameInfo nobody populated -- a struct returned on a path that forgot to
// set the verdict -- must not read as a deliverable book frame. It is the same
// argument that made `rest.WalkUnset` the zero value rather than
// `WalkComplete`.
func TestZeroFrameInfoIsNotDeliverable(t *testing.T) {
	var zero FrameInfo
	if zero.Deliver {
		t.Fatal("the zero FrameInfo is deliverable")
	}
	if zero.Kind != FrameUnknown {
		t.Fatalf("the zero FrameInfo classifies as %v", zero.Kind)
	}
}

// TestSubscriptionArgumentsThatFailSilentlyAreRejected covers the two
// subscription mistakes the exchange answers with silence rather than an
// error.
func TestSubscriptionArgumentsThatFailSilentlyAreRejected(t *testing.T) {
	if _, err := subscribeDelta(nil); err == nil {
		t.Fatal("an empty subscription was accepted; the exchange accepts it " +
			"too, and delivers nothing")
	}
	if _, err := subscribeDelta([]string{"A", "A"}); err == nil {
		t.Fatal("a duplicated ticker was accepted; every delta for it would " +
			"be delivered twice and the book would carry double depth")
	}
	if _, err := subscribeDelta([]string{"A", ""}); err == nil {
		t.Fatal("an empty ticker was accepted")
	}

	b, err := subscribeDelta([]string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cmd":"subscribe","id":1,"params":{"channels":["orderbook_delta"],` +
		`"market_tickers":["A","B"]}}`
	if string(b) != want {
		t.Fatalf("delta subscription payload:\n got %s\nwant %s", b, want)
	}

	tb, err := subscribeTrade()
	if err != nil {
		t.Fatal(err)
	}
	wantTrade := `{"cmd":"subscribe","id":2,"params":{"channels":["trade"]}}`
	if string(tb) != wantTrade {
		t.Fatalf("trade subscription payload:\n got %s\nwant %s", tb, wantTrade)
	}

	// sids must be an empty array and never JSON null: a null there is a
	// different request, and the difference is only visible in what does not
	// arrive.
	rb, err := resnapshotRequest(nil, []string{"A"})
	if err != nil {
		t.Fatal(err)
	}
	wantRe := `{"cmd":"update_subscription","id":3,"params":{"action":` +
		`"get_snapshot","market_tickers":["A"],"sids":[]}}`
	if string(rb) != wantRe {
		t.Fatalf("resnapshot payload:\n got %s\nwant %s", rb, wantRe)
	}
}

// TestRejectedBookFrameImmediatelyQuarantinesAnActionableMarket is the
// REVOCATION property, and it is a different claim from
// TestCoreRejectedBookFrameCannotUnlockOrRefreshGate.
//
// That test starts after a disconnect, when the market is already unqualified,
// so it can only show that a refused frame does not OPEN the gate. The
// dangerous case is the opposite one: a market that is currently licensed to
// place. A whole-cent delta arrives with a size core cannot parse. Inspection
// accepts it -- there is nothing about the prices to object to -- and core
// refuses the whole frame, leaving the book as it was.
//
// The book is now KNOWN to be behind: we saw a delta we could not apply. If the
// licence survives, placement continues against that book until either a
// resnapshot lands or `quiet_s` elapses -- up to a minute of quoting on a book
// we have already been told is wrong. At `S=12` that is one more order at up to
// 99c, $11.88 in a single market, and the account balance is the only hard
// bound on the general case.
//
// So a refused book frame REVOKES: the market is quarantined and its snapshot
// generation is cleared, immediately, in the same call.
func TestRejectedBookFrameImmediatelyQuarantinesAnActionableMarket(t *testing.T) {
	// Whole cents, unparseable size. Nothing this package checks can object.
	const badSizeDelta = `{"type":"orderbook_delta","sid":7,"seq":2,"msg":{` +
		`"market_ticker":"` + fxTicker + `","ts_ms":1754400001000,` +
		`"side":"yes","price_dollars":"0.4200","delta_fp":"not-a-size"}}`
	const goodDelta = `{"type":"orderbook_delta","sid":7,"seq":3,"msg":{` +
		`"market_ticker":"` + fxTicker + `","ts_ms":1754400002000,` +
		`"side":"yes","price_dollars":"0.4100","delta_fp":"-5.00"}}`

	p := testParams()

	// live builds a gate whose single market is fully licensed to place, plus
	// a real core.Rig with a book already established for it.
	live := func(t *testing.T) (*Gate, *core.Rig, ReconcileToken) {
		t.Helper()
		var rec recorder
		rig := core.NewRig(&rec, map[string]float64{fxTicker: 100})
		if err := rig.Handle([]byte(fxSnapshot)); err != nil {
			t.Fatal(err)
		}
		g, err := NewGate([]string{fxTicker}, p)
		if err != nil {
			t.Fatal(err)
		}
		tok := connectAndReconcile(g, fxTicker, at(0))
		if !g.Actionable(fxTicker, at(0)) {
			t.Fatal("setup: the market must START actionable, or this test " +
				"proves 'cannot unlock' rather than 'must revoke'")
		}
		return g, rig, tok
	}

	for _, tc := range []struct {
		name    string
		handler func(*core.Rig) func() error
	}{
		{"core refuses the frame", func(rig *core.Rig) func() error {
			return func() error { return rig.Handle([]byte(badSizeDelta)) }
		}},
		{"no handler is wired", func(*core.Rig) func() error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, rig, _ := live(t)

			info := InspectFrame([]byte(badSizeDelta))
			if info.Granularity || !info.Deliver {
				t.Fatalf("the fixture must pass THIS package's checks: %+v", info)
			}

			eff := g.ApplyFrame(info, tc.handler(rig), at(1))

			if g.Actionable(fxTicker, at(1)) {
				t.Fatal("a book frame that never reached the book left the " +
					"market licensed to place; we know the book is behind, " +
					"and quoting against it continues until a resnapshot " +
					"lands or quiet_s elapses")
			}
			if eff.Delivered {
				t.Fatal("the frame was reported delivered")
			}
			if !eff.Resnapshot {
				t.Fatal("no resnapshot was requested for a book we know is behind")
			}
			if len(eff.Reduce) != 0 {
				t.Fatalf("reduce = %v; one refused frame is a data-quality "+
					"event a resnapshot repairs within a round trip, and "+
					"reducing on it converts a hiccup into an unwind. If it "+
					"does not recover, the quiet clock is not being refreshed "+
					"either and F5 reduces on the existing path", eff.Reduce)
			}
			if g.Reducing(fxTicker) {
				t.Fatal("the sticky REDUCING flag was raised by one refused frame")
			}

			// The quiet clock must not have been refreshed, or a market whose
			// every frame core refuses looks busy forever and F5 never fires.
			tick := g.Tick(Stamp{Mono: at(1).Mono + p.Quiet + time.Second})
			if len(tick.Reduce) != 1 || tick.Reduce[0] != fxTicker {
				t.Fatalf("reduce after quiet_s = %v; the refusal refreshed the "+
					"silence clock", tick.Reduce)
			}
		})
	}

	// A DELTA that core accepts must not reopen the market. A delta applies an
	// increment to a book we have already been told is wrong; only a full
	// replacement can make it right again.
	t.Run("an accepted delta does not reopen it", func(t *testing.T) {
		g, rig, _ := live(t)
		g.ApplyFrame(InspectFrame([]byte(badSizeDelta)),
			func() error { return rig.Handle([]byte(badSizeDelta)) }, at(1))
		if g.Actionable(fxTicker, at(1)) {
			t.Fatal("setup: the market should be quarantined")
		}

		eff := g.ApplyFrame(InspectFrame([]byte(goodDelta)), okHandle, at(2))
		if !eff.Delivered {
			t.Fatalf("the good delta was not delivered: %+v", eff)
		}
		if g.Actionable(fxTicker, at(2)) {
			t.Fatal("an accepted DELTA reopened a quarantined market; a delta " +
				"is an increment against a book we know is wrong, and only a " +
				"full snapshot replaces it")
		}
	})

	// An accepted SNAPSHOT does, which is what makes the assertions above
	// about the rejection rather than about a gate that never reopens.
	t.Run("an accepted snapshot does reopen it", func(t *testing.T) {
		g, rig, _ := live(t)
		g.ApplyFrame(InspectFrame([]byte(badSizeDelta)),
			func() error { return rig.Handle([]byte(badSizeDelta)) }, at(1))
		if g.Actionable(fxTicker, at(1)) {
			t.Fatal("setup: the market should be quarantined")
		}

		eff := g.ApplyFrame(InspectFrame([]byte(fxSnapshot)), okHandle, at(2))
		if !eff.Delivered {
			t.Fatalf("the resnapshot was not delivered: %+v", eff)
		}
		if !g.Actionable(fxTicker, at(2)) {
			t.Fatal("an accepted snapshot did not restore the market")
		}
	})
}
