package main

import "testing"

// Python's `int(time.time() * 1000)` truncates a FLOAT64 of seconds, not the
// exact nanosecond count, so it rounds across a millisecond boundary in a
// meaningful fraction of cases. Exact integer division is therefore wrong here
// even though it is more accurate.
//
// The construction matters twice over. CPython forms ONE int64 nanosecond value
// and converts that to double (_PyTime_AsSecondsDouble); it does not convert
// seconds and nanoseconds separately and add them. The naive split disagrees in
// BOTH directions, which the first two cases below pin — an earlier version of
// this fixture used only nsec=999809, where all three formulas happen to agree,
// and so proved nothing about the one that matters.
//
// P23: every expected value is a literal, never a Go expression. Go folds
// untyped constant arithmetic at arbitrary precision, so computing one here
// would produce the mathematically correct answer rather than the float64 one.
func TestPyTimeMsMatchesCPython(t *testing.T) {
	cases := []struct {
		sec  int64
		nsec int
		want int64
	}{
		{1784934732, 999808, 1784934732001},    // split formula gives ...000
		{1784934732, 1999975, 1784934732001},   // split formula gives ...002
		{1784934732, 999809, 1784934732001},    // all formulas agree
		{1784934732, 0, 1784934732000},         // whole-second fast path
		{1784934732, 1, 1784934732000},
		{1784934732, 999999999, 1784934733000}, // rolls into the next SECOND
		{1784934732, 500000000, 1784934732500},
		{1900000000, 999999999, 1900000001000},
		{2000000000, 999999999, 2000000001000},
		{1784938812, 123456789, 1784938812123},
	}

	vsExact, vsSplit := 0, 0
	for _, c := range cases {
		if got := pyTimeMs(c.sec, c.nsec); got != c.want {
			t.Errorf("pyTimeMs(%d, %d) = %d, want %d (CPython)", c.sec, c.nsec, got, c.want)
		}
		total := c.sec*1_000_000_000 + int64(c.nsec)
		if total/1_000_000 != c.want {
			vsExact++
		}
		// The formula this fixture exists to rule out.
		if int64((float64(c.sec)+float64(c.nsec)/1e9)*1000) != c.want {
			vsSplit++
		}
	}
	// Discriminating power, as the rounding and summation fixtures carry. If
	// either wrong formula agreed everywhere, this test could not detect it.
	if vsExact == 0 {
		t.Fatal("no case distinguishes CPython's float conversion from exact " +
			"integer division")
	}
	if vsSplit == 0 {
		t.Fatal("no case distinguishes CPython's single-int64 conversion from " +
			"sec + nsec/1e9; this is exactly the hole the first fixture had")
	}
	t.Logf("%d cases expose exact-division, %d expose the split-conversion hazard",
		vsExact, vsSplit)
}

// The whole-second fast path is CPython's, and is not merely an optimisation:
// (double)(t/1e9) and (double)t/1e9 need not agree.
func TestPyTimeSecondsWholeSecondFastPath(t *testing.T) {
	if got := pyTimeSeconds(1784934732 * 1_000_000_000); got != 1784934732.0 {
		t.Errorf("pyTimeSeconds(whole second) = %v, want 1784934732", got)
	}
}
