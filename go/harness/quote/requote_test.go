package quote

import (
	"testing"
	"time"

	"lip/harness/cfg"
)

// settled is any touch age past the debounce, for the cases not testing it.
const settled = time.Second

// at builds a RequoteInput resting at `ourPrice` against an external touch of
// `touch`, with both debounce clocks already satisfied.
func at(ourPrice, touch int) RequoteInput {
	return RequoteInput{
		Side:        SideYes,
		Role:        RoleAdding,
		OurPrice:    ourPrice,
		OurSize:     qty(12),
		HasOurs:     true,
		Ext:         External{Price: touch, Size: 100, Found: true},
		TouchHeld:   settled,
		StrandedFor: settled,
		Size:        qty(12),
		Headroom:    qty(48),
	}
}

// TestRequoteOnlyChasesAgainstUs is H-Q-6.
//
// "Requote only when the reference moves against us, leaving our order >= 1
// tick behind the touch. Do not requote when it moves in our favour -- our
// order is then the best bid and already scores at N = 0."
//
// The favourable direction is the half that costs money to get wrong, and it
// gets wrong quietly: a requote on every move in either direction is still a
// correct-looking harness that quotes at the touch. It just pays twice the
// writes against §6.6's budget for a move that bought nothing, and every one of
// those writes is a fresh queue position on a side that was already first.
func TestRequoteOnlyChasesAgainstUs(t *testing.T) {
	p := cfg.Default() // debounce 250ms, stale_bid_ticks 8

	for _, tc := range []struct {
		name    string
		our     int
		touch   int
		move    bool
		trigger Trigger
		price   int
		why     string
	}{
		{"one tick behind", 49, 50, true, TriggerBehind, 50,
			"the reference moved up and away from us: H-Q-6's threshold is " +
				"exactly one tick"},
		{"far behind but under the brake", 44, 50, true, TriggerBehind, 50,
			"six ticks behind is still an ordinary chase"},
		{"at the touch", 50, 50, false, TriggerNone, 0,
			"already where H-Q-1 wants us"},
		{"one tick in our favour", 50, 49, false, TriggerNone, 0,
			"the touch fell BELOW us: we are the best bid, already at N = 0, " +
				"and moving down pays a write to buy nothing"},
		{"several ticks in our favour", 50, 45, false, TriggerNone, 0,
			"still inside the stranded threshold, so H-Q-6 still declines"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(at(tc.our, tc.touch), p)
			if got.Move != tc.move {
				t.Fatalf("Move = %v (trigger %s, block %s), want %v -- %s",
					got.Move, got.Trigger, got.Block, tc.move, tc.why)
			}
			if got.Trigger != tc.trigger {
				t.Errorf("Trigger = %s, want %s", got.Trigger, tc.trigger)
			}
			if tc.move && got.Price != tc.price {
				t.Errorf("Price = %dc, want %dc", got.Price, tc.price)
			}
		})
	}
}

// TestStrandedBrakeFiresInBothDirections is H-Q-8.
//
// The rule's text says "behind" and its rationale says the opposite -- "our own
// order is the only thing holding a price level nobody else wants" is our order
// ABOVE the touch. "Regardless of which direction it moved" is what settles it,
// and both cases are independently motivated:
//
//   - eight ticks below the touch is an order that has stopped scoring, in the
//     case H-Q-6 could not fix because the gap opened during a write outage;
//   - eight ticks above it is an order improving the touch by eight ticks,
//     which is the maximal form of the adverse selection H-Q-2 declines to buy
//     one tick at a time.
//
// Implementing one direction leaves the other with no rule at all.
func TestStrandedBrakeFiresInBothDirections(t *testing.T) {
	p := cfg.Default() // stale_bid_ticks = 8

	t.Run("stranded above the touch", func(t *testing.T) {
		// We are the last bid standing at 50; everyone else is at 42.
		got := Decide(at(50, 42), p)
		if !got.Move {
			t.Fatalf("no move (trigger %s, block %s): we are eight ticks "+
				"inside a touch nobody else is at, which is H-Q-8's own "+
				"stated case -- 'our own order is the only thing holding a "+
				"price level nobody else wants'", got.Trigger, got.Block)
		}
		if got.Trigger != TriggerStranded {
			t.Errorf("Trigger = %s, want %s", got.Trigger, TriggerStranded)
		}
		if got.Price != 42 {
			t.Errorf("Price = %dc, want 42c: H-Q-8 snaps back TO the external "+
				"touch, not one tick toward it", got.Price)
		}
	})

	t.Run("stranded below the touch", func(t *testing.T) {
		got := Decide(at(42, 50), p)
		if !got.Move || got.Price != 50 {
			t.Fatalf("Move = %v at %dc, want a move to 50c", got.Move, got.Price)
		}
		if got.Trigger != TriggerStranded {
			t.Errorf("Trigger = %s, want %s -- eight ticks behind is the "+
				"brake, not an ordinary chase", got.Trigger, TriggerStranded)
		}
	})

	t.Run("the threshold is exact", func(t *testing.T) {
		// Seven ticks in our favour is not stranded, and H-Q-6 declines it.
		if got := Decide(at(50, 43), p); got.Move {
			t.Errorf("seven ticks above the touch moved (trigger %s): the "+
				"brake is at stale_bid_ticks = %d, and below it H-Q-6's "+
				"favourable-direction rule governs", got.Trigger, p.StaleBidTicks)
		}
		// Eight is.
		if got := Decide(at(50, 42), p); !got.Move {
			t.Error("eight ticks above the touch did not move")
		}
	})
}

// TestStrandedBrakeIsTestedBeforeTheFavourableDirectionRule pins the ORDER of
// the two rules, which is the whole reason the brake works.
//
// H-Q-6 returns "do not requote" for every move in our favour. H-Q-8 fires on a
// move in our favour that went too far. Evaluate H-Q-6 first and the brake is
// unreachable in exactly the direction its rationale describes -- the harness
// would still pass every test written from the "behind" reading of H-Q-8, and
// would sit inside an abandoned touch indefinitely.
func TestStrandedBrakeIsTestedBeforeTheFavourableDirectionRule(t *testing.T) {
	p := cfg.Default()
	got := Decide(at(60, 40), p) // twenty ticks in our "favour"
	if !got.Move || got.Trigger != TriggerStranded {
		t.Fatalf("Move = %v, trigger %s: an order twenty ticks inside the "+
			"external touch must snap back. If H-Q-6's favourable-direction "+
			"branch returns first, this order never moves again",
			got.Move, got.Trigger)
	}
}

// TestDebounceHoldsTheChase is H-Q-7.
//
// "Requote only once the new touch has held for 250ms. Bursts under 100ms are
// 52.6% of all moves and 0.1% of scored time; chasing them buys ~0.0003 of
// multiplier while multiplying order rate."
func TestDebounceHoldsTheChase(t *testing.T) {
	p := cfg.Default()

	in := at(49, 50)
	in.TouchHeld = 100 * time.Millisecond
	got := Decide(in, p)
	if got.Move {
		t.Errorf("a 100ms-old touch was chased: 52.6%% of all moves are " +
			"bursts under 100ms and they are 0.1%% of scored time")
	}
	if got.Block != BlockDebounce {
		t.Errorf("Block = %s, want %s", got.Block, BlockDebounce)
	}

	in.TouchHeld = p.Debounce
	if got := Decide(in, p); !got.Move {
		t.Errorf("a touch that has held for exactly debounce_s (%v) was not "+
			"chased: H-Q-7's threshold is inclusive", p.Debounce)
	}
}

// TestStrandedDebounceUsesItsOwnClock is the flicker defeat.
//
// The stranded brake must not be debounced against the age of the current touch
// PRICE. A touch that moves every 200ms resets that clock forever, so an order
// left twenty ticks inside a churning market -- precisely the market that
// stranded it -- would never snap back. The condition's own age is the right
// measurement, and it is a different number.
func TestStrandedDebounceUsesItsOwnClock(t *testing.T) {
	p := cfg.Default()

	in := at(60, 40)
	in.TouchHeld = 10 * time.Millisecond // the touch is flickering
	in.StrandedFor = time.Minute         // but we have been stranded for a minute

	got := Decide(in, p)
	if !got.Move || got.Trigger != TriggerStranded {
		t.Fatalf("Move = %v (block %s): the brake was debounced against the "+
			"touch price's age instead of the stranded condition's age, so a "+
			"market that moves every 200ms defeats the one rule written to "+
			"rescue an order nobody is trading against", got.Move, got.Block)
	}

	// And a genuinely new gap still waits.
	in.StrandedFor = 10 * time.Millisecond
	if got := Decide(in, p); got.Move {
		t.Error("a 10ms-old gap snapped immediately: the brake has its own " +
			"clock, not no clock")
	}
}

// TestPresenceRestorationIsNotDebounced pins the case with nothing resting.
//
// H-Q-7 exists to stop us CHASING a touch that has not settled. With nothing on
// this side there is no order to chase with, and the thing being weighed is a
// presence gap -- which is revenue (S4). Waiting 250ms to open one costs more
// than the write it saves.
func TestPresenceRestorationIsNotDebounced(t *testing.T) {
	p := cfg.Default()

	in := at(0, 50)
	in.HasOurs = false
	in.TouchHeld = time.Millisecond

	got := Decide(in, p)
	if !got.Move {
		t.Fatalf("nothing was placed into an empty side (block %s)", got.Block)
	}
	if got.Trigger != TriggerAbsent {
		t.Errorf("Trigger = %s, want %s: this is §6.6's P2 presence "+
			"restoration, not a requote", got.Trigger, TriggerAbsent)
	}
	if got.Kind != KindPlace {
		t.Errorf("Kind = %s, want %s: there is nothing to cancel",
			got.Kind, KindPlace)
	}
	if got.Price != 50 {
		t.Errorf("Price = %dc, want the touch at 50c", got.Price)
	}
}

// TestNoTouchNoDecision is H-Q-10 reaching into §6.5.
//
// A book that is entirely ours has no external touch. Moving to "the touch"
// there means moving to our own price, which is the self-chase; and a caller
// that read a zero-valued price as a decision would send a 0c order.
func TestNoTouchNoDecision(t *testing.T) {
	p := cfg.Default()

	in := at(50, 0)
	in.Ext = External{Found: false}
	got := Decide(in, p)
	if got.Move {
		t.Errorf("a placement was decided against a book with no external "+
			"touch, at %dc", got.Price)
	}
	if got.Block != BlockNoTouch {
		t.Errorf("Block = %s, want %s", got.Block, BlockNoTouch)
	}
}

// TestSelfCrossClampsOnlyForAReducer is H-CO-6 inside the requote path, and the
// HQL-003 regression.
//
// The touch is where H-Q-1 wants us and H-CO-6 says we may not go there. The
// resolution is ASYMMETRIC, and an earlier version got that wrong by applying
// one rule to both sides.
//
// On a REDUCING side: clamp and place. A4's obligation is to have an exit
// RESTING, and an exit a few ticks back is enormously better than no exit. A
// harness that declined to quote whenever the touch crossed its own other leg
// would drop the exit at exactly the prices where the two legs are closest,
// which is where inventory is most likely.
//
// On an ADDING side: place nothing. A4 imposes no obligation there, so the
// clamp buys nothing and costs the difference -- a quote parked several ticks
// behind the touch earns little or nothing at DF = 0.5 while remaining
// perfectly fillable. That is unremunerated inventory risk taken deliberately.
// The old test described the reducing-side rationale while its helper set
// RoleAdding, so it positively blessed the behaviour it was arguing against.
func TestSelfCrossClampsOnlyForAReducer(t *testing.T) {
	p := cfg.Default()

	base := func(role Role) RequoteInput {
		in := at(40, 60)
		in.Role = role
		in.OtherPrice, in.HasOther = 45, true // 60 + 45 = 105 >= 100
		return in
	}

	t.Run("a reducer clamps and places", func(t *testing.T) {
		got := Decide(base(RoleReducing), p)
		if !got.Move {
			t.Fatalf("no move (block %s): the touch is unreachable but a legal "+
				"price below it exists, and dropping the EXIT is the worse "+
				"answer -- A4 requires one resting", got.Block)
		}
		if Crosses(got.Price, 45) {
			t.Fatalf("placed at %dc against our own 45c on the other side: "+
				"%dc >= 100 (H-CO-6, A3)", got.Price, got.Price+45)
		}
		if got.Price != 54 {
			t.Errorf("Price = %dc, want 54c -- the highest non-crossing price "+
				"against a 45c other leg. Anything lower gives up score for "+
				"nothing (one tick back halves it at DF = 0.5)", got.Price)
		}
		if !got.Clamped {
			t.Error("the clamp was not reported: a book whose two touches " +
				"cross our own pair is a state the operator should see")
		}
	})

	t.Run("an adding side places nothing", func(t *testing.T) {
		for _, role := range []Role{RoleAdding} {
			got := Decide(base(role), p)
			if got.Move {
				t.Fatalf("a %s side placed at %dc, six ticks behind a 60c "+
					"touch. That order is fillable and barely scores: when "+
					"the field falls to 54 it takes 12 contracts of "+
					"inventory we were not paid to hold. A4 imposes no "+
					"obligation on an adding side, so there is nothing to "+
					"weigh against that", role, got.Price)
			}
			if got.Block != BlockSelfCross {
				t.Errorf("Block = %s, want %s", got.Block, BlockSelfCross)
			}
			if got.Clamped {
				t.Error("Clamped was set on a side that placed nothing")
			}
		}
	})

	t.Run("no placeable price at all", func(t *testing.T) {
		// Even the exit gives up when the other leg is at 99c: every bid from
		// 1c up sums to at least 100.
		in := base(RoleReducing)
		in.OtherPrice = 99
		if got := Decide(in, p); got.Move {
			t.Errorf("placed at %dc against a 99c other leg", got.Price)
		} else if got.Block != BlockSelfCross {
			t.Errorf("Block = %s, want %s", got.Block, BlockSelfCross)
		}
	})

	t.Run("an unobstructed touch is unaffected", func(t *testing.T) {
		// The clamp must not fire when it is not needed, on either side.
		for _, role := range []Role{RoleAdding, RoleReducing} {
			in := at(40, 50)
			in.Role = role
			in.OtherPrice, in.HasOther = 30, true // 50 + 30 = 80 < 100
			got := Decide(in, p)
			if !got.Move || got.Price != 50 || got.Clamped {
				t.Errorf("%s side: Move %v at %dc (clamped %v), want a clean "+
					"move to the 50c touch", role, got.Move, got.Price, got.Clamped)
			}
		}
	})
}

// TestRequoteKindIsH_Q_9sThreeConditions walks H-Q-9 and H-Q-9a.
//
// Place-then-cancel is permitted only on an upward requote, on the adding side,
// within the momentary aggregate cap, and without self-crossing. Every other
// case is cancel-confirm-place, where the replacement is not dispatched until
// the cancel is confirmed by response or sweep.
//
// The reducing-side clause is HR-004: two individually-|q|-capped reducers that
// both fill flip the position, so a momentary overlap on that side is exactly
// the overshoot H-Q-5a forbids. The presence gap is accepted -- a reducer that
// overshoots is worse than a reducer that is briefly absent.
func TestRequoteKindIsH_Q_9sThreeConditions(t *testing.T) {
	p := cfg.Default()

	base := func() RequoteInput {
		in := at(49, 50) // an upward requote
		in.AllowPlaceThenCancel = true
		return in
	}

	t.Run("adding side, upward, funded", func(t *testing.T) {
		got := Decide(base(), p)
		if got.Kind != KindPlaceThenCancel {
			t.Errorf("Kind = %s, want %s: presence gaps are revenue (S4), so "+
				"the ordering is not cosmetic", got.Kind, KindPlaceThenCancel)
		}
	})

	t.Run("reducing side is never place-then-cancel", func(t *testing.T) {
		in := base()
		in.Role = RoleReducing
		if got := Decide(in, p); got.Kind != KindCancelConfirmPlace {
			t.Errorf("Kind = %s, want %s: on a reducing side the momentary "+
				"aggregate would exceed |q|, and two individually-capped "+
				"reducers that both fill flip the position (HR-004)",
				got.Kind, KindCancelConfirmPlace)
		}
	})

	t.Run("a downward move is never place-then-cancel", func(t *testing.T) {
		in := base()
		in.OurPrice, in.Ext.Price = 60, 40 // the stranded brake, moving down
		if got := Decide(in, p); got.Kind != KindCancelConfirmPlace {
			t.Errorf("Kind = %s, want %s: placing the lower order first "+
				"leaves the higher one live, so it remains the touch we are "+
				"trying to leave and the brake is defeated for as long as the "+
				"cancel takes", got.Kind, KindCancelConfirmPlace)
		}
	})

	t.Run("no headroom for the momentary overlap", func(t *testing.T) {
		in := base()
		in.Headroom = in.Size - 1
		if got := Decide(in, p); got.Kind != KindCancelConfirmPlace {
			t.Errorf("Kind = %s, want %s: H-Q-9's second condition is that "+
				"the MOMENTARY aggregate is within S_max and the capital cap, "+
				"and both orders are live at once",
				got.Kind, KindCancelConfirmPlace)
		}
	})

	t.Run("the pilot profile disables it entirely", func(t *testing.T) {
		in := base()
		in.AllowPlaceThenCancel = false
		if got := Decide(in, p); got.Kind != KindCancelConfirmPlace {
			t.Errorf("Kind = %s, want %s: pilot-plan §2.7 and §7.1 use "+
				"cancel-confirm-place everywhere and pay the presence gap",
				got.Kind, KindCancelConfirmPlace)
		}
	})
}

// TestDecideNeverEmitsAnUntradablePrice sweeps the whole price domain.
//
// Every touch, every one of our own prices, both roles, with and without an
// other-side leg. Whatever comes back must be placeable and must not cross.
func TestDecideNeverEmitsAnUntradablePrice(t *testing.T) {
	p := cfg.Default()

	var moved int
	for touch := MinPrice; touch <= MaxPrice; touch++ {
		for our := MinPrice; our <= MaxPrice; our++ {
			for _, other := range []int{0, 10, 45, 60, 99} {
				in := at(our, touch)
				in.HasOther = other != 0
				in.OtherPrice = other

				got := Decide(in, p)
				if !got.Move {
					continue
				}
				moved++

				// HQL-003: an adding side never places behind the touch to
				// escape a self-cross. If it moved at all, it moved to the
				// touch itself.
				if in.Role == RoleAdding && got.Clamped {
					t.Fatalf("an adding side reported a clamp at %dc against "+
						"a touch of %dc", got.Price, touch)
				}

				if !ValidPrice(got.Price) {
					t.Fatalf("Decide(our=%dc, touch=%dc, other=%dc) placed at "+
						"%dc, outside %d..%d",
						our, touch, other, got.Price, MinPrice, MaxPrice)
				}
				if in.HasOther && Crosses(got.Price, other) {
					t.Fatalf("Decide(our=%dc, touch=%dc, other=%dc) placed at "+
						"%dc, summing to %dc (H-CO-6, A3)",
						our, touch, other, got.Price, got.Price+other)
				}
				// H-Q-2: never inside the touch.
				if got.Price > touch {
					t.Fatalf("Decide(our=%dc, touch=%dc) placed at %dc, "+
						"INSIDE the external touch: H-Q-2 declines a real "+
						"scoring gain because the adverse selection costs more",
						our, touch, got.Price)
				}
			}
		}
	}
	if moved == 0 {
		t.Fatal("the sweep never produced a move")
	}
}

// TestDecideIsIdempotentAtRest: once the move is made, the same input decides
// nothing further.
//
// A requote rule that still wants to move after moving is an infinite write
// loop against §6.6's budget, and it would be P3 -- the class §6.6 permits to
// starve, so it would consume the whole bounded allowance and be invisible in
// the P0/P1 metrics.
func TestDecideIsIdempotentAtRest(t *testing.T) {
	p := cfg.Default()

	for touch := MinPrice; touch <= MaxPrice; touch++ {
		for _, other := range []int{0, 45, 90} {
			in := at(40, touch)
			in.HasOther = other != 0
			in.OtherPrice = other

			first := Decide(in, p)
			if !first.Move {
				continue
			}
			in.OurPrice = first.Price
			if second := Decide(in, p); second.Move {
				t.Fatalf("after moving to %dc against a touch of %dc, Decide "+
					"wanted to move again to %dc (trigger %s)",
					first.Price, touch, second.Price, second.Trigger)
			}
		}
	}
}
