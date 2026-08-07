package hstore

import (
	"errors"
	"fmt"
	"sync"

	"lip/harness/quote"
	"lip/harness/rest"
	"lip/harness/risk"
)

// ---------------------------------------------------------------------------
// H-ORD-9 — the durable ownership barrier
// ---------------------------------------------------------------------------

// Ownership answers `risk.OwnershipLookup`, and it answers it from an in-memory
// index over COMMITTED `owned_order` bindings and nothing else.
//
// This is a read index over durable facts, not a fallback. The distinction is
// the whole of H-ORD-9: an index that could answer from something other than the
// ledger would answer during exactly the failure the rule exists for, and the
// answer it would give -- "not ours" -- is the one that turns a manual taker
// fill into a false F14 or a real one into a silent hole.
//
// So there are three states per order id, not two:
//
//   - in the committed index: OURS;
//   - absent, with no binding outstanding: FOREIGN;
//   - a binding submitted and not yet committed, or a reservation that has not
//     been resolved at all: UNRESOLVED.
//
// and one state that is not about the order at all: a binding the writer has
// stopped retrying makes the whole batch an ERROR, because the store is faulty
// and nothing it says about anything can be relied on.
//
// # The unresolved set, and why the absence of a row is not evidence
//
// H-ORD-6 commits the reservation BEFORE the order is dispatched, so between
// that commit and the binding's there is a coid in the ledger with no order id
// against it. A fill arriving in that window carries an order id the index has
// never seen -- and the index used to answer plain `false`, which is FOREIGN,
// which is SEV1 plus a durable operator-only WINDING_DOWN latch produced by our
// own order (adversarial FINDING 2). The window is not narrow: it spans the
// process crash that H-ORD-6 exists for, because the reservation is durable and
// the binding is what was lost.
//
// `unresolved` is therefore the set of coids the ledger has reserved and not yet
// resolved -- neither bound nor abandoned. While it is non-empty, an order id
// absent from `byOrder` is UNRESOLVED and not foreign: some outstanding
// reservation could still turn out to be it. Only when the set is EMPTY has the
// ledger accounted for every order it could have created, and only then does
// absence mean foreign. That is H-ORD-9's "absent, with no binding outstanding"
// read literally.
//
// It is keyed by COID rather than by order id on purpose: the order id is the
// thing that is not known. Which means the answer it gives is a GATE and not a
// per-id lookup, and it is deliberately coarse -- one outstanding reservation
// defers every unrecognised fill. `lip-eyq`'s startup walk is what drains it.
type Ownership struct {
	mu sync.RWMutex
	// byOrder maps a committed exchange order id to the coid that reserved it.
	byOrder map[string]string
	// pending counts submitted-but-uncommitted bindings per order id.
	pending map[string]int
	// failed records bindings the writer has stopped retrying, with why.
	failed map[string]string
	// unresolved is the set of coids with a committed reservation and no
	// committed resolution -- no order id, no abandonment.
	unresolved map[string]struct{}
}

func newOwnership() *Ownership {
	return &Ownership{
		byOrder:    make(map[string]string),
		pending:    make(map[string]int),
		failed:     make(map[string]string),
		unresolved: make(map[string]struct{}),
	}
}

// OwnsOrders classifies a batch of exchange order ids against the ledger.
//
// The batch shape is not an optimisation. `risk.Portfolio.ApplyFills` must
// classify EVERY unseen fill before it mutates anything, because a walk applied
// half-way leaves `q_local` describing a prefix of the account's history while
// the dedup set claims all of it. A per-fill lookup that could fail in the
// middle makes that atomicity impossible to state.
//
// The returned slice always matches the input length on success. On error it is
// nil and NOTHING may be inferred about any id in the batch.
//
// The whole-batch error is now reserved for ONE cause: a binding the writer has
// stopped retrying. That is a store fault, and it is the case
// `OWNERSHIP_LEDGER_UNAVAILABLE` was written for. A binding merely in flight is
// no longer an error -- it is `OwnershipUnresolved`, which is epistemically the
// same statement ("the durable ledger has not concluded") delivered per-id
// instead of by poisoning the walk, and which the caller can defer on rather
// than having to discard a complete read.
func (o *Ownership) OwnsOrders(orderIDs []string) ([]risk.Ownership, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	out := make([]risk.Ownership, len(orderIDs))
	for i, id := range orderIDs {
		if why, bad := o.failed[id]; bad {
			return nil, fmt.Errorf("the durable binding for order %s did not "+
				"commit (%s), so this order is neither owned nor foreign and "+
				"no fill on it can be classified", id, why)
		}
		_, bound := o.byOrder[id]
		switch {
		case bound:
			out[i] = risk.OwnershipOurs
		case o.pending[id] > 0:
			// Submitted, not committed. H-ORD-9 classifies against the DURABLE
			// ledger, so an uncommitted intent is not evidence of ownership --
			// but it is conclusive evidence that this id is not a stranger's.
			// `M-HS-BINDCACHE` answers OURS here, which makes a binding that
			// never lands look like a fact.
			out[i] = risk.OwnershipUnresolved
		case len(o.unresolved) > 0:
			// Not bound, and the ledger holds reservations that could still
			// become this id. `M-HS-OWNCONCLUSIVE` drops this arm and answers
			// FOREIGN, which is the F2 catastrophe: a SEV1 and a durable global
			// stop declared about our own dispatched order.
			out[i] = risk.OwnershipUnresolved
		default:
			out[i] = risk.OwnershipForeign
		}
	}
	return out, nil
}

// Unresolved reports the coids with a committed reservation and no committed
// resolution, as a copy.
//
// A copy and not the map, and no SQL surface: this exists so the portfolio walk
// can ask "is this listed order one of mine that I have lost the binding for?"
// and bind it. A caller holding the live map could mutate the index that decides
// whether a fill is foreign, from outside the writer that is the only thing
// allowed to change it.
func (o *Ownership) Unresolved() map[string]struct{} {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]struct{}, len(o.unresolved))
	for coid := range o.unresolved {
		out[coid] = struct{}{}
	}
	return out
}

// UnresolvedCount is how many reservations are outstanding. It is what makes
// FOREIGN conclusive when it reads zero.
func (o *Ownership) UnresolvedCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.unresolved)
}

// Bound returns the coid a committed binding attached to this order id.
func (o *Ownership) Bound(orderID string) (string, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	coid, ok := o.byOrder[orderID]
	return coid, ok
}

// Size is how many committed bindings the index holds.
func (o *Ownership) Size() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.byOrder)
}

// load installs the bindings AND the outstanding reservations read from the
// database at Open.
//
// Both halves, from the same read, before any classification can happen. An Open
// that seeded only the bindings would spend its first poll answering FOREIGN for
// every order the previous incarnation dispatched and did not get to bind --
// which is the crash H-ORD-6's two-stage commit exists to survive, converted
// into a global stop by the recovery.
func (o *Ownership) load(bindings map[string]string,
	unresolved map[string]struct{}) {

	o.mu.Lock()
	defer o.mu.Unlock()
	o.byOrder = make(map[string]string, len(bindings))
	for orderID, coid := range bindings {
		o.byOrder[orderID] = coid
	}
	o.unresolved = make(map[string]struct{}, len(unresolved))
	for coid := range unresolved {
		o.unresolved[coid] = struct{}{}
	}
}

// reserveCommitted is called ONLY after a reservation's transaction commits. It
// opens the window in which an unrecognised order id is unresolved rather than
// foreign, and the window opens at the same moment the dispatch becomes legal.
func (o *Ownership) reserveCommitted(coid string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.unresolved[coid] = struct{}{}
}

// resolveCommitted closes it, from either terminal direction: a committed
// binding or a committed abandonment.
func (o *Ownership) resolveCommitted(coid string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.unresolved, coid)
}

// markPending is called at SUBMISSION. It makes the order unclassifiable, which
// is the honest answer while the binding is in flight.
func (o *Ownership) markPending(orderID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pending[orderID]++
	delete(o.failed, orderID)
}

// commitBinding is called ONLY after the transaction commits.
func (o *Ownership) commitBinding(orderID, coid string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending[orderID] > 0 {
		o.pending[orderID]--
		if o.pending[orderID] == 0 {
			delete(o.pending, orderID)
		}
	}
	delete(o.failed, orderID)
	o.byOrder[orderID] = coid
	// The reservation is resolved in the same instant the binding becomes a
	// durable fact. Leaving it outstanding would keep every unrecognised fill
	// deferred forever on the strength of a coid that is no longer outstanding.
	delete(o.unresolved, coid)
}

// failBinding is called when the writer has stopped retrying a binding.
func (o *Ownership) failBinding(orderID string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending[orderID] > 0 {
		o.pending[orderID]--
		if o.pending[orderID] == 0 {
			delete(o.pending, orderID)
		}
	}
	if _, done := o.byOrder[orderID]; done {
		return
	}
	o.failed[orderID] = err.Error()
}

// ---------------------------------------------------------------------------
// The two stages
// ---------------------------------------------------------------------------

// Ownership returns the durable ownership index. It satisfies
// `risk.OwnershipLookup`.
func (s *Store) Ownership() *Ownership { return s.own }

// UnresolvedReservations is the coids the portfolio walk may still be able to
// bind, as a copy. Half of `wsx.OrderBinder`.
func (s *Store) UnresolvedReservations() map[string]struct{} {
	return s.own.Unresolved()
}

// BindListedOrder is `BindOrder` without the receipt, for the adopted
// resting-order walk.
//
// The receipt is dropped deliberately rather than forgotten. A receipt exists so
// a caller can match a durable outcome in `TakeResults`, and the walk has
// nothing to match it to: it re-runs every poll cycle, it re-learns the same
// binding until one commits, and an already-committed binding is idempotent. The
// walk's contract is "submit and move on"; keeping the receipt would suggest
// there is a completion it is entitled to wait for, and a poll loop that waited
// for durability before classifying fills would stall the position model behind
// the disk.
func (s *Store) BindListedOrder(coid, orderID string, boundMs int64) error {
	_, err := s.BindOrder(coid, orderID, boundMs)
	return err
}

// ReserveOrder is stage one of H-ORD-6: the coid becomes durable BEFORE the
// order is dispatched.
//
// It returns a receipt, not a permit. The permit arrives through `TakeResults`
// once the SQLite transaction has committed, and that is the entire mechanism:
// there is no code path from "I want to place an order" to "I may place this
// order" that does not pass through a commit. `M-HS-PERMIT` adds one.
func (s *Store) ReserveOrder(h RunHandle, o rest.CreateOrder, role quote.Role,
	reservedMs int64) (Receipt, error) {

	rec, err := newReservation(h, o, role, reservedMs)
	if err != nil {
		return Receipt{}, err
	}
	return s.submit(&submission{
		kind:    KindReserveOrder,
		reserve: rec,
		order:   o,
		role:    role,
	})
}

// BindOrder is stage two: the exchange order id learned from an ACK, a same-coid
// recovery, or an adopted resting-order walk.
//
// The ownership index is updated ONLY after the transaction commits. Until then
// the order id is PENDING, and `OwnsOrders` returns an error for it rather than
// a guess in either direction. `M-HS-BINDCACHE` updates the index here instead,
// which makes a binding that never lands look like a fact.
func (s *Store) BindOrder(coid, orderID string, boundMs int64) (Receipt, error) {
	if coid == "" {
		return Receipt{}, errors.New("a binding with no coid: the reservation " +
			"is what makes the order ours, and there is nothing to bind to")
	}
	if orderID == "" {
		return Receipt{}, errors.New("a binding with no order id: H-ORD-9 " +
			"classifies fills by order_id, and an empty one classifies nothing")
	}
	if boundMs <= 0 {
		return Receipt{}, errors.New("a binding with no timestamp; the schema " +
			"requires order_id and bound_ms to be set together")
	}
	bind := orderBinding{Coid: coid, OrderID: orderID, BoundMs: boundMs}
	s.own.markPending(orderID)
	rcpt, err := s.submit(&submission{kind: KindBindOrder, bind: bind})
	if err != nil {
		// The binding was never accepted, so the order id must not be left
		// pending forever: it would be permanently unclassifiable on the
		// strength of a submission that did not happen.
		s.own.failBinding(orderID, err)
		return Receipt{}, err
	}
	return rcpt, nil
}

// ResolveReservationAbandoned is stage two's other terminal: the exchange never
// took this coid, so no order id will ever bind to it.
//
// It is the drain on the unresolved set, and something has to drain it or the
// first reservation that never reached the exchange defers every unrecognised
// fill for the lifetime of the deployment -- which converts the F2 repair from
// "no false foreign" into "no foreign ever", and H-ORD-9 would then be a rule
// with no consequence. `lip-eyq` owns the walk that decides a coid was never
// taken; this is the record it writes.
//
// Like every other stage-two write, the index changes only after the commit. An
// abandonment held in memory would let a coid stop deferring while the row still
// says it is outstanding, and the next restart would resurrect it.
func (s *Store) ResolveReservationAbandoned(coid string, abandonedMs int64) (
	Receipt, error) {

	if coid == "" {
		return Receipt{}, errors.New("an abandonment with no coid: the " +
			"reservation is the thing being resolved, and there is nothing to " +
			"resolve")
	}
	if abandonedMs <= 0 {
		return Receipt{}, errors.New("an abandonment with no timestamp; when " +
			"we concluded the exchange never took the order is the whole of " +
			"what this row records")
	}
	return s.submit(&submission{
		kind:    KindAbandonReservation,
		abandon: reservationAbandonment{Coid: coid, AbandonedMs: abandonedMs},
	})
}

// confidence: high
