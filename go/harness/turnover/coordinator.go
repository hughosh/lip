package turnover

import (
	"context"
	"fmt"
	"sort"
	"time"

	"lip/harness/cfg"
	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

// Observations supplies read-only production adapters. Account must normalize
// account truth and local reservations under the existing owner's authority.
// Market must authenticate shard identity and scoped cash and return an external
// book mark (with our orders removed), plus the owner's schedule decision.
type Observations interface {
	Account(context.Context) (Input, error)
	Market(context.Context, string) (Observation, error)
}

type Book struct {
	Ticker string
	Proof  Proof
	Mark4  int64
}

type Schedule struct {
	Ticker string
	Proof  Proof
	// Deadlines are monotonic and include all close/early-close restrictions.
	// Zero grants nothing. The existing owner computes these restrictions.
	AddUntil, ReduceUntil time.Duration
}

type Observation struct {
	Identity Identity
	Cash     Cash
	Book     Book
	Schedule Schedule
}

// State is the durable restart record. Snapshot.Previous is always nil; the
// remaining value contains normalized obligations and frozen capital, not live
// pointers. Monotonic observations are historical after restart and never grant
// admission until Turn replaces them with fresh observations.
type State struct {
	Version       int
	Snapshot      Input
	StopAdding    bool
	LedgerMarkets []string
}

// Durable.Load returns nil only for a genuinely absent initial record. Replace
// must atomically and durably replace the record before returning nil. On an
// ambiguous failure the caller stops admission and requires a successful Turn;
// the next Turn conservatively retains any locally attempted reservation.
type Durable interface {
	Load(context.Context) (*State, error)
	Replace(context.Context, State) error
}

// Coordinator is a proposed collaborator of the single account owner. All
// methods must be called on that owner goroutine. It sends no exchange requests
// except through the read-only Observations contract, and never dispatches an
// admitted order. Existing quote, lifecycle, maker-only and transport guards
// remain required. No timers or independent trading authority live here.
type Coordinator struct {
	source    Observations
	store     Durable
	now       func() time.Duration
	params    cfg.Params
	state     State
	plan      *Plan
	books     map[string]Book
	schedules map[string]Schedule
	blocked   bool
}

// NewCoordinator reloads durable obligations. Frozen caps supplied on restart
// must match persisted caps, so a rotation/restart cannot mint fresh capital.
func NewCoordinator(ctx context.Context, source Observations, store Durable, now func() time.Duration, params cfg.Params, caps map[Shard]num.Money) (*Coordinator, error) {
	if source == nil || store == nil || now == nil || params.CapitalMax <= 0 || params.TruthMaxAge <= 0 {
		return nil, fmt.Errorf("missing coordinator dependency or capital/age bounds")
	}
	if err := params.Validate(); err != nil {
		return nil, err
	}
	c := &Coordinator{source: source, store: store, now: now, params: params, blocked: true}
	saved, err := store.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load turnover state: %w", err)
	}
	c.state = State{Version: 1, Snapshot: Input{FrozenCaps: cloneCaps(caps), AggregateCap: params.CapitalMax}}
	if saved == nil {
		return c, nil
	}
	if saved.Version != 1 || saved.Snapshot.Previous != nil || saved.Snapshot.AggregateCap != params.CapitalMax || !sameCaps(saved.Snapshot.FrozenCaps, caps) {
		return nil, fmt.Errorf("unsupported restart state or changed frozen capital")
	}
	c.state = cloneState(*saved)
	c.plan, err = Build(c.state.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("invalid durable turnover state: %w", err)
	}
	// Process monotonic clocks cannot be compared across restarts. This plan
	// retains obligations only; fresh Turn observations must establish truth.
	c.plan.At = 0
	return c, nil
}

// Turn gathers every managed market before persisting any selection change.
// A failed read/build/save leaves the published plan unchanged and admission
// blocked. Persisted UNKNOWN reservations omitted by a REST walk are retained.
func (c *Coordinator) Turn(ctx context.Context, selected []string) (*Plan, error) {
	c.blocked = true
	in, err := c.source.Account(ctx)
	if err != nil {
		return c.Current(), err
	}
	in = cloneInput(in)
	in.Now, in.MaxAge = c.now(), c.params.TruthMaxAge
	in.Selected, in.Previous = append([]string(nil), selected...), c.plan
	in.FrozenCaps, in.AggregateCap = cloneCaps(c.state.Snapshot.FrozenCaps), c.state.Snapshot.AggregateCap
	resolved := map[string]bool{}
	for _, r := range in.Resolved {
		if fresh(r.Proof, in) {
			resolved[r.ID] = true
		}
	}
	seen := map[string]bool{}
	for _, order := range in.Orders {
		seen[order.ID] = true
	}
	for _, order := range c.state.Snapshot.Orders {
		if order.Ownership == Owned && !seen[order.ID] && !resolved[order.ID] {
			in.Orders = append(in.Orders, order)
		}
	}
	want, foreign := map[string]bool{}, map[string]bool{}
	for ticker, q := range in.Positions {
		if q != 0 {
			want[ticker] = true
		}
	}
	for _, order := range in.Orders {
		if order.Ownership == Owned {
			want[order.Ticker] = true
		}
		if order.Ownership == Foreign {
			foreign[order.Ticker] = true
			want[order.Ticker] = true
		}
	}
	for _, ticker := range selected {
		if !foreign[ticker] {
			want[ticker] = true
		}
	}
	books, schedules := map[string]Book{}, map[string]Schedule{}
	in.Identities, in.Cash = nil, map[Shard]Cash{}
	for _, ticker := range keys(want) {
		obs, err := c.source.Market(ctx, ticker)
		if err != nil {
			return c.Current(), err
		}
		if obs.Identity.Ticker != ticker || obs.Book.Ticker != ticker || obs.Schedule.Ticker != ticker {
			return c.Current(), fmt.Errorf("market observation identity mismatch")
		}
		in.Identities = append(in.Identities, obs.Identity)
		if old, ok := in.Cash[obs.Identity.Shard]; ok && old.Proof.At == obs.Cash.Proof.At && old.Available != obs.Cash.Available {
			return c.Current(), fmt.Errorf("conflicting same-time scoped cash")
		}
		if old, ok := in.Cash[obs.Identity.Shard]; !ok || obs.Cash.Proof.At >= old.Proof.At {
			in.Cash[obs.Identity.Shard] = obs.Cash
		}
		books[ticker], schedules[ticker] = obs.Book, obs.Schedule
	}
	in.Now = c.now()
	plan, err := Build(in)
	if err != nil {
		return c.Current(), err
	}
	for _, m := range plan.Markets {
		if !validObservation(books[m.Ticker], schedules[m.Ticker], in) {
			return c.Current(), fmt.Errorf("missing/stale book or schedule for %s", m.Ticker)
		}
	}
	ledger := map[string]bool{}
	for _, ticker := range c.state.LedgerMarkets {
		ledger[ticker] = true
	}
	for ticker := range in.Positions {
		ledger[ticker] = true
	}
	for _, order := range in.Orders {
		if order.Ownership == Owned {
			ledger[order.Ticker] = true
		}
	}
	snapshot := cloneInput(in)
	snapshot.Previous = nil
	if plan.AddingStopped {
		c.state.StopAdding = true
	}
	next := State{Version: 1, Snapshot: snapshot, StopAdding: c.state.StopAdding, LedgerMarkets: keys(ledger)}
	if err := c.store.Replace(ctx, cloneState(next)); err != nil {
		return c.Current(), fmt.Errorf("persist turnover observation: %w", err)
	}
	c.state, c.plan, c.books, c.schedules, c.blocked = next, plan, books, schedules, false
	return c.Current(), nil
}

func validObservation(book Book, schedule Schedule, in Input) bool {
	return fresh(book.Proof, in) && fresh(schedule.Proof, in) && book.Mark4 > 0 && book.Mark4 < risk.SettlementPrice4
}

type Intent struct {
	Order Order
	Role  quote.Role
	// The account owner supplies its current lifecycle adding authority.
	AddingAuthorized bool
}

// Admit durably reserves a proposed immutable order body. This is accounting
// admission only; it is never a write permit. Retries use the existing owner's
// same-coid path rather than obtaining a second reservation here.
func (c *Coordinator) Admit(ctx context.Context, intent Intent) error {
	if c.blocked || c.plan == nil {
		return fmt.Errorf("turnover observation/persistence not ready")
	}
	in := cloneInput(c.state.Snapshot)
	in.Now, in.Previous = c.now(), c.plan
	current, err := Build(in)
	if err != nil {
		c.blocked = true
		return err
	}
	for _, m := range current.Markets {
		if !validObservation(c.books[m.Ticker], c.schedules[m.Ticker], in) {
			return fmt.Errorf("stale book/schedule")
		}
	}
	order := intent.Order
	if order.Ownership != Owned || (intent.Role != quote.RoleAdding && intent.Role != quote.RoleReducing) {
		return fmt.Errorf("invalid reservation ownership/role")
	}
	for _, old := range in.Orders {
		if old.ID == order.ID {
			return fmt.Errorf("identity already reserved")
		}
	}
	var market *Market
	for i := range current.Markets {
		if current.Markets[i].Ticker == order.Ticker {
			market = &current.Markets[i]
		}
	}
	if market == nil {
		return fmt.Errorf("unmanaged reservation market")
	}
	q := in.Positions[order.Ticker]
	side, held := quote.ReducingSide(q)
	schedule := c.schedules[order.Ticker]
	params := c.params
	params.CapitalMax = current.CapitalLimit
	exposures, foreignReserved, err := foreignCommitments(in, current.Exposures)
	if err != nil {
		return err
	}
	var budget num.Money
	if intent.Role == quote.RoleReducing {
		if !held || order.Side != side || in.Now >= schedule.ReduceUntil {
			return fmt.Errorf("reducer has no current inventory/schedule authority")
		}
		remaining := q.Abs()
		for _, old := range current.Owned {
			if old.Ticker == order.Ticker && old.Side == side {
				if old.Quantity > remaining {
					return fmt.Errorf("existing reducer exceeds inventory")
				}
				remaining -= old.Quantity
			}
		}
		if order.Quantity <= 0 || order.Quantity > remaining {
			return fmt.Errorf("reducer exceeds unreserved inventory")
		}
		budget, _ = risk.ReducingBudget(exposures, params)
	} else {
		if !intent.AddingAuthorized || c.state.StopAdding || current.AddingStopped || !market.Selected || market.State == quote.Reducing || in.Now >= schedule.AddUntil {
			return fmt.Errorf("adding authority blocked")
		}
		need, err := reducerNeed(in, current.Owned)
		if err != nil {
			return err
		}
		budget = risk.AddingBudget(order.Ticker, exposures, need, params)
	}
	value, err := cost(order.Quantity, order.Price4)
	if err != nil {
		return err
	}
	for _, shard := range current.Shards {
		if shard.Shard == market.Shard {
			budget = min(budget, max(num.Money(0), shard.PlacementLimit-foreignReserved[market.Shard]))
		}
	}
	if value > budget {
		return fmt.Errorf("reservation exceeds aggregate or scoped budget")
	}
	in.Orders = append(in.Orders, order)
	nextPlan, err := Build(in)
	if err != nil {
		return err
	}
	snapshot := cloneInput(in)
	snapshot.Previous = nil
	next := cloneState(c.state)
	next.Snapshot = snapshot
	if err := c.store.Replace(ctx, cloneState(next)); err != nil {
		// Save may have committed despite its error. Retain the attempted
		// commitment locally until positive resolution; never dispatch it.
		c.state.Snapshot.Orders = append(c.state.Snapshot.Orders, order)
		c.blocked = true
		return fmt.Errorf("persist reservation: %w", err)
	}
	c.state, c.plan = next, nextPlan
	return nil
}

func reducerNeed(in Input, orders []Order) (num.Money, error) {
	var need num.Money
	for ticker, q := range in.Positions {
		side, held := quote.ReducingSide(q)
		if !held {
			continue
		}
		remaining := q.Abs()
		for _, order := range orders {
			if order.Ticker == ticker && order.Side == side {
				remaining -= min(remaining, order.Quantity)
			}
		}
		value, err := cost(remaining, risk.SettlementPrice4)
		if err != nil {
			return 0, err
		}
		need, err = add(need, value)
		if err != nil {
			return 0, err
		}
	}
	return need, nil
}

// CancelCandidates proposes only owned adding-side identities that should
// retire. No foreign order or quantity-capped reducer is a cancel candidate.
func (c *Coordinator) CancelCandidates() []Order {
	if c.plan == nil {
		return nil
	}
	markets := map[string]Market{}
	for _, market := range c.plan.Markets {
		markets[market.Ticker] = market
	}
	var out []Order
	for _, order := range c.plan.Owned {
		side, held := quote.ReducingSide(c.state.Snapshot.Positions[order.Ticker])
		if held && side == order.Side {
			continue
		}
		market := markets[order.Ticker]
		if c.blocked || c.state.StopAdding || !market.Selected || market.State == quote.Reducing {
			out = append(out, order)
		}
	}
	return out
}

// PnLInputs includes all held/observed historical markets plus ledgerTickers
// from the existing durable fill ledger. It does not compute or reset P&L.
func (c *Coordinator) PnLInputs(ledgerTickers []string) ([]risk.PnLInput, error) {
	if c.blocked || c.plan == nil {
		return nil, fmt.Errorf("portfolio truth is not ready")
	}
	check := cloneInput(c.state.Snapshot)
	check.Now = c.now()
	check.Previous = c.plan
	if _, err := Build(check); err != nil {
		return nil, err
	}
	tickers := map[string]bool{}
	for _, ticker := range c.state.LedgerMarkets {
		tickers[ticker] = true
	}
	for _, ticker := range ledgerTickers {
		tickers[ticker] = true
	}
	in := c.state.Snapshot
	in.Now = c.now()
	var out []risk.PnLInput
	for _, ticker := range keys(tickers) {
		p := risk.PnLInput{Ticker: ticker, QExch: in.Positions[ticker]}
		book := c.books[ticker]
		if p.QExch != 0 && !c.blocked && fresh(book.Proof, in) {
			p.Mark4, p.MarkOK = book.Mark4, book.Mark4 > 0 && book.Mark4 < risk.SettlementPrice4
		}
		out = append(out, p)
	}
	return out, nil
}

// Current returns a defensive copy of the last successfully persisted plan.
func (c *Coordinator) Current() *Plan {
	if c.plan == nil {
		return nil
	}
	out := *c.plan
	out.Markets = append([]Market(nil), out.Markets...)
	out.Owned = append([]Order(nil), out.Owned...)
	out.Foreign = append([]Order(nil), out.Foreign...)
	out.Excluded = append([]string(nil), out.Excluded...)
	out.Retired = append([]string(nil), out.Retired...)
	out.Shards = append([]ShardPlan(nil), out.Shards...)
	out.Exposures = append([]risk.Exposure(nil), out.Exposures...)
	out.frozenCaps = cloneCaps(out.frozenCaps)
	return &out
}

func cloneCaps(in map[Shard]num.Money) map[Shard]num.Money {
	out := make(map[Shard]num.Money, len(in))
	for s, cap := range in {
		out[s] = cap
	}
	return out
}

func sameCaps(a, b map[Shard]num.Money) bool {
	if len(a) != len(b) {
		return false
	}
	for s, v := range a {
		if n, ok := b[s]; !ok || n != v {
			return false
		}
	}
	return true
}

func cloneInput(in Input) Input {
	out := in
	out.Selected = append([]string(nil), in.Selected...)
	out.Orders = append([]Order(nil), in.Orders...)
	out.Resolved = append([]Resolution(nil), in.Resolved...)
	out.Identities = append([]Identity(nil), in.Identities...)
	out.Positions = make(map[string]num.Qty, len(in.Positions))
	for k, v := range in.Positions {
		out.Positions[k] = v
	}
	out.Cash = make(map[Shard]Cash, len(in.Cash))
	for k, v := range in.Cash {
		out.Cash[k] = v
	}
	out.FrozenCaps = cloneCaps(in.FrozenCaps)
	return out
}

func cloneState(in State) State {
	in.Snapshot = cloneInput(in.Snapshot)
	in.LedgerMarkets = append([]string(nil), in.LedgerMarkets...)
	sort.Strings(in.LedgerMarkets)
	return in
}

// Foreign orders never enter our cancellation set, but their collateral is
// conservatively withheld again even when authenticated cash already excludes
// it. The extra reservation cannot create borrowing across shards.
func foreignCommitments(in Input, owned []risk.Exposure) ([]risk.Exposure, map[Shard]num.Money, error) {
	ex := append([]risk.Exposure(nil), owned...)
	shards := map[string]Shard{}
	for _, identity := range in.Identities {
		shards[identity.Ticker] = identity.Shard
	}
	reserved := map[Shard]num.Money{}
	var total num.Money
	for _, e := range owned {
		var err error
		total, err = add(total, e.Total())
		if err != nil {
			return nil, nil, err
		}
	}
	for _, order := range in.Orders {
		if order.Ownership != Foreign {
			continue
		}
		shard, ok := shards[order.Ticker]
		if !ok {
			return nil, nil, fmt.Errorf("foreign commitment has no shard identity")
		}
		value, err := cost(order.Quantity, order.Price4)
		if err != nil {
			return nil, nil, err
		}
		total, err = add(total, value)
		if err != nil {
			return nil, nil, err
		}
		reserved[shard], err = add(reserved[shard], value)
		if err != nil {
			return nil, nil, err
		}
		ex = append(ex, risk.Exposure{Ticker: order.Ticker, Adding: value})
	}
	return ex, reserved, nil
}

// confidence: high
