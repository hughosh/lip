package quote

import (
	"testing"

	"lip/core"
	"lip/harness/num"
)

// book builds a level set from (cents, size) pairs.
func book(t *testing.T, pairs ...[2]float64) *core.Levels {
	t.Helper()
	lv := core.NewLevels()
	for _, p := range pairs {
		lv.Set(int(p[0]), p[1])
	}
	return lv
}

// TestExternalBestSubtractsOurOwnSize is the M9 self-chase test.
//
// M9: "Use the raw book instead of external_best", caught by "a self-chase
// test: our own order must never move the quote."
//
// The mutation is one character wide -- read lv.Max() instead of subtracting --
// and it is invisible in every case where somebody else is also at the touch,
// which is most of them. It becomes visible in exactly one situation, which is
// also the situation that costs money: when we are the last bid standing at a
// price everyone else has left. The raw book says the touch is our own price,
// so H-Q-6 concludes we are not behind, no requote is issued, and H-Q-8's
// stranded brake never fires either -- because the touch it measures staleness
// against is our own order. We hold a price nobody wants, indefinitely, and the
// two rules written to prevent that both read as satisfied.
func TestExternalBestSubtractsOurOwnSize(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lv        *core.Levels
		ours      []Resting
		wantPrice int
		wantSize  float64
		wantFound bool
		why       string
	}{
		{
			name:      "we are the last bid standing",
			lv:        book(t, [2]float64{50, 100}, [2]float64{49, 200}),
			ours:      []Resting{{Price: 50, Size: qty(100)}},
			wantPrice: 49, wantSize: 200, wantFound: true,
			why: "the whole of 50 is ours, so the external touch is 49. The " +
				"raw book would answer 50 -- our own order -- and we would " +
				"hold a price the field has left, with H-Q-6 seeing nothing " +
				"to chase and H-Q-8 measuring staleness against ourselves",
		},
		{
			name:      "we are at the touch alongside a real field",
			lv:        book(t, [2]float64{50, 100}, [2]float64{49, 200}),
			ours:      []Resting{{Price: 50, Size: qty(40)}},
			wantPrice: 50, wantSize: 60, wantFound: true,
			why: "60 contracts at 50 are somebody else's, so the touch is " +
				"still 50 and we are correctly at it. This is the case the " +
				"mutation gets RIGHT, which is why it survives casual testing",
		},
		{
			name:      "subtraction is only at our own price",
			lv:        book(t, [2]float64{50, 100}, [2]float64{49, 200}),
			ours:      []Resting{{Price: 49, Size: qty(200)}},
			wantPrice: 50, wantSize: 100, wantFound: true,
			why: "our size at 49 must not be taken out of 50 -- that would " +
				"delete somebody else's real liquidity",
		},
		{
			name:      "our aggregate spans two prices mid-requote",
			lv:        book(t, [2]float64{51, 12}, [2]float64{50, 12}, [2]float64{48, 5}),
			ours:      []Resting{{Price: 51, Size: qty(12)}, {Price: 50, Size: qty(12)}},
			wantPrice: 48, wantSize: 5, wantFound: true,
			why: "H-Q-9's place-then-cancel rests us at TWO prices at once. " +
				"Subtracting only one leaves the other in the touch, which is " +
				"the self-chase reintroduced through the requote path",
		},
		{
			name:      "two orders of ours at one price",
			lv:        book(t, [2]float64{50, 20}, [2]float64{49, 7}),
			ours:      []Resting{{Price: 50, Size: qty(12)}, {Price: 50, Size: qty(8)}},
			wantPrice: 49, wantSize: 7, wantFound: true,
			why: "the aggregate is the sum (H-Q-5b), not the largest entry",
		},
		{
			name:      "an unconfirmed cancel is still ours",
			lv:        book(t, [2]float64{50, 12}, [2]float64{47, 3}),
			ours:      []Resting{{Price: 50, Size: qty(12)}},
			wantPrice: 47, wantSize: 3, wantFound: true,
			why: "cancel-requested is not cancelled (H-FAIL-3). The order is " +
				"still in the book, so it is still ours to subtract",
		},
		{
			name:      "the book is entirely ours",
			lv:        book(t, [2]float64{50, 12}),
			ours:      []Resting{{Price: 50, Size: qty(12)}},
			wantFound: false,
			why: "nobody else is bidding. There is no external touch, and " +
				"that is a different answer from 'the touch is 50'",
		},
		{
			name:      "an empty book",
			lv:        book(t),
			ours:      nil,
			wantFound: false,
			why:       "nothing to read",
		},
		{
			name:      "nothing of ours resting",
			lv:        book(t, [2]float64{50, 100}),
			ours:      nil,
			wantPrice: 50, wantSize: 100, wantFound: true,
			why: "with no order of ours the external book IS the book -- " +
				"subtracting anything here would remove a stranger's size",
		},
		{
			name:      "a zero-size entry of ours changes nothing",
			lv:        book(t, [2]float64{50, 100}),
			ours:      []Resting{{Price: 50, Size: 0}},
			wantPrice: 50, wantSize: 100, wantFound: true,
			why: "an order of zero size is not in the book",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ExternalBest(tc.lv, tc.ours)
			if got.Found != tc.wantFound {
				t.Fatalf("Found = %v, want %v -- %s", got.Found, tc.wantFound, tc.why)
			}
			if !tc.wantFound {
				return
			}
			if got.Price != tc.wantPrice {
				t.Fatalf("external touch = %dc, want %dc -- %s",
					got.Price, tc.wantPrice, tc.why)
			}
			if got.Size != tc.wantSize {
				t.Errorf("external size at %dc = %v, want %v -- %s",
					got.Price, got.Size, tc.wantSize, tc.why)
			}
		})
	}
}

// TestExternalBestClearsExactlyOwnedLevels is the HR-026 class one layer out.
//
// Book sizes are float64 and our size is exact, so a level that is entirely
// ours does not subtract to a clean zero -- it subtracts to a binary residue.
// A `> 0` test would then find a level with 1e-16 contracts on it, report it as
// the external touch, and hand back our own price as somebody else's. That is
// M9's observable produced by a rounding rule rather than by a missing
// subtraction, and no amount of care in ExternalBest's structure prevents it.
func TestExternalBestClearsExactlyOwnedLevels(t *testing.T) {
	// Sizes whose decimal representation is not exact in binary, accumulated
	// the way a partially-filled order arrives.
	for _, size := range []float64{0.1, 0.3, 0.07, 12.34, 0.1 + 0.2} {
		lv := core.NewLevels()
		lv.Set(50, size)
		lv.Set(49, 5)

		ours := []Resting{{Price: 50, Size: num.QtyFromFloat(size)}}
		got := ExternalBest(lv, ours)

		if !got.Found || got.Price != 49 {
			t.Errorf("with a level of %v entirely ours, external touch = "+
				"%dc (found %v), want 49c: the residue of subtracting an "+
				"exact quantity from a float level must not read as somebody "+
				"else's liquidity", size, got.Price, got.Found)
		}
	}

	// And the converse: one genuine quantum beyond our size is a real level and
	// must survive. This is the bound that makes the epsilon safe -- it can
	// never be raised past the exchange's own quantum.
	lv := core.NewLevels()
	lv.Set(50, 12.01)
	lv.Set(49, 5)
	got := ExternalBest(lv, []Resting{{Price: 50, Size: qty(12)}})
	if !got.Found || got.Price != 50 {
		t.Fatalf("external touch = %dc (found %v), want 50c: one quantum "+
			"beyond our own size is the smallest real quantity the exchange "+
			"can express, and discarding it would abandon a live touch",
			got.Price, got.Found)
	}
}

// TestExternalBestClampsOverSubtraction pins the race that would otherwise
// delete a stranger's liquidity.
//
// Our order fills and the exchange removes it from the book. The delta can
// reach us before the fill report does, so for a few milliseconds our model
// claims size the book no longer shows. Subtracting it anyway takes the
// difference out of whoever is left at that price -- `probebot.py` records
// exactly this hazard for its dry-run branch, where subtracting "would delete
// somebody else's real liquidity."
func TestExternalBestClampsOverSubtraction(t *testing.T) {
	lv := core.NewLevels()
	lv.Set(50, 5) // we think we have 12 here; the book shows 5
	lv.Set(49, 3)

	got := ExternalBest(lv, []Resting{{Price: 50, Size: qty(12)}})

	if !got.OverSubtracted {
		t.Error("a claim larger than the level went unreported: sustained, " +
			"this means the order model and the book disagree, and only the " +
			"caller holds the clock that can tell a race from a defect")
	}
	if !got.Found || got.Price != 49 {
		t.Fatalf("external touch = %dc (found %v), want 49c",
			got.Price, got.Found)
	}
	if got.Size != 3 {
		t.Errorf("external size = %v, want 3: the excess must be clamped at "+
			"the level, never carried down into the next one", got.Size)
	}
}

// TestBehindByHasNoAnswerWithoutATouch pins the distinction H-Q-6 and H-Q-8
// both depend on.
//
// Both rules are stated in ticks behind the external touch. When the book is
// entirely ours there is no external touch, and "0 ticks behind" is the one
// answer that must not be returned: it reads as "we are exactly at the touch",
// which is the self-chase conclusion, reached this time through the return
// convention rather than through the subtraction.
func TestBehindByHasNoAnswerWithoutATouch(t *testing.T) {
	entirelyOurs := ExternalBest(
		book(t, [2]float64{50, 12}), []Resting{{Price: 50, Size: qty(12)}})

	if _, ok := BehindBy(50, entirelyOurs); ok {
		t.Fatal("BehindBy answered against a book with no external touch: " +
			"a caller reading that as 0 concludes we are at a touch that " +
			"does not exist, and never requotes")
	}

	ext := ExternalBest(book(t, [2]float64{50, 100}), nil)
	for _, tc := range []struct {
		our  int
		want int
		why  string
	}{
		{42, 8, "eight ticks behind a 50c touch -- H-Q-8's stranded brake " +
			"fires at stale_bid_ticks = 8"},
		{50, 0, "at the touch, which is where H-Q-1 wants us"},
		{51, -1, "improving the touch: H-Q-2 forbids placing here, and the " +
			"sign is what tells the caller which rule applies"},
	} {
		got, ok := BehindBy(tc.our, ext)
		if !ok {
			t.Fatalf("BehindBy(%d) had no answer against a live touch", tc.our)
		}
		if got != tc.want {
			t.Errorf("BehindBy(%d) = %d, want %d -- %s",
				tc.our, got, tc.want, tc.why)
		}
	}
}
