package turnover

import (
	"math"
	"reflect"
	"testing"
	"time"

	"lip/harness/num"
	"lip/harness/quote"
)

func dollars(n int64) num.Money { return num.Money(n * num.MoneyScale) }

func fixture() Input {
	p := Proof{Complete: true, At: 10 * time.Second}
	a, b := Shard{ExchangeIndex: 1}, Shard{ExchangeIndex: 2}
	return Input{
		Now: 10 * time.Second, MaxAge: time.Second, Selected: []string{"B"},
		Positions:      map[string]num.Qty{"A": 200},
		PositionsProof: p, OrdersProof: p, OwnershipProof: p, PendingProof: p,
		Identities: []Identity{{Ticker: "A", Shard: a, Proof: p}, {Ticker: "B", Shard: b, Proof: p}},
		Cash:       map[Shard]Cash{a: {Available: dollars(10), Proof: p}, b: {Available: dollars(20), Proof: p}},
		FrozenCaps: map[Shard]num.Money{a: dollars(10), b: dollars(20)}, AggregateCap: dollars(30),
	}
}

func mustBuild(t *testing.T, in Input) *Plan {
	t.Helper()
	p, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestartAndRotationRetainOldInventory(t *testing.T) {
	in := fixture()
	in.Selected = []string{"A"}
	old := mustBuild(t, in)
	in.Previous, in.Selected = old, []string{"B"}
	p := mustBuild(t, in)
	if len(p.Markets) != 2 || p.Markets[0].Ticker != "A" || p.Markets[0].Selected || p.Markets[0].State != quote.Reducing || !p.Markets[1].Selected {
		t.Fatalf("rotation lost reducer or selection: %+v", p.Markets)
	}
	// A restart reconstructs the same management from complete account truth.
	in.Previous = nil
	restarted := mustBuild(t, in)
	if !reflect.DeepEqual(restarted.Markets, p.Markets) {
		t.Fatal("restart changed management")
	}
	if p.Committed != dollars(2) || len(p.Exposures) != 2 {
		t.Fatalf("aggregate: %+v", p)
	}
}

func TestUnknownRetainsFlatMarketUntilPositiveResolution(t *testing.T) {
	in := fixture()
	in.Positions = map[string]num.Qty{}
	in.Orders = []Order{{ID: "unknown-A", Ticker: "A", Side: quote.SideYes, Quantity: 100, Price4: 4000, Ownership: Owned}}
	old := mustBuild(t, in)
	if len(old.Markets) != 2 || old.Committed != 400000 {
		t.Fatalf("unknown not reserved: %+v", old)
	}
	in.Previous, in.Orders = old, nil
	if p, err := Build(in); err == nil || p != nil {
		t.Fatal("empty complete walk resolved UNKNOWN")
	}
	in.Resolved = []Resolution{{ID: "unknown-A", Proof: in.OrdersProof}}
	p := mustBuild(t, in)
	if !reflect.DeepEqual(p.Retired, []string{"A"}) || len(p.Markets) != 1 || p.Markets[0].Ticker != "B" {
		t.Fatalf("resolved flat A not retired: %+v", p)
	}
}

func TestForeignExclusionNeverBecomesOwned(t *testing.T) {
	in := fixture()
	in.Selected = []string{"A", "B", "C"}
	in.Orders = []Order{
		{ID: "foreign-A", Ticker: "A", Side: quote.SideYes, Quantity: 100, Price4: 5000, Ownership: Foreign},
		{ID: "foreign-C", Ticker: "C", Side: quote.SideNo, Quantity: 100, Price4: 5000, Ownership: Foreign},
	}
	p := mustBuild(t, in)
	if !p.AddingStopped || len(p.Owned) != 0 || len(p.Foreign) != 2 || len(p.Markets) != 2 || p.Markets[0].Selected || p.Markets[0].State != quote.Reducing {
		t.Fatalf("foreign activity altered owned obligations: %+v", p)
	}
	if p.Committed != dollars(2) || !reflect.DeepEqual(p.Excluded, []string{"A", "C"}) {
		t.Fatalf("foreign accounting: %+v", p)
	}
	// Output is detached from the caller's order memory.
	in.Orders[0].Ticker = "mutated"
	if p.Foreign[0].Ticker != "A" {
		t.Fatal("plan aliases orders")
	}
}

func TestShardCashAndCapsAreCountedOnce(t *testing.T) {
	in := fixture()
	a := in.Identities[0].Shard
	in.Identities[1].Shard = a
	in.Orders = []Order{
		{ID: "reduce-A", Ticker: "A", Side: quote.SideNo, Quantity: 100, Price4: 4000, Ownership: Owned},
		{ID: "add-B", Ticker: "B", Side: quote.SideYes, Quantity: 100, Price4: 6000, Ownership: Owned},
	}
	p := mustBuild(t, in)
	if len(p.Shards) != 1 || p.CapitalLimit != dollars(10) || p.Committed != dollars(3) || p.Shards[0].OrderReserved != dollars(1) || p.Shards[0].PlacementLimit != dollars(7) {
		t.Fatalf("shared shard duplicated funds: %+v", p)
	}
	if p.Exposures[0].Reducing != 400000 || p.Exposures[1].Adding != 600000 {
		t.Fatalf("roles: %+v", p.Exposures)
	}
}

func TestDifferentShardsCannotSpendEachOthersCash(t *testing.T) {
	in := fixture()
	a := in.Identities[0].Shard
	f := in.Cash[a]
	f.Available = 100000
	in.Cash[a] = f
	in.Orders = []Order{{ID: "reduce-A", Ticker: "A", Side: quote.SideNo, Quantity: 100, Price4: 4000, Ownership: Owned}}
	p := mustBuild(t, in)
	if !p.AddingStopped || p.Shards[0].PlacementLimit != 0 || p.Shards[1].PlacementLimit != dollars(20) || p.Committed != 2400000 {
		t.Fatalf("cross-shard funding: %+v", p)
	}
}

func TestMalformedOrIncompleteEvidenceFailsClosed(t *testing.T) {
	cases := map[string]func(*Input){
		"positions incomplete": func(in *Input) { in.PositionsProof.Complete = false },
		"orders incomplete":    func(in *Input) { in.OrdersProof.Complete = false },
		"ownership incomplete": func(in *Input) { in.OwnershipProof.Complete = false },
		"pending incomplete":   func(in *Input) { in.PendingProof.Complete = false },
		"old truth":            func(in *Input) { in.PositionsProof.At = 0 },
		"future truth":         func(in *Input) { in.OrdersProof.At = in.Now + 1 },
		"missing identity":     func(in *Input) { in.Identities = in.Identities[1:] },
		"conflicting identity": func(in *Input) {
			id := in.Identities[0]
			id.Shard.ExchangeIndex++
			in.Identities = append(in.Identities, id)
		},
		"stale identity":    func(in *Input) { in.Identities[0].Proof.At = 0 },
		"missing cash":      func(in *Input) { delete(in.Cash, in.Identities[0].Shard) },
		"stale cash":        func(in *Input) { s := in.Identities[0].Shard; f := in.Cash[s]; f.Proof.At = 0; in.Cash[s] = f },
		"missing cap":       func(in *Input) { delete(in.FrozenCaps, in.Identities[0].Shard) },
		"position overflow": func(in *Input) { in.Positions["A"] = num.Qty(math.MinInt64) },
		"total overflow": func(in *Input) {
			for s := range in.FrozenCaps {
				in.FrozenCaps[s] = num.Money(math.MaxInt64)
			}
		},
		"unclassified order": func(in *Input) {
			in.Orders = []Order{{ID: "x", Ticker: "A", Side: quote.SideYes, Quantity: 100, Price4: 5000}}
		},
		"duplicate identity": func(in *Input) {
			o := Order{ID: "x", Ticker: "A", Side: quote.SideYes, Quantity: 100, Price4: 5000, Ownership: Owned}
			in.Orders = []Order{o, o}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			in := fixture()
			change(&in)
			if p, err := Build(in); err == nil || p != nil {
				t.Fatalf("accepted invalid evidence: %+v", p)
			}
		})
	}
}

func TestFrozenCapsAndShardIdentityCannotChange(t *testing.T) {
	for _, kind := range []string{"cap", "aggregate", "shard"} {
		t.Run(kind, func(t *testing.T) {
			in := fixture()
			in.Previous = mustBuild(t, in)
			switch kind {
			case "cap":
				in.FrozenCaps[in.Identities[0].Shard] += dollars(1)
			case "aggregate":
				in.AggregateCap += dollars(1)
			case "shard":
				in.Identities[0].Shard = in.Identities[1].Shard
			}
			if p, err := Build(in); err == nil || p != nil {
				t.Fatal("accepted changed run authority")
			}
		})
	}
}

func TestRetirementRequiresTruthAtLeastAsRecentAsPreviousPlan(t *testing.T) {
	in := fixture()
	in.Previous = mustBuild(t, in)
	in.Positions = map[string]num.Qty{}
	in.PositionsProof.At--
	if p, err := Build(in); err == nil || p != nil {
		t.Fatal("retired using an older flat observation")
	}
}

func TestAggregateCapStillConstrainsSeparateShards(t *testing.T) {
	in := fixture()
	in.Positions["B"] = 400
	in.AggregateCap = dollars(5)
	p := mustBuild(t, in)
	if p.CapitalLimit != dollars(5) || p.Committed != dollars(6) || !p.AddingStopped || len(p.Markets) != 2 {
		t.Fatalf("aggregate cap lost recovery obligations: %+v", p)
	}
}
