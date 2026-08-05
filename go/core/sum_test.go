package core

import (
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"testing"
)

// P24. CPython's sum() is not naive addition over floats — it carries a
// Neumaier compensation term. The fixture pins what CPython actually produced;
// a naive Go loop fails it.

func TestP24_SumMatchesCPythonCompensatedSummation(t *testing.T) {
	rows := fixture(t, "sums.tsv")
	for _, r := range rows {
		l := NewLevels()
		if r[0] != "" {
			for i, s := range strings.Split(r[0], ",") {
				v, err := strconv.ParseFloat(s, 64)
				if err != nil {
					t.Fatalf("bad fixture value %q: %v", s, err)
				}
				l.Set(i, v) // distinct keys, inserted in fixture order
			}
		}
		raw, err := hex.DecodeString(r[1])
		if err != nil || len(raw) != 8 {
			t.Fatalf("bad fixture row %v", r)
		}
		var want uint64
		for _, b := range raw {
			want = want<<8 | uint64(b)
		}
		if got := math.Float64bits(l.Sum()); got != want {
			t.Fatalf("Sum over %d values = %016x, CPython sum() = %016x\n  values: %.80s",
				l.Len(), got, want, r[0])
		}
	}
	t.Logf("%d summation cases match CPython exactly", len(rows))
}

// Guards the fixture's discriminating power, as for the rounding fixture: if
// naive summation would pass every case, the fixture cannot detect P24.
func TestP24_FixtureDiscriminatesNaiveSummation(t *testing.T) {
	rows := fixture(t, "sums.tsv")
	n := 0
	for _, r := range rows {
		if r[0] == "" {
			continue
		}
		var naive, comp, c float64
		for _, s := range strings.Split(r[0], ",") {
			v, _ := strconv.ParseFloat(s, 64)
			naive += v
			tt := comp + v
			if math.Abs(comp) >= math.Abs(v) {
				c += (comp - tt) + v
			} else {
				c += (v - tt) + comp
			}
			comp = tt
		}
		if naive != comp+c {
			n++
		}
	}
	if n == 0 {
		t.Fatal("fixture contains no case where naive summation differs from " +
			"compensated; it cannot detect the P24 hazard")
	}
	t.Logf("%d of %d summation cases expose the naive-summation hazard", n, len(rows))
}
