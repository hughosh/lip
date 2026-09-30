package rest

import (
	"math"
	"strings"
	"testing"

	"lip/harness/num"
)

func TestCompareBooksSizePrecisionAtDepth(t *testing.T) {
	no := SideBooks{WS: []BookLevel{lv(40, 600)}, REST: []BookLevel{lv(40, 600)}}
	base := func(ws, remote float64) BookCheck {
		return BookCheck{Target: 500, Yes: SideBooks{
			WS:   []BookLevel{lv(24, 100), lv(23, ws)},
			REST: []BookLevel{lv(24, 100), lv(23, remote)},
		}, No: no}
	}
	for _, tc := range []struct {
		name  string
		check BookCheck
		agree bool
		why   string
	}{
		{"observed depth-two roundoff", base(456.00000000000006, 456), true, ""},
		// lip-2w3 pinned this as a mismatch; candidate-5 then raised 4- and
		// 2-step residue in production (lip-2mz). Residue is not a step count.
		{"two rounding steps are the same book size", base(math.Nextafter(456.00000000000006, math.Inf(1)), 456), true, ""},
		{"real one-cent size difference", base(456.01, 456), false, "disagrees at depth 2"},
		{"boundary reached exactly despite rounded level", BookCheck{Target: 556,
			Yes: SideBooks{WS: []BookLevel{lv(24, 100), lv(23, 456.00000000000006)}, REST: []BookLevel{lv(24, 100), lv(23, 456)}}, No: no}, true, ""},
		{"target walk does not round an undershoot into qualification", BookCheck{Target: math.Nextafter(456, math.Inf(1)),
			Yes: SideBooks{WS: []BookLevel{lv(23, 456)}, REST: []BookLevel{lv(23, 456)}}, No: no}, false, "does not reach Target Size"},
		{"invalid infinity is not agreement", base(math.Inf(1), math.Inf(1)), false, ""},
		{"invalid NaN is not agreement", base(math.NaN(), math.NaN()), false, ""},
		{"invalid negative size is not agreement", base(-1, -1), false, ""},
		// Past the grid's int64 range two DIFFERENT sizes could convert to the
		// same Qty, so no size that large is a book size at all.
		{"sizes beyond the grid's range are not agreement", base(1e300, 1e300), false, "does not reach Target Size"},
		{"different oversized sizes cannot meet on the grid", base(1e300, 1e299), false, "does not reach Target Size"},
		// From 2^45 contracts a double cannot keep adjacent quanta apart; both
		// of these become 3518437208883203 quanta.
		{"a quantum apart where doubles no longer hold the grid", base(35184372088832.02, 35184372088832.03), false, "does not reach Target Size"},
		{"a quantum apart just below maxBookSize", base(9999999999999.98, 9999999999999.99), false, "disagrees at depth 2"},
		// Residue that outlived ApplyDelta's 1e-9 deletion at a price the
		// exchange has emptied: the live book has a level REST does not.
		{"a residue-only websocket level is named by its price", BookCheck{Target: 25,
			Yes: SideBooks{WS: []BookLevel{lv(51, 4e-9), lv(50, 30)}, REST: []BookLevel{lv(50, 30)}}, No: no}, false, "the websocket says 51c and REST says 50c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareBooks(tc.check)
			if got.Agree != tc.agree || (tc.why != "" && !strings.Contains(got.Why, tc.why)) {
				t.Fatalf("CompareBooks agree=%v why=%q; want agree=%v why containing %q", got.Agree, got.Why, tc.agree, tc.why)
			}
		})
	}
}

func TestCompareBooksOwnedSizePrecision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  float64
		agree bool
	}{
		{"one representable step below own size", math.Nextafter(456, math.Inf(-1)), true},
		{"real one-cent undershoot", 455.99, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			side := SideBooks{WS: []BookLevel{lv(23, tc.size)}, REST: []BookLevel{lv(23, tc.size)},
				Ours: []OwnResting{{Cents: 23, Size: num.QtyFromFloat(456)}}}
			got := CompareBooks(BookCheck{Target: 400, Yes: side,
				No: SideBooks{WS: []BookLevel{lv(40, 500)}, REST: []BookLevel{lv(40, 500)}}})
			if got.Agree != tc.agree {
				t.Fatalf("CompareBooks agree=%v why=%q; want %v", got.Agree, got.Why, tc.agree)
			}
		})
	}
}

// TestCompareBooksObservedResidueAgrees is lip-2mz.
//
// The websocket book adds every delta into a float64 level
// (core.Book.ApplyDelta, frozen with go/core), so a level's residue grows with
// the number of deltas it has absorbed and no step count bounds it. lip-2w3
// forgave one adjacent step; candidate-5 then raised two
// BOOK_CROSSCHECK_MISMATCH anomalies in run 20260929T2055382WVM5 (a29: 4
// steps, a36: 2 steps), and each one quarantined the market and stopped adding
// for the life of the process.
//
// Each case puts the observed pair at its observed side, price and depth under
// the market's observed Target Size of 1000. Only that level is known at the
// time of the mismatch; the other levels are the stage's 20:55:28Z REST read
// (evidence/market-book-series.json) and are there to put it at that depth.
func TestCompareBooksObservedResidueAgrees(t *testing.T) {
	yes := func(at28 float64) []BookLevel {
		return []BookLevel{lv(33, 173.83), lv(32, 251.87), lv(31, 50.72), lv(30, 142.49),
			lv(29, 250), lv(28, at28), lv(27, 225), lv(26, 27)}
	}
	no := func(at64 float64) []BookLevel {
		return []BookLevel{lv(65, 28.92), lv(64, at64), lv(63, 1355), lv(60, 200),
			lv(56, 49), lv(52, 103.70), lv(51, 1246.62), lv(50, 872)}
	}
	a29 := func(ws float64) BookCheck {
		return BookCheck{Target: 1000,
			Yes: SideBooks{WS: yes(ws), REST: yes(30)},
			No:  SideBooks{WS: no(103.63), REST: no(103.63)}}
	}
	a36 := func(ws float64) BookCheck {
		return BookCheck{Target: 1000,
			Yes: SideBooks{WS: yes(56.48), REST: yes(56.48)},
			No:  SideBooks{WS: no(ws), REST: no(90.85)}}
	}
	for _, tc := range []struct {
		name  string
		check BookCheck
		agree bool
		why   string
	}{
		{"a29: YES 28c at depth 6 is 29.999999999999986 against 30", a29(29.999999999999986), true, ""},
		{"a36: NO 64c at depth 2 is 90.85000000000002 against 90.85", a36(90.85000000000002), true, ""},
		{"residue far beyond any observed step count", a29(30 - 1e-7), true, ""},
		{"a29's level one quantum short", a29(29.99), false, "the yes side disagrees at depth 6"},
		{"a36's level one quantum long", a36(90.86), false, "the no side disagrees at depth 2"},
		{"nearer the next quantum is the next quantum", a29(30.006), false, "the yes side disagrees at depth 6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareBooks(tc.check)
			if got.Agree != tc.agree || (tc.why != "" && !strings.Contains(got.Why, tc.why)) {
				t.Fatalf("CompareBooks agree=%v why=%q; want agree=%v why containing %q", got.Agree, got.Why, tc.agree, tc.why)
			}
		})
	}
}

// TestCompareBooksTargetBoundaryResidue is where residue meets the Target.
//
// A level carrying residue can leave the websocket's running total just under
// the Target where REST's is exactly on it. Walked in float64 the two books then
// reach the Target at different depths -- or the websocket's walk never does --
// although both state the same resting liquidity. The walk is on the grid for
// that reason, and a real quantum short of the Target is still disagreement.
func TestCompareBooksTargetBoundaryResidue(t *testing.T) {
	check := func(ws, remote []BookLevel) BookCheck {
		return BookCheck{Target: 100, Yes: SideBooks{WS: ws, REST: remote},
			No: SideBooks{WS: []BookLevel{lv(40, 600)}, REST: []BookLevel{lv(40, 600)}}}
	}
	for _, tc := range []struct {
		name  string
		check BookCheck
		agree bool
		why   string
	}{
		{"residue on the level that reaches the Target",
			check([]BookLevel{lv(50, 70), lv(49, 29.999999999999986), lv(48, 5)},
				[]BookLevel{lv(50, 70), lv(49, 30), lv(48, 5)}), true, ""},
		{"residue that leaves the websocket's float total short of the Target",
			check([]BookLevel{lv(50, 70), lv(49, 29.999999999999986)},
				[]BookLevel{lv(50, 70), lv(49, 30)}), true, ""},
		{"a quantum short of the Target reaches it a level deeper",
			check([]BookLevel{lv(50, 70), lv(49, 29.99), lv(48, 5)},
				[]BookLevel{lv(50, 70), lv(49, 30), lv(48, 5)}), false, "disagrees at depth 2"},
		{"a quantum short of the Target with nothing deeper",
			check([]BookLevel{lv(50, 70), lv(49, 29.99)},
				[]BookLevel{lv(50, 70), lv(49, 30)}), false, "does not reach Target Size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareBooks(tc.check)
			if got.Agree != tc.agree || (tc.why != "" && !strings.Contains(got.Why, tc.why)) {
				t.Fatalf("CompareBooks agree=%v why=%q; want agree=%v why containing %q", got.Agree, got.Why, tc.agree, tc.why)
			}
		})
	}
}

// TestCompareBooksOwnedSizeResidue is lip-2mz for H-FAIL-6's second clause.
// Our 30 rests at 23c, deeper than a walk the first level completes, so only
// the own-resting check reads that level.
func TestCompareBooksOwnedSizeResidue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ws, remote float64
		agree      bool
		why        string
	}{
		{"a29's residue under our own size on the websocket", 29.999999999999986, 30, true, ""},
		{"the same residue on both books", 29.999999999999986, 29.999999999999986, true, ""},
		{"the websocket shows a quantum less than ours", 29.99, 30, false, "websocket book shows only"},
		{"REST shows a quantum less than ours", 30, 29.99, false, "REST book shows only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareBooks(BookCheck{Target: 400,
				Yes: SideBooks{WS: []BookLevel{lv(24, 500), lv(23, tc.ws)},
					REST: []BookLevel{lv(24, 500), lv(23, tc.remote)},
					Ours: []OwnResting{{Cents: 23, Size: num.QtyFromFloat(30)}}},
				No: SideBooks{WS: []BookLevel{lv(40, 500)}, REST: []BookLevel{lv(40, 500)}}})
			if got.Agree != tc.agree || (tc.why != "" && !strings.Contains(got.Why, tc.why)) {
				t.Fatalf("CompareBooks agree=%v why=%q; want agree=%v why containing %q", got.Agree, got.Why, tc.agree, tc.why)
			}
		})
	}
}
