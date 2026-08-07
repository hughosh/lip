package hstore

import (
	"errors"
	"fmt"
	"sync"

	"lip/harness/quote"
	"lip/harness/rest"
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
//   - a binding submitted and not yet committed, or one that failed: an ERROR.
//
// The third is why the seam returns `([]bool, error)` rather than `bool`.
// "Ledger unavailable" must never become "foreign".
type Ownership struct {
	mu sync.RWMutex
	// byOrder maps a committed exchange order id to the coid that reserved it.
	byOrder map[string]string
	// pending counts submitted-but-uncommitted bindings per order id.
	pending map[string]int
	// failed records bindings the writer has stopped retrying, with why.
	failed map[string]string
}

func newOwnership() *Ownership {
	return &Ownership{
		byOrder: make(map[string]string),
		pending: make(map[string]int),
		failed:  make(map[string]string),
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
func (o *Ownership) OwnsOrders(orderIDs []string) ([]bool, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	out := make([]bool, len(orderIDs))
	for i, id := range orderIDs {
		if why, bad := o.failed[id]; bad {
			return nil, fmt.Errorf("the durable binding for order %s did not "+
				"commit (%s), so this order is neither owned nor foreign and "+
				"no fill on it can be classified", id, why)
		}
		if o.pending[id] > 0 {
			return nil, fmt.Errorf("a binding for order %s has been submitted "+
				"and has not committed; H-ORD-9 classifies against the DURABLE "+
				"ledger, and answering from an uncommitted intent is the "+
				"in-memory fallback the rule forbids", id)
		}
		_, ok := o.byOrder[id]
		out[i] = ok
	}
	return out, nil
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

// load installs the bindings read from the database at Open.
func (o *Ownership) load(bindings map[string]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.byOrder = make(map[string]string, len(bindings))
	for orderID, coid := range bindings {
		o.byOrder[orderID] = coid
	}
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

// confidence: high
