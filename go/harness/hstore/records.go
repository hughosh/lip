package hstore

import (
	"errors"
	"fmt"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// Kinds and receipts
// ---------------------------------------------------------------------------

// RecordKind names which of the five records a submission produces.
type RecordKind uint8

const (
	// KindInvalid is the zero value and names nothing. A submission that
	// reached the writer carrying it is a programming error, not a record.
	KindInvalid RecordKind = iota
	KindBeginRun
	KindReserveOrder
	KindBindOrder
	// KindAbandonReservation is stage two's other terminal: the exchange never
	// took the coid. It writes `owned_order.abandoned_ms` and is what drains
	// the unresolved set.
	KindAbandonReservation
	KindFill
	KindStateEvent
	KindAnomaly
	KindDelivery
)

func (k RecordKind) String() string {
	switch k {
	case KindBeginRun:
		return "run"
	case KindReserveOrder:
		return "reserve_order"
	case KindBindOrder:
		return "bind_order"
	case KindAbandonReservation:
		return "abandon_reservation"
	case KindFill:
		return "our_fill"
	case KindStateEvent:
		return "state_event"
	case KindAnomaly:
		return "anomaly"
	case KindDelivery:
		return "delivery"
	}
	return "invalid"
}

// Receipt identifies one submitted record so its durable outcome can be matched
// to it in `TakeResults`.
//
// The zero value is INVALID: sequence numbers begin at 1, so a caller that
// forgot to keep the receipt cannot accidentally match the first record.
type Receipt struct {
	seq  uint64
	kind RecordKind
}

// Valid reports whether this receipt came from a submission.
func (r Receipt) Valid() bool { return r.seq != 0 }

// Kind is which record this receipt is for.
func (r Receipt) Kind() RecordKind { return r.kind }

// Seq is the submission order. Monotonic within one Store.
func (r Receipt) Seq() uint64 { return r.seq }

// ---------------------------------------------------------------------------
// The two licences
// ---------------------------------------------------------------------------

// RunHandle is proof that this run's `run` row is durable.
//
// It is required by every record that references `run(run_id)`, which is the
// whole point: the foreign key would fail at the writer, but by then the caller
// has already believed a record was accepted. Requiring the handle moves that
// failure to the submission, where the caller can still do something about it.
//
// The zero value is INVALID and no exported constructor produces one. Go permits
// `hstore.RunHandle{}` from any package -- every field is unexported, so the
// composite literal compiles -- and that is exactly why the zero value must
// assert nothing. `TestStoreSurfaceCannotForgeLicences` asserts the seal from
// outside the package, which is the only vantage point from which it is a
// checkable claim.
type RunHandle struct {
	runID string
	seq   uint64
}

// Valid reports whether this handle was issued by a committed `run` row.
func (h RunHandle) Valid() bool { return h.runID != "" && h.seq != 0 }

// RunID is the run this handle licenses records for.
func (h RunHandle) RunID() string { return h.runID }

// DispatchPermit is H-ORD-6's ownership barrier made into an object: it exists
// only because a coid reservation COMMITTED before the order was dispatched.
//
// The eventual `lip-3af` dispatcher consumes permits and never a raw
// `rest.CreateOrder`, so "did we record this order before we sent it?" stops
// being a rule someone has to remember and becomes the only way to obtain the
// argument. `M-HS-PERMIT` issues one at enqueue time to show the difference is
// real.
//
// The permit is bound byte-for-byte to its order: `Order` re-checks that the
// carried order still names the coid the reservation committed, so a permit
// cannot be paired with a different order by a caller holding both.
type DispatchPermit struct {
	coid  string
	order rest.CreateOrder
	role  quote.Role
	store *Store
}

// Valid reports whether this permit was issued by a committed reservation.
func (p DispatchPermit) Valid() bool {
	return p.store != nil && p.coid != "" && p.order.ClientOrderID() != ""
}

// Coid is the durably reserved client order id.
func (p DispatchPermit) Coid() string { return p.coid }

// Role is whether the permitted order adds risk or reduces it.
func (p DispatchPermit) Role() quote.Role { return p.role }

// Order releases the permitted order, or says why it may not be dispatched.
//
// H-STORE-3 is enforced HERE and only here. An ADDING order requires storage to
// be healthy right now: the ownership record of an order placed while the store
// is broken cannot be written, and an order whose ownership is unrecorded is a
// fill we will later classify as foreign. A REDUCING order is deliberately
// unaffected -- it stays dispatchable after a later storage failure, because the
// alternative is a disk problem that strands inventory, and every stop path in
// this system stops adding and none stops reducing (I1).
func (p DispatchPermit) Order() (rest.CreateOrder, error) {
	if !p.Valid() {
		return rest.CreateOrder{}, errors.New("this dispatch permit was not " +
			"issued by a committed coid reservation; H-ORD-6 requires the " +
			"ownership record to be durable BEFORE the order is dispatched, " +
			"and a zero permit asserts exactly the thing that was never done")
	}
	if p.order.ClientOrderID() != p.coid {
		return rest.CreateOrder{}, fmt.Errorf("permit reserved coid %q but "+
			"carries an order for %q; the permit is bound to the order whose "+
			"reservation committed and to no other", p.coid,
			p.order.ClientOrderID())
	}
	if p.role == quote.RoleAdding && !p.store.Health().AllowsAdding() {
		return rest.CreateOrder{}, fmt.Errorf("storage is unhealthy (%s), so "+
			"adding authority is revoked (H-STORE-3): an order placed now "+
			"could not have its ownership recorded, and an unrecorded order "+
			"produces a fill that H-ORD-9 must classify as foreign. Reducing "+
			"and monitoring are unaffected", p.store.Health().LastError())
	}
	return p.order, nil
}

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

// Result is one submitted record's durable outcome.
//
// The two licences are delivered HERE and nowhere else, which is what makes
// "only after the transaction commits" structural rather than remembered.
type Result struct {
	// Receipt matches this result to its submission.
	Receipt Receipt
	// Kind is the record that was written.
	Kind RecordKind
	// Err is why the record is not durable. A result with a non-nil Err is
	// terminal: the writer only publishes one after it has stopped retrying.
	Err error

	run    RunHandle
	permit DispatchPermit
}

// OK reports whether the record is durable.
func (r Result) OK() bool { return r.Err == nil }

// RunHandle is the licence a committed `run` row issues.
func (r Result) RunHandle() (RunHandle, bool) {
	if r.Err != nil || !r.run.Valid() {
		return RunHandle{}, false
	}
	return r.run, true
}

// Permit is the licence a committed coid reservation issues.
func (r Result) Permit() (DispatchPermit, bool) {
	if r.Err != nil || !r.permit.Valid() {
		return DispatchPermit{}, false
	}
	return r.permit, true
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

// Health is the store's own report on itself, taken as an immutable snapshot.
//
// The zero value is CONSERVATIVE: not healthy, adding not authorised. A caller
// that failed to obtain a real snapshot must not read the failure as permission,
// and `var h Health` is the shape that failure takes in Go.
//
// There is deliberately nothing here that can disable reducing or monitoring.
// H-STORE-3 revokes adding and only adding, so a predicate for the other two
// would be a predicate some caller eventually consults.
type Health struct {
	healthy   bool
	adding    bool
	stalled   bool
	pending   int
	failures  uint64
	committed uint64
	lastErr   string
}

// Healthy reports whether every submitted record is durable or on its way.
func (h Health) Healthy() bool { return h.healthy }

// AllowsAdding is H-STORE-3's authority, and the only authority this type has.
func (h Health) AllowsAdding() bool { return h.adding }

// Stalled reports whether the writer's current record has been in flight past
// the progress bound. A stall is unhealthy for the same reason a failure is:
// nothing behind it is durable.
func (h Health) Stalled() bool { return h.stalled }

// Pending is how many submitted records are not yet durable.
func (h Health) Pending() int { return h.pending }

// Failures counts write attempts that returned an error.
func (h Health) Failures() uint64 { return h.failures }

// Committed is the sequence number of the last durable record.
func (h Health) Committed() uint64 { return h.committed }

// LastError is the most recent write failure, or "" when there has been none
// since the store last became healthy.
func (h Health) LastError() string { return h.lastErr }

// ---------------------------------------------------------------------------
// The record payloads
// ---------------------------------------------------------------------------

// runRecord is one `run` row: §15 requires "every parameter in §16, verbatim".
type runRecord struct {
	RunID     string
	StartedMs int64
	Params    cfg.Params
	// ConfigJSON is the encoded form. It is computed at SUBMISSION so that an
	// unencodable configuration fails where the caller can see it, and stored
	// so the writer's retry cannot re-encode a struct that has since changed.
	ConfigJSON []byte
}

// orderReservation is stage one of `owned_order`: durable before dispatch.
type orderReservation struct {
	Coid       string
	RunID      string
	ReservedMs int64
	Ticker     string
	Side       string
	Role       string
	PriceCents int
	CountQ     int64
}

// orderBinding is stage two: the exchange order id, learned from an ACK, a
// same-coid recovery, or an adopted resting-order walk.
type orderBinding struct {
	Coid    string
	OrderID string
	BoundMs int64
}

// reservationAbandonment is stage two's other terminal: the conclusion, recorded
// durably, that the exchange never took this coid and no order id will bind to
// it. It is what allows `Ownership` to stop deferring on that reservation.
type reservationAbandonment struct {
	Coid        string
	AbandonedMs int64
}

// fillRecord is one `our_fill` row.
type fillRecord struct {
	TradeID      string
	FirstRunID   string
	FirstSeenMs  int64
	Backfilled   bool
	OrderID      string
	Ticker       string
	Side         string
	Price4       int64
	CountQ       int64
	FeeMicros    int64
	IsTaker      bool
	ExchangeTsMs int64
}

// anomalyRecord is one `anomaly` row at first sight. `journaled_ms`,
// `delivered_ms`, `attempts`, `last_attempt_ms` and `suppressed_count` are
// managed by the writer and the delivery path, never by the producer.
type anomalyRecord struct {
	AnomalyID string
	RunID     string
	Class     string
	Sev       int
	Ticker    string
	Text      string
	FirstMs   int64
}

// DeliveryAttempt is one push's outcome, covering every anomaly row it
// represented.
//
// It carries a LIST because §13.2's suppression is an aggregate: one push
// reports a class and market's occurrences and marks every one of them
// delivered. Recording them one at a time would leave a window in which some
// rows of a delivered aggregate are still pending, and a restart in that window
// would deliver the remainder a second time.
type DeliveryAttempt struct {
	// AnomalyIDs is every row this push represented, oldest first. The first is
	// the representative whose text was sent.
	AnomalyIDs []string
	// AttemptMs is when the push was attempted.
	AttemptMs int64
	// Delivered is whether the transport reported success.
	Delivered bool
	// DeliveredMs is when it succeeded. Ignored unless Delivered.
	DeliveredMs int64
}

func (d DeliveryAttempt) validate() error {
	if len(d.AnomalyIDs) == 0 {
		return errors.New("a delivery attempt that represents no anomaly row " +
			"is not an attempt; the row is the thing whose delivery state is " +
			"being changed")
	}
	seen := make(map[string]struct{}, len(d.AnomalyIDs))
	for _, id := range d.AnomalyIDs {
		if id == "" {
			return errors.New("a delivery attempt named an empty anomaly id")
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("anomaly %s appears twice in one delivery "+
				"attempt, so its suppression count would be wrong", id)
		}
		seen[id] = struct{}{}
	}
	if d.AttemptMs <= 0 {
		return errors.New("a delivery attempt with no timestamp cannot pace " +
			"its own retry ladder")
	}
	if d.Delivered && d.DeliveredMs <= 0 {
		return errors.New("a delivery reported successful with no delivery " +
			"timestamp; §13's record is when the operator was told, and a zero " +
			"there reads as 1970")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Conversions from the harness's own types
// ---------------------------------------------------------------------------

// newReservation converts a validated order intent into stage one of
// `owned_order`, in exact integer units.
func newReservation(h RunHandle, o rest.CreateOrder, role quote.Role,
	reservedMs int64) (orderReservation, error) {

	if !h.Valid() {
		return orderReservation{}, errors.New("no run handle: an owned_order " +
			"row references run(run_id), and a reservation without a committed " +
			"run is a coid attributed to no incarnation")
	}
	if o.ClientOrderID() == "" {
		return orderReservation{}, errors.New("an order with no client order " +
			"id cannot be reserved: the coid IS the ownership record (H-ORD-1)")
	}
	if !rest.IsOurs(o.ClientOrderID()) {
		return orderReservation{}, fmt.Errorf("coid %q does not parse as one "+
			"of ours; §7.5's adoption classifies resting orders by that parse, "+
			"so reserving an unparseable coid records an order the next "+
			"incarnation will call foreign", o.ClientOrderID())
	}
	if o.Ticker() == "" {
		return orderReservation{}, errors.New("an order with no ticker")
	}
	if role != quote.RoleAdding && role != quote.RoleReducing {
		return orderReservation{}, fmt.Errorf("unknown role %d", role)
	}
	if reservedMs <= 0 {
		return orderReservation{}, errors.New("a reservation with no timestamp")
	}
	return orderReservation{
		Coid:       o.ClientOrderID(),
		RunID:      h.runID,
		ReservedMs: reservedMs,
		Ticker:     o.Ticker(),
		Side:       o.Side().String(),
		Role:       role.String(),
		PriceCents: o.PriceCents(),
		CountQ:     int64(o.Count()),
	}, nil
}

// newFillRecord converts one classified fill into `our_fill`'s exact units:
// quantity quanta, Price4 ten-thousandths of a dollar, and fee microdollars.
func newFillRecord(h RunHandle, f risk.FillEvent, firstSeenMs int64,
	backfilled bool) (fillRecord, error) {

	if !h.Valid() {
		return fillRecord{}, errors.New("no run handle: our_fill records the " +
			"run that FIRST saw the trade, and there is no such run without one")
	}
	if f.TradeID == "" {
		return fillRecord{}, errors.New("a fill with no trade_id cannot enter " +
			"our_fill: H-ORD-6 makes trade_id the primary key and the join " +
			"against rig.db")
	}
	if f.OrderID == "" {
		return fillRecord{}, errors.New("a fill with no order_id cannot be " +
			"attributed to a bound owned_order, and H-ORD-9 forbids the " +
			"inference that would otherwise fill the gap")
	}
	if firstSeenMs <= 0 {
		return fillRecord{}, errors.New("a fill with no first_seen timestamp")
	}
	if f.ExchangeTsMs <= 0 {
		return fillRecord{}, fmt.Errorf("fill %s carries no exchange "+
			"timestamp; without it the record cannot be ordered against the "+
			"public trade stream it is joined to", f.TradeID)
	}
	return fillRecord{
		TradeID:      f.TradeID,
		FirstRunID:   h.runID,
		FirstSeenMs:  firstSeenMs,
		Backfilled:   backfilled,
		OrderID:      f.OrderID,
		Ticker:       f.Ticker,
		Side:         f.Side.String(),
		Price4:       f.Price4,
		CountQ:       int64(f.Count),
		FeeMicros:    int64(f.Fee),
		IsTaker:      f.IsTaker,
		ExchangeTsMs: f.ExchangeTsMs,
	}, nil
}

// newAnomalyRecord converts a `risk.Anomaly` into its row.
func newAnomalyRecord(h RunHandle, anomalyID string, a risk.Anomaly,
	firstMs int64) (anomalyRecord, error) {

	if !h.Valid() {
		return anomalyRecord{}, errors.New("no run handle")
	}
	if anomalyID == "" {
		return anomalyRecord{}, errors.New("an anomaly with no id cannot be " +
			"reconciled against the text journal after a crash")
	}
	if a.Class == "" {
		return anomalyRecord{}, errors.New("an anomaly with no class cannot be " +
			"rate limited per (class,ticker) (§13.2)")
	}
	if a.Text == "" {
		return anomalyRecord{}, errors.New("an anomaly with no text is a row " +
			"the operator cannot act on")
	}
	if a.Sev != risk.SEV1 && a.Sev != risk.SEV2 && a.Sev != risk.SEV3 {
		return anomalyRecord{}, fmt.Errorf("unknown severity %d", a.Sev)
	}
	if firstMs <= 0 {
		return anomalyRecord{}, errors.New("an anomaly with no first_ms")
	}
	return anomalyRecord{
		AnomalyID: anomalyID,
		RunID:     h.runID,
		Class:     a.Class,
		Sev:       int(a.Sev),
		Ticker:    a.Ticker,
		Text:      a.Text,
		FirstMs:   firstMs,
	}, nil
}

// ---------------------------------------------------------------------------
// Read-only row views
// ---------------------------------------------------------------------------

// RunRow is a `run` row as read back, with its configuration decoded.
type RunRow struct {
	RunID     string
	StartedMs int64
	Params    cfg.Params
}

// OwnedOrderRow is an `owned_order` row as read back.
type OwnedOrderRow struct {
	Coid       string
	RunID      string
	ReservedMs int64
	Ticker     string
	Side       string
	Role       string
	PriceCents int
	Count      num.Qty
	// OrderID and BoundMs are set together or not at all.
	OrderID string
	BoundMs int64
	Bound   bool
	// AbandonedMs and Abandoned are the other terminal: the exchange never took
	// this coid. A row that is neither Bound nor Abandoned is a reservation
	// still outstanding, and it is why an order id absent from the ledger is
	// not yet evidence of a third party.
	AbandonedMs int64
	Abandoned   bool
}

// FillRow is an `our_fill` row as read back.
type FillRow struct {
	TradeID      string
	FirstRunID   string
	FirstSeenMs  int64
	Backfilled   bool
	OrderID      string
	Ticker       string
	Side         string
	Price4       int64
	Count        num.Qty
	Fee          num.Money
	IsTaker      bool
	ExchangeTsMs int64
}

// AnomalyRow is an `anomaly` row as read back, including its delivery state.
type AnomalyRow struct {
	AnomalyID string
	RunID     string
	Class     string
	Sev       risk.Severity
	Ticker    string
	Text      string
	FirstMs   int64
	// Journaled is false until BOTH journals hold the record. Until then the
	// row is not delivery-visible (§13.1).
	Journaled       bool
	JournaledMs     int64
	Delivered       bool
	DeliveredMs     int64
	Attempts        int
	LastAttemptMs   int64
	SuppressedCount int
}

// confidence: high
