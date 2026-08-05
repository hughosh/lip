package core

import "testing"

// The differential replay is STRUCTURALLY blind to everything in this file: a
// tape is the input to a replay, so no reconnect ever occurs during one. These
// rules are carried by these tests and by nothing else — the same position P5,
// P7 and P13 were in before gate 7 exposed it.

// --- P25: reconnect discards five pieces of state, together -----------------

func TestP25_ResetOnReconnectClearsEverything(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0, "BBB": 0})

	feed(t, r,
		snapshot(2, 1, "AAA"),
		snapshot(2, 2, "BBB"),
		delta(2, 3, "AAA", 1_000_000),
	)
	if r.StaleCount() != 0 {
		t.Fatalf("stale = %d before the reconnect, want 0", r.StaleCount())
	}
	if r.Book("AAA").Yes().Len() == 0 {
		t.Fatal("AAA's book is empty before the reconnect; the fixture is wrong")
	}

	r.ResetOnReconnect()

	for _, tk := range []string{"AAA", "BBB"} {
		b := r.Book(tk)
		if b.Yes().Len() != 0 || b.No().Len() != 0 {
			t.Errorf("%s: levels survived the reconnect (yes=%d no=%d). A level "+
				"carried across the gap may already be wrong, and a trade arriving "+
				"before the replacement snapshot would be attributed to it",
				tk, b.Yes().Len(), b.No().Len())
		}
		if _, _, lag := b.StateBefore(9_999_999); lag != nil {
			t.Errorf("%s: history survived the reconnect", tk)
		}
		if b.RefYes != nil || b.RefNo != nil || b.Gate != nil {
			t.Errorf("%s: RefYes/RefNo/Gate = %v/%v/%v, want all nil",
				tk, b.RefYes, b.RefNo, b.Gate)
		}
	}
	if r.StaleCount() != 2 {
		t.Errorf("stale = %d, want 2: every market is quarantined until it resnaps",
			r.StaleCount())
	}
	if len(r.Sids()) != 0 {
		t.Errorf("Sids() = %v, want empty: the new connection may renumber them",
			r.Sids())
	}
	if r.NeedsResnapshot {
		t.Error("NeedsResnapshot survived the reconnect")
	}
}

// The reference reset is the half with no visible symptom until it is missing,
// so pin the symptom: an identical book after a reconnect must STILL write a
// reference row, because change detection has nothing left to compare against.
func TestP25_ReferenceIsRewrittenAfterReconnectEvenIfTheBookIsIdentical(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0})

	feed(t, r, snapshot(2, 1, "AAA"))
	if len(rec.refs) != 1 {
		t.Fatalf("got %d reference rows from the first snapshot, want 1", len(rec.refs))
	}
	// A second identical snapshot writes nothing: nothing moved.
	feed(t, r, snapshot(2, 2, "AAA"))
	if len(rec.refs) != 1 {
		t.Fatalf("an unchanged book wrote %d reference rows, want 1", len(rec.refs))
	}

	r.ResetOnReconnect()
	feed(t, r, snapshot(7, 1, "AAA")) // same book, new connection, new sid

	if len(rec.refs) != 2 {
		t.Errorf("got %d reference rows, want 2: after a reconnect the first "+
			"snapshot must write one even though the book is unchanged, because "+
			"RefYes/RefNo/Gate were reset to nil", len(rec.refs))
	}
}

// A delta arriving before the replacement snapshot must be ignored: that is what
// the stale set is for, and it is the failure the reset exists to prevent.
func TestP25_DeltaBeforeResnapshotIsIgnored(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0})

	feed(t, r, snapshot(2, 1, "AAA"))
	r.ResetOnReconnect()

	feed(t, r, delta(7, 1, "AAA", 5_000_000))
	if r.Book("AAA").Yes().Len() != 0 {
		t.Error("a delta was applied to a quarantined book after a reconnect")
	}
	if r.Watermark() != 0 {
		t.Errorf("watermark = %d, want 0: the delta branch returns before "+
			"advancing it for a stale market (P15)", r.Watermark())
	}
}

// --- P27 support: sid order is insertion order, not map order ---------------

func TestP25_SidsAreInFirstSeenOrder(t *testing.T) {
	rec := &recorder{}
	r := NewRig(rec, map[string]float64{"AAA": 0, "BBB": 0})

	// Descending sids, so a sorted or map-ranged implementation disagrees.
	feed(t, r,
		snapshot(9, 1, "AAA"),
		snapshot(4, 1, "BBB"),
		delta(9, 2, "AAA", 1_000_000),
		delta(4, 2, "BBB", 1_000_001),
	)

	got := r.Sids()
	want := []int64{9, 4}
	if len(got) != len(want) {
		t.Fatalf("Sids() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sids() = %v, want %v (Python sends list(self.seq), which is "+
				"dict insertion order)", got, want)
		}
	}
}
