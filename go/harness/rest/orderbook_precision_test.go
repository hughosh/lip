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
		{"two rounding steps remain a mismatch", base(math.Nextafter(456.00000000000006, math.Inf(1)), 456), false, "disagrees at depth 2"},
		{"real one-cent size difference", base(456.01, 456), false, "disagrees at depth 2"},
		{"boundary reached exactly despite rounded level", BookCheck{Target: 556,
			Yes: SideBooks{WS: []BookLevel{lv(24, 100), lv(23, 456.00000000000006)}, REST: []BookLevel{lv(24, 100), lv(23, 456)}}, No: no}, true, ""},
		{"target walk does not round an undershoot into qualification", BookCheck{Target: math.Nextafter(456, math.Inf(1)),
			Yes: SideBooks{WS: []BookLevel{lv(23, 456)}, REST: []BookLevel{lv(23, 456)}}, No: no}, false, "does not reach Target Size"},
		{"invalid infinity is not agreement", base(math.Inf(1), math.Inf(1)), false, ""},
		{"invalid NaN is not agreement", base(math.NaN(), math.NaN()), false, ""},
		{"invalid negative size is not agreement", base(-1, -1), false, ""},
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
