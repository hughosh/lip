package hstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// TestRecordRejectionIsRaisedPerLostRecordAndNotFromHealth is where the SEV1
// comes FROM.
//
// Every terminal `Result` the store publishes is one record that is GONE, and
// nothing else raises that condition. The tempting alternative is to read it
// off `Health()`: the store latches a sticky `fault` and renders it into
// `LastError()`, so an unhealthy store does say that something was rejected.
// That reading is wrong in both directions and this test asserts both.
//
//   - It UNDERCOUNTS. `fault` is one string for the whole store, overwritten by
//     each rejection, so two lost records read as one condition -- and the one
//     it names is whichever happened last. The record whose evidence is gone is
//     the record nobody is now told about.
//   - It has no EDGE. A sticky fault is reported forever, so a health-derived
//     alert either fires once and never repeats, or repeats on every poll for
//     the life of the process. `Result.Err != nil` is terminal by construction
//     -- the writer never publishes an error it is still retrying -- so it
//     fires exactly once per record that was lost, which is the event.
//
// `LastError()` stays what it is: operator text for the heartbeat, not a
// protocol.
func TestRecordRejectionIsRaisedPerLostRecordAndNotFromHealth(t *testing.T) {
	s, _, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	// Two records that are rejected PERMANENTLY, for two different reasons on
	// two different coids. Neither coid was ever reserved, so neither can be
	// bound and neither can be abandoned; both are rule violations the writer
	// stops retrying, which is what makes each `Result` terminal.
	bindCoid, err := rest.Coid("runa", 0, quote.SideYes, 91)
	if err != nil {
		t.Fatal(err)
	}
	abandonCoid, err := rest.Coid("runa", 0, quote.SideNo, 92)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindOrder(bindCoid, "ord-orphan",
		1_700_000_001_000); err != nil {
		t.Fatalf("BindOrder: %v", err)
	}
	if _, err := s.ResolveReservationAbandoned(abandonCoid,
		1_700_000_002_000); err != nil {
		t.Fatalf("ResolveReservationAbandoned: %v", err)
	}
	// And one that succeeds, because a durable record must raise NOTHING. An
	// alert that fires on every outcome is an alert nobody reads.
	if _, err := s.RecordAnomaly(h, "keep-1",
		anomaly("OWNER_STALLED", risk.SEV1, ""), 1_700_000_003_000); err != nil {
		t.Fatalf("RecordAnomaly: %v", err)
	}

	results := pump(t, s)
	raised := Rejections(results)
	if len(raised) != 2 {
		t.Fatalf("%d SEV1(s) raised for 2 lost records and 1 durable one: %+v\n"+
			"results: %+v", len(raised), raised, results)
	}
	for _, a := range raised {
		if a.Class != RecordRejectedClass {
			t.Fatalf("a lost record raised class %q, want %q", a.Class,
				RecordRejectedClass)
		}
		if a.Sev != risk.SEV1 {
			t.Fatalf("a lost record raised %s; a record that is gone is not a "+
				"notice, it is a hole in the audit trail", a.Sev)
		}
	}

	// Each names ITS OWN record. One aggregate saying "something failed" is the
	// health reading by another route.
	joined := strings.Join([]string{raised[0].Text, raised[1].Text}, "\n")
	for _, want := range []string{bindCoid, abandonCoid,
		KindBindOrder.String(), KindAbandonReservation.String()} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no raised SEV1 names %q; the operator is told a record "+
				"was lost without being told which:\n%s", want, joined)
		}
	}
	if raised[0].Text == raised[1].Text {
		t.Fatalf("both lost records raised the SAME text, so the second is "+
			"indistinguishable from a repeat of the first:\n%s", raised[0].Text)
	}

	// The health reading, for contrast: ONE latched string, naming ONE of the
	// two coids. Deriving the alert from it loses the other record entirely.
	last := s.Health().LastError()
	named := 0
	for _, coid := range []string{bindCoid, abandonCoid} {
		if strings.Contains(last, coid) {
			named++
		}
	}
	if named != 1 {
		t.Fatalf("Health().LastError() names %d of the 2 lost records; this "+
			"test's premise is that it names exactly one, and that is why the "+
			"SEV1 cannot be derived from it: %s", named, last)
	}
}

// TestWriterExitRaisesOneRejectionPerRecordItWasHolding is the other terminal
// path.
//
// A writer that returns fails everything it was holding -- one terminal
// `Result` each -- and those records are gone for the same reason a permanently
// rejected one is: nothing will ever write them. The condition is the SAME
// condition, and it is reached without any permanent `fault` being latched at
// all, so a health-derived alert would report the writer's exit as one event
// rather than as the several records it destroyed.
func TestWriterExitRaisesOneRejectionPerRecordItWasHolding(t *testing.T) {
	s, c, _ := gatedStore(t)
	h := begin(t, s, "runa", 1_700_000_000_000)

	// Nothing commits from here on, so what is submitted stays queued.
	c.setBackErr(errors.New("input/output error"))
	for i, id := range []string{"gone-1", "gone-2"} {
		if _, err := s.RecordAnomaly(h, id,
			anomaly("FOREIGN_FILL", risk.SEV1, "KXTEST-A"),
			1_700_000_010_000+int64(i)); err != nil {
			t.Fatalf("RecordAnomaly %s: %v", id, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	<-done

	raised := Rejections(s.TakeResults())
	if len(raised) != 2 {
		t.Fatalf("a writer exited holding 2 accepted records and raised %d "+
			"SEV1(s): %+v", len(raised), raised)
	}
	for _, a := range raised {
		if a.Class != RecordRejectedClass || a.Sev != risk.SEV1 {
			t.Fatalf("a record lost with the writer raised %s %s, want SEV1 %s",
				a.Sev, a.Class, RecordRejectedClass)
		}
		if !strings.Contains(a.Text, KindAnomaly.String()) {
			t.Fatalf("the raised SEV1 does not say which record was lost: %s",
				a.Text)
		}
	}
}
