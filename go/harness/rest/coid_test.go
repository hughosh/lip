package rest

import (
	"testing"

	"lip/harness/quote"
)

// TestCoidIsDeterministicAndParsable is H-ORD-1.
//
// The format is `lipH-{runID}-{marketIdx:03d}-{side}-{seq:08d}`, and every part
// of it is load-bearing:
//
//   - the `lipH-` prefix is how §7.5's startup adoption distinguishes our
//     orders from anything else on the account;
//   - determinism is what makes H-ORD-2b's same-coid retry possible at all --
//     a retry under a new coid IS a new order, and the exchange cannot report
//     it as a duplicate because it is not one;
//   - the fixed-width fields are what make the round trip unambiguous.
func TestCoidIsDeterministicAndParsable(t *testing.T) {
	const runID = "01JZQK8ZR9"

	got, err := Coid(runID, 3, quote.SideNo, 47)
	if err != nil {
		t.Fatalf("Coid: %v", err)
	}
	if want := "lipH-01JZQK8ZR9-003-no-00000047"; got != want {
		t.Fatalf("Coid = %q, want %q", got, want)
	}

	// Deterministic: same inputs, same coid, every time. This is the property
	// the same-coid retry depends on, and it must survive a process restart --
	// which is why nothing here reads a clock or a random source.
	again, _ := Coid(runID, 3, quote.SideNo, 47)
	if again != got {
		t.Fatalf("Coid is not deterministic: %q then %q", got, again)
	}

	p, ok := ParseCoid(got)
	if !ok {
		t.Fatalf("ParseCoid(%q) did not recognise our own coid", got)
	}
	if p.RunID != runID || p.MarketIdx != 3 || p.Side != quote.SideNo || p.Seq != 47 {
		t.Fatalf("ParseCoid = %+v, want run %q market 3 side no seq 47",
			p, runID)
	}

	// Every field, swept, round-tripped.
	for idx := 0; idx <= 999; idx += 37 {
		for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
			for _, seq := range []uint64{0, 1, 99, 12345678, 99999999} {
				c, err := Coid(runID, idx, side, seq)
				if err != nil {
					t.Fatalf("Coid(%d, %s, %d): %v", idx, side, seq, err)
				}
				p, ok := ParseCoid(c)
				if !ok {
					t.Fatalf("ParseCoid(%q) failed", c)
				}
				if p.MarketIdx != idx || p.Side != side || p.Seq != seq {
					t.Fatalf("%q round-tripped to %+v", c, p)
				}
			}
		}
	}
}

// TestCoidRejectsAmbiguousRunIDs.
//
// The separator is "-", so a run id containing one shifts every field after it.
// ParseCoid is how a reconciliation walk decides which market an order found on
// the account belongs to, and a shifted field attributes somebody's position to
// the wrong ticker -- which then sizes a reducer against a position that market
// does not hold.
func TestCoidRejectsAmbiguousRunIDs(t *testing.T) {
	for _, bad := range []string{"", "a-b", "run id", "run_id", "run.id", "üü"} {
		if _, err := Coid(bad, 0, quote.SideYes, 0); err == nil {
			t.Errorf("Coid accepted the run id %q", bad)
		}
	}
	for _, good := range []string{"01JZQK8ZR9", "abc", "ABC123", "0"} {
		if err := ValidRunID(good); err != nil {
			t.Errorf("ValidRunID(%q) = %v, want nil", good, err)
		}
	}

	// Field overflow: %03d and %08d cannot represent these, and a coid that
	// silently widened a field would no longer round-trip.
	if _, err := Coid("run", 1000, quote.SideYes, 0); err == nil {
		t.Error("Coid accepted a market index of 1000")
	}
	if _, err := Coid("run", -1, quote.SideYes, 0); err == nil {
		t.Error("Coid accepted a negative market index")
	}
	if _, err := Coid("run", 0, quote.SideYes, 100_000_000); err == nil {
		t.Error("Coid accepted a sequence of 100000000")
	}
}

// TestParseCoidRejectsForeignOrders is §7.5's foreign-activity assertion.
//
// Anything not recognisably ours is escalated to the operator rather than
// adopted. Being strict is the safe direction: an unrecognised order raises an
// alarm, whereas a mis-parsed one is silently attributed to a market it has
// nothing to do with, and the harness then sizes that market's reducer against
// a position it does not hold.
func TestParseCoidRejectsForeignOrders(t *testing.T) {
	for _, tc := range []struct {
		coid string
		why  string
	}{
		{"", "an order with no coid at all"},
		{"7f3a2b1c-9d8e-4f5a-b6c7-d8e9f0a1b2c3", "probebot.py's uuid4()"},
		{"lipH-run-003-no", "a truncated coid: four fields, not five"},
		{"lipH-run-003-no-00000001-extra", "six fields"},
		{"lip-run-003-no-00000001", "the wrong prefix -- `lip` is not `lipH`"},
		{"LIPH-run-003-no-00000001", "the prefix is case-sensitive"},
		{"lipH-run-3-no-00000001", "an unpadded market index: %03d is " +
			"fixed-width, and reading a variable-width field as though it " +
			"were fixed is how a parse silently succeeds on the wrong string"},
		{"lipH-run-003-no-47", "an unpadded sequence"},
		{"lipH-run-003-buy-00000001", "`buy` is not a book side; §4 has only " +
			"yes and no, and the wire's bid/ask lives at the edge"},
		{"lipH-run-abc-no-00000001", "a non-numeric market index"},
		{"lipH-run-003-no-abcdefgh", "a non-numeric sequence"},
		{"lipH--003-no-00000001", "an empty run id"},
	} {
		if p, ok := ParseCoid(tc.coid); ok {
			t.Errorf("ParseCoid(%q) = %+v, ok -- %s", tc.coid, p, tc.why)
		}
		if IsOurs(tc.coid) {
			t.Errorf("IsOurs(%q) = true -- %s", tc.coid, tc.why)
		}
	}

	ours, _ := Coid("01JZQK8ZR9", 0, quote.SideYes, 1)
	if !IsOurs(ours) {
		t.Errorf("IsOurs(%q) = false for a coid we just built", ours)
	}
}

// TestCoidsAreUniquePerOrder.
//
// At most one order can exist per coid (H-ORD-2b), so two different orders must
// never share one. A collision would make the 409 recovery path identify the
// wrong order as the survivor.
func TestCoidsAreUniquePerOrder(t *testing.T) {
	seen := map[string]bool{}
	for idx := range 6 {
		for _, side := range []quote.Side{quote.SideYes, quote.SideNo} {
			for seq := uint64(0); seq < 200; seq++ {
				c, err := Coid("run", idx, side, seq)
				if err != nil {
					t.Fatal(err)
				}
				if seen[c] {
					t.Fatalf("coid collision at market %d, %s, seq %d: %q",
						idx, side, seq, c)
				}
				seen[c] = true
			}
		}
	}
}
