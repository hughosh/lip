// Package turnover builds account-wide management plans. A plan is accounting
// evidence, not placement authority: book, schedule, lifecycle and dispatch
// guards must still approve every request.
package turnover

import (
	"fmt"
	"math"
	"sort"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
	"lip/harness/risk"
)

type Shard struct {
	ExchangeIndex int
	Subaccount    int
}

// Proof timestamps are monotonic completion times in the caller's clock domain.
// Complete means an authoritative full observation, never a partial page.
type Proof struct {
	Complete bool
	At       time.Duration
}

type Ownership uint8

const (
	Unclassified Ownership = iota
	Owned
	Foreign
)

// Order is one normalized possibly-live commitment. ID must identify the same
// order across REST, sending, UNKNOWN and cancel-pending representations (use
// the durable coid for owned orders). The caller supplies the conservative
// maximum remaining quantity once, not a row per representation.
type Order struct {
	ID        string
	Ticker    string
	Side      quote.Side
	Quantity  num.Qty
	Price4    int64
	Ownership Ownership
}

type Identity struct {
	Ticker string
	Shard  Shard
	Proof  Proof
}

type Cash struct {
	Available num.Money
	Proof     Proof
}

// Resolution is positive terminal evidence for an owned identity. In
// particular, absence from a complete resting-orders walk cannot resolve an
// UNKNOWN create. The caller must obtain this proof from the reservation/order
// lifecycle, not synthesize it from an omitted row.
type Resolution struct {
	ID    string
	Proof Proof
}

type Input struct {
	Now, MaxAge time.Duration
	Selected    []string
	Positions   map[string]num.Qty
	Orders      []Order
	// These proofs cover account-wide truth, ownership classification, and
	// the complete local sending/UNKNOWN/cancel-pending reservation inventory.
	PositionsProof, OrdersProof, OwnershipProof, PendingProof Proof
	Identities                                                []Identity
	Cash                                                      map[Shard]Cash
	// FrozenCaps and AggregateCap are immutable run limits. Cash refreshes
	// cannot increase them. Previous enforces this across plan revisions.
	FrozenCaps   map[Shard]num.Money
	AggregateCap num.Money
	Previous     *Plan
	Resolved     []Resolution
}

type Market struct {
	Ticker   string
	Shard    Shard
	Selected bool // eligible selection; foreign exclusions already removed
	State    quote.MarketState
	Exposure risk.Exposure
}

type ShardPlan struct {
	Shard          Shard
	FrozenCap      num.Money
	Cash           num.Money
	Committed      num.Money
	OrderReserved  num.Money
	PlacementLimit num.Money // min(cap - commitments, cash - orders), floored at zero
}

// Plan is immutable by convention. Build never aliases its input collections.
// Owned and Foreign are separate accounting lists, never cancellation requests.
// On any Build error callers must retain their prior managed set, block new
// placements, and continue reconciliation; a nil plan is not an empty account.
type Plan struct {
	At             time.Duration
	Markets        []Market
	Owned, Foreign []Order
	Excluded       []string
	Retired        []string
	Shards         []ShardPlan
	Exposures      []risk.Exposure
	Committed      num.Money
	CapitalLimit   num.Money
	AddingStopped  bool
	frozenCaps     map[Shard]num.Money
	runCap         num.Money
}

func fresh(p Proof, in Input) bool {
	return p.Complete && p.At >= 0 && p.At <= in.Now && in.Now-p.At <= in.MaxAge
}

func validShard(s Shard) bool { return s.ExchangeIndex >= 0 && s.Subaccount >= 0 }

func add(a, b num.Money) (num.Money, error) {
	if a < 0 || b < 0 || int64(b) > math.MaxInt64-int64(a) {
		return 0, fmt.Errorf("collateral overflow or negative amount")
	}
	return a + b, nil
}

func cost(q num.Qty, price int64) (num.Money, error) {
	if q == num.Qty(math.MinInt64) || price < 0 {
		return 0, fmt.Errorf("invalid collateral quantity or price")
	}
	q = q.Abs()
	if price != 0 && int64(q) > math.MaxInt64/price {
		return 0, fmt.Errorf("collateral overflow")
	}
	return risk.SideCost(q, price), nil
}

// Build retains inherited exposure independently of current selection. Every
// disappearing prior owned identity requires positive resolution, and retirement
// additionally requires fresh complete positions/orders/local-reservation truth.
func Build(in Input) (*Plan, error) {
	if in.Now < 0 || in.MaxAge <= 0 || in.AggregateCap <= 0 {
		return nil, fmt.Errorf("invalid clock bounds or aggregate cap")
	}
	for _, p := range []Proof{in.PositionsProof, in.OrdersProof, in.OwnershipProof, in.PendingProof} {
		if !fresh(p, in) {
			return nil, fmt.Errorf("incomplete, stale or future account/ownership/reservation truth")
		}
	}
	out := &Plan{At: in.Now, CapitalLimit: in.AggregateCap, runCap: in.AggregateCap, frozenCaps: make(map[Shard]num.Money)}
	for shard, cap := range in.FrozenCaps {
		if !validShard(shard) || cap < 0 {
			return nil, fmt.Errorf("invalid frozen shard cap")
		}
		out.frozenCaps[shard] = cap
	}
	if old := in.Previous; old != nil {
		if in.Now < old.At || in.AggregateCap != old.runCap || len(old.frozenCaps) != len(in.FrozenCaps) {
			return nil, fmt.Errorf("plan clock rewound or frozen run caps changed")
		}
		for s, cap := range old.frozenCaps {
			if next, ok := in.FrozenCaps[s]; !ok || next != cap {
				return nil, fmt.Errorf("frozen shard cap changed")
			}
		}
	}
	selected, managed, excluded := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, ticker := range in.Selected {
		if ticker == "" {
			return nil, fmt.Errorf("empty selected ticker")
		}
		selected[ticker] = true
	}
	orders := make(map[string]Order)
	for _, order := range in.Orders {
		if order.ID == "" || order.Ticker == "" || order.Quantity <= 0 || order.Price4 <= 0 || order.Price4 >= risk.SettlementPrice4 || (order.Side != quote.SideYes && order.Side != quote.SideNo) {
			return nil, fmt.Errorf("invalid possibly-live order")
		}
		if order.Ownership != Owned && order.Ownership != Foreign {
			return nil, fmt.Errorf("unresolved order ownership")
		}
		if _, exists := orders[order.ID]; exists {
			return nil, fmt.Errorf("duplicate order identity %s", order.ID)
		}
		orders[order.ID] = order
		if order.Ownership == Foreign {
			out.Foreign = append(out.Foreign, order)
			excluded[order.Ticker] = true
			out.AddingStopped = true
		} else {
			out.Owned = append(out.Owned, order)
			managed[order.Ticker] = true
		}
	}
	resolved := make(map[string]Proof)
	for _, r := range in.Resolved {
		if r.ID == "" || !fresh(r.Proof, in) {
			return nil, fmt.Errorf("invalid owned-order resolution")
		}
		if _, exists := orders[r.ID]; exists {
			return nil, fmt.Errorf("resolved order is still possibly live")
		}
		resolved[r.ID] = r.Proof
	}
	if old := in.Previous; old != nil {
		for _, before := range old.Owned {
			if current, exists := orders[before.ID]; exists {
				if current.Ownership != Owned || current.Ticker != before.Ticker || current.Side != before.Side || current.Price4 != before.Price4 {
					return nil, fmt.Errorf("owned order identity changed")
				}
			} else if proof, ok := resolved[before.ID]; !ok || proof.At < old.At {
				return nil, fmt.Errorf("owned order %s disappeared without positive resolution", before.ID)
			}
		}
	}
	for ticker, q := range in.Positions {
		if ticker == "" {
			return nil, fmt.Errorf("empty position ticker")
		}
		if q != 0 {
			managed[ticker] = true
		}
	}
	for ticker := range selected {
		if !excluded[ticker] {
			managed[ticker] = true
		}
	}
	identities := make(map[string]Shard)
	for _, identity := range in.Identities {
		if identity.Ticker == "" || !validShard(identity.Shard) || !fresh(identity.Proof, in) {
			return nil, fmt.Errorf("invalid or stale market shard identity")
		}
		if previous, ok := identities[identity.Ticker]; ok && previous != identity.Shard {
			return nil, fmt.Errorf("conflicting market shard identity")
		}
		identities[identity.Ticker] = identity.Shard
	}
	if old := in.Previous; old != nil {
		for _, market := range old.Markets {
			if managed[market.Ticker] {
				if shard, ok := identities[market.Ticker]; !ok || shard != market.Shard {
					return nil, fmt.Errorf("managed market shard identity changed")
				}
			} else {
				if in.PositionsProof.At < old.At || in.OrdersProof.At < old.At || in.PendingProof.At < old.At {
					return nil, fmt.Errorf("retirement truth predates prior plan")
				}
				out.Retired = append(out.Retired, market.Ticker)
			}
		}
	}
	shards := make(map[Shard]*ShardPlan)
	var capTotal num.Money
	for _, ticker := range keys(managed) {
		shard, ok := identities[ticker]
		if !ok {
			return nil, fmt.Errorf("missing shard identity for %s", ticker)
		}
		sp := shards[shard]
		if sp == nil {
			cash, ok := in.Cash[shard]
			cap, admitted := in.FrozenCaps[shard]
			if !ok || !admitted || cash.Available < 0 || !fresh(cash.Proof, in) {
				return nil, fmt.Errorf("missing or stale funded shard for %s", ticker)
			}
			sp = &ShardPlan{Shard: shard, FrozenCap: cap, Cash: cash.Available}
			shards[shard] = sp
			var err error
			capTotal, err = add(capTotal, cap)
			if err != nil {
				return nil, err
			}
		}
		exposure := risk.Exposure{Ticker: ticker}
		var err error
		exposure.Position, err = cost(in.Positions[ticker], risk.SettlementPrice4)
		if err != nil {
			return nil, err
		}
		for _, order := range out.Owned {
			if order.Ticker != ticker {
				continue
			}
			value, err := cost(order.Quantity, order.Price4)
			if err != nil {
				return nil, err
			}
			side, held := quote.ReducingSide(in.Positions[ticker])
			if held && order.Side == side {
				exposure.Reducing, err = add(exposure.Reducing, value)
			} else {
				exposure.Adding, err = add(exposure.Adding, value)
			}
			if err != nil {
				return nil, err
			}
			sp.OrderReserved, err = add(sp.OrderReserved, value)
			if err != nil {
				return nil, err
			}
		}
		total, err := add(exposure.Position, exposure.Adding)
		if err != nil {
			return nil, err
		}
		total, err = add(total, exposure.Reducing)
		if err != nil {
			return nil, err
		}
		sp.Committed, err = add(sp.Committed, total)
		if err != nil {
			return nil, err
		}
		out.Committed, err = add(out.Committed, total)
		if err != nil {
			return nil, err
		}
		state := quote.Idle
		if in.Positions[ticker] != 0 {
			state = quote.Reducing
		}
		out.Markets = append(out.Markets, Market{Ticker: ticker, Shard: shard, Selected: selected[ticker] && !excluded[ticker], State: state, Exposure: exposure})
		out.Exposures = append(out.Exposures, exposure)
	}
	if capTotal < out.CapitalLimit {
		out.CapitalLimit = capTotal
	}
	for _, sp := range shards {
		sp.PlacementLimit = min(sp.FrozenCap-sp.Committed, sp.Cash-sp.OrderReserved)
		if sp.PlacementLimit < 0 {
			sp.PlacementLimit = 0
		}
		if sp.Committed > sp.FrozenCap || sp.OrderReserved > sp.Cash {
			out.AddingStopped = true
		}
		out.Shards = append(out.Shards, *sp)
	}
	if out.Committed > out.CapitalLimit {
		out.AddingStopped = true
	}
	out.Excluded = keys(excluded)
	sort.Strings(out.Retired)
	sort.Slice(out.Shards, func(i, j int) bool {
		a, b := out.Shards[i].Shard, out.Shards[j].Shard
		if a.ExchangeIndex != b.ExchangeIndex {
			return a.ExchangeIndex < b.ExchangeIndex
		}
		return a.Subaccount < b.Subaccount
	})
	for _, orders := range [][]Order{out.Owned, out.Foreign} {
		sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	}
	return out, nil
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// confidence: high
