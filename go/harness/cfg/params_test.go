package cfg

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"lip/harness/num"
)

// spec16 is harness-spec.md §16's table, transcribed by field rather than by
// row so that the reflection sweep below can prove the transcription is
// COMPLETE and not merely correct where it was looked at.
//
// Every expected value is a raw literal in the target type's own units -- not
// num.QtyFromFloat(12) or num.MoneyFromDollars(100). Default() calls those
// helpers, and an expectation that calls them too would agree with the code
// under test about any conversion error rather than detect it. §16 states
// contracts and dollars; num.QtyScale and num.MoneyScale fix the quanta; the
// literals below are that arithmetic done by hand, once.
var spec16 = map[string]any{
	// --- selection and capital (§10) ---
	"NMarkets":       6,                // n_markets       §10.3
	"CapitalMax":     num.Money(100e6), // capital_max     100 USD
	"CapitalReserve": 0.25,             // capital_reserve
	"Concentration":  2.0,              // concentration
	"S":              num.Qty(1200),    // S               12 contracts
	"SMax":           num.Qty(4800),    // S_max           48 = 4·S
	"InvSoft":        num.Qty(300),     // inv_soft         3 = 0.25·S
	"InvHard":        num.Qty(700),     // inv_hard         7 = 0.6·S
	"InvKill":        num.Qty(1800),    // inv_kill        18 = 1.5·S
	"PnLKill":        num.Money(-15e6), // pnl_kill        -15 USD
	// --- requote policy (§6.5) and the write queue (§6.6) ---
	"Debounce":        250 * time.Millisecond, // debounce_s          0.250
	"RequoteInterval": 5 * time.Second,        // requote_interval_s  5.0
	"StaleBidTicks":   8,                      // stale_bid_ticks
	"MaxQueueAge":     30 * time.Second,       // max_queue_age
	"WriteRate":       5.0,                    // write_rate
	"WriteBurst":      10,                     // write_burst
	// --- polling and freshness (§8, §9, §11) ---
	"PositionPoll":     5 * time.Second,    // position_poll_s
	"BalancePoll":      60 * time.Second,   // balance_poll_s
	"SchedulePoll":     30 * time.Second,   // schedule_poll_s
	"GateFailDebounce": 30 * time.Second,   // gate_fail_debounce_s
	"OwnerStall":       3 * time.Second,    // owner_stall_s
	"TruthMaxAge":      60 * time.Second,   // truth_max_age_s
	"PnLMarkMaxAge":    30 * time.Second,   // pnl_mark_max_age_s
	"Reselect":         3600 * time.Second, // reselect_s
	"Hysteresis":       0.30,               // hysteresis
	"PosDriftTol":      num.Qty(1),         // pos_drift_tol   0.01 contracts
	"PosDriftHard":     num.Qty(500),       // pos_drift_hard  5 contracts
	// --- transport (F1) ---
	"WSPingInterval":   10 * time.Second, // ping_interval_s
	"PongTimeout":      5 * time.Second,  // pong_timeout_s
	"ReadDeadline":     60 * time.Second, // read_deadline_s
	"Quiet":            60 * time.Second, // quiet_s
	"DisconnectReduce": 60 * time.Second, // disconnect_halt_s, RENAMED per H-HALT-1
	"Stuck":            1800 * time.Second,
	// --- order lifecycle (§7) ---
	"UnknownResolve":   10 * time.Second,  // unknown_resolve_s
	"RetrySameCoidMax": 3,                 // retry_same_coid_max
	"UnknownPing":      120 * time.Second, // unknown_ping_s
	// --- close handling (§9) ---
	"CloseLead":             1 * time.Hour,       // close_lead   1h, was 4h
	"FinalLead":             60 * time.Second,    // final_lead
	"CloseLeadKeepReducing": true,                // close_lead_keep_reducing
	"MaxTenor":              30 * 24 * time.Hour, // max_tenor_d  30 days
	// --- lifecycle and reporting ---
	"DrainTimeout": 12 * time.Hour,     // drain_timeout_h
	"Heartbeat":    3600 * time.Second, // heartbeat_s
	"Backfill":     24 * time.Hour,     // backfill_h
}

// TestDefaultMatchesSpec16 is V1's §16 transcription check.
//
// §15 requires the `run` row to record "every parameter in §16, verbatim", and
// §10.3 rescaled ten of them from a $500 configuration to a $100 one. A run
// whose configuration does not match the document is a run whose evidence
// cannot be interpreted afterwards -- and, for the four inventory knobs, a run
// whose risk ladder is not the one anybody reviewed.
//
// The sweep is over the struct's own fields, so a parameter added to Params
// without a line in this table fails here rather than going unchecked.
func TestDefaultMatchesSpec16(t *testing.T) {
	got := Default()
	v := reflect.ValueOf(got)
	ty := v.Type()

	seen := make(map[string]bool, len(spec16))
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		want, ok := spec16[name]
		if !ok {
			t.Errorf("Params.%s has no expected value in this table: either it "+
				"is a §16 parameter that nothing pins, or it is a knob that "+
				"never reaches the `run` row §15 requires", name)
			continue
		}
		seen[name] = true

		wv := reflect.ValueOf(want)
		if wv.Type() != ty.Field(i).Type {
			t.Errorf("Params.%s is %v but the table expects %v -- the units "+
				"differ, so the comparison below would be meaningless",
				name, ty.Field(i).Type, wv.Type())
			continue
		}
		if !reflect.DeepEqual(v.Field(i).Interface(), want) {
			t.Errorf("Params.%s = %v, §16 says %v", name, v.Field(i).Interface(), want)
		}
	}
	for name := range spec16 {
		if !seen[name] {
			t.Errorf("this table pins %q, which is not a field of Params -- a "+
				"renamed parameter silently stops being checked", name)
		}
	}
	if n := ty.NumField(); n != len(spec16) {
		t.Errorf("Params has %d fields against %d pinned parameters", n, len(spec16))
	}
}

// TestDefaultRatiosHoldAtS pins §10.3's derivation rather than its arithmetic.
//
// "The inventory ladder and S_max keep their ratios to S (0.25 / 0.6 / 1.5 /
// 4x), because they are expressed in units of a quote, not in absolute
// contracts -- leaving inv_hard = 60 beside S = 12 would mean holding five
// quotes' worth of inventory before reducing at all."
//
// That is the failure this guards. The rescale from S = 100 to S = 12 touched
// five numbers, and any one of them left behind is not a rounding error: an
// unrescaled inv_hard defers REDUCING past the point the taper was designed
// around, and an unrescaled inv_kill defers WINDING_DOWN past that.
//
// The ladder is stated in WHOLE contracts, so the comparison is against the
// ratio rounded to a contract. Only inv_hard is actually inexact -- 0.6·12 is
// 7.2 and §16 says 7 -- and rounding it DOWN is the conservative direction:
// REDUCING begins 0.2 contracts earlier than the ratio asks, never later. The
// tolerance is half a contract rather than an epsilon so that a knob left at
// its S = 100 value cannot hide inside it: at S = 12 every unrescaled figure is
// wrong by at least 18 contracts.
func TestDefaultRatiosHoldAtS(t *testing.T) {
	p := Default()
	sContracts := p.S.Float()
	for _, tc := range []struct {
		name  string
		got   num.Qty
		ratio float64
		why   string
	}{
		{"S_max", p.SMax, 4, "the aggregate per-side cap is four quotes"},
		{"inv_soft", p.InvSoft, 0.25, "the taper begins at a quarter quote"},
		{"inv_hard", p.InvHard, 0.6, "the taper reaches zero and the market " +
			"enters REDUCING"},
		{"inv_kill", p.InvKill, 1.5, "global WINDING_DOWN (F17)"},
	} {
		exact := tc.ratio * sContracts
		want := num.Qty(int64(math.Round(exact)) * num.QtyScale)
		if tc.got != want {
			t.Errorf("%s = %s, want %s (%.2f·S = %.2f contracts at S = %s, "+
				"rounded to a whole contract) -- %s",
				tc.name, tc.got.Wire(), want.Wire(), tc.ratio, exact,
				p.S.Wire(), tc.why)
		}
		// And the rounding is to a whole contract, not a licence to drift: the
		// stated value stays within half a contract of the ratio it derives
		// from, in units of S.
		if drift := math.Abs(tc.got.Float() - exact); drift > 0.5 {
			t.Errorf("%s is %.2f contracts from %.2f·S = %.2f -- that is no "+
				"longer the same ladder", tc.name, drift, tc.ratio, exact)
		}
	}

	// The ladder is strictly increasing at these values, which is what §6.2's
	// taper needs and what Validate() enforces as a rule. Stated here as well
	// because the ratios alone do not imply it once each one is rounded.
	if !(p.InvSoft < p.InvHard && p.InvHard < p.InvKill && p.InvKill <= p.SMax) {
		t.Errorf("the inventory ladder is not increasing: inv_soft %s, "+
			"inv_hard %s, inv_kill %s, S_max %s", p.InvSoft.Wire(),
			p.InvHard.Wire(), p.InvKill.Wire(), p.SMax.Wire())
	}
}

// TestDefaultIsFundable is the lip-afr regression, and the reason §10.3 exists
// in its current form.
//
// H-CAP-8 rejects a configuration whose worst permitted simultaneous fill set
// cannot be funded. At n_markets · S · max_price with the 99c bound H-SEL-7
// permits, the previous defaults gave 6 · 100 · $0.99 = $594 against a $500
// capital_max: **the recommended opening configuration was required to reject
// itself at startup**, so no implementation could satisfy §10.3 and H-CAP-8 at
// once. It is HR-005's class -- a capital rule forbidding the configuration it
// recommends -- reached from the other direction.
//
// This is arithmetic over four §16 knobs, so it is checkable here, before
// harness/risk exists to enforce it against a live price. Any future change to
// n_markets, S, capital_max or capital_reserve that reintroduces the conflict
// fails here rather than at startup on the live account.
func TestDefaultIsFundable(t *testing.T) {
	p := Default()

	// H-SEL-7's bound: no market is entered above 99c, so 99c is the most a
	// one-sided fill of the whole quote can cost per contract.
	const maxPrice4 = 99 * 100 // 1e-4 USD units

	worst := num.Money(0)
	for i := 0; i < p.NMarkets; i++ {
		worst += num.Notional(p.S, maxPrice4)
	}
	deployable := num.Money(float64(p.CapitalMax) * (1 - p.CapitalReserve))

	if worst > deployable {
		t.Fatalf("the §10.3 opening configuration is not fundable: the worst "+
			"permitted simultaneous one-sided fill set costs %s "+
			"(%d markets · %s contracts · $0.99) against %s deployable "+
			"(%s capital_max less a %.0f%% reserve). H-CAP-8 requires the "+
			"harness to reject this at startup, so §10.3 would be recommending "+
			"a configuration it forbids",
			worst, p.NMarkets, p.S.Wire(), deployable,
			p.CapitalMax, p.CapitalReserve*100)
	}

	// The figure §10.3 states, pinned exactly. Its derivation is what makes
	// S = 12 the largest quote that fits, so a drift here means the size was
	// re-chosen rather than re-derived.
	if want := num.MoneyFromDollars(71.28); worst != want {
		t.Errorf("worst-case fill set = %s, §10.3 states %s", worst, want)
	}
	if want := num.MoneyFromDollars(75); deployable != want {
		t.Errorf("deployable capital = %s, §10.3 states %s", deployable, want)
	}
}

// TestDefaultValidates: the shipped configuration passes its own gate.
//
// Trivial to state and the single most load-bearing assertion in this file. A
// Validate() rule that Default() cannot satisfy is not caught by any of the
// rejection cases below -- every one of them starts from Default() and breaks
// one field, so all of them would still "reject" and all would pass.
func TestDefaultValidates(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default() does not satisfy Validate(): %v", err)
	}
}

// TestValidateRejects drives one broken field at a time through every relation
// §16 states as a rule.
//
// Each case asserts the error MENTIONS the parameter it is about. An assertion
// that Validate merely returned non-nil is satisfied by a validator that
// rejects for an unrelated reason, and every case here starts from a Default()
// that differs in exactly one field -- so "some error occurred" is close to
// free, and proves close to nothing.
func TestValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Params)
		want   string // substring the message must contain
		why    string
	}{
		{"no markets", func(p *Params) { p.NMarkets = 0 }, "n_markets",
			"a harness quoting nothing is a configuration error, not an idle run"},
		{"zero capital", func(p *Params) { p.CapitalMax = 0 }, "capital_max",
			"H-CAP-1's cap at zero funds nothing and would read as a permanent " +
				"insufficient-balance condition"},
		{"negative capital", func(p *Params) { p.CapitalMax = -1 }, "capital_max", ""},
		{"reserve below zero", func(p *Params) { p.CapitalReserve = -0.1 },
			"capital_reserve", "a negative reserve deploys more than capital_max"},
		{"reserve of one", func(p *Params) { p.CapitalReserve = 1 },
			"capital_reserve", "reserving everything leaves nothing to quote with"},
		{"concentration below one", func(p *Params) { p.Concentration = 0.9 },
			"concentration", "below 1 it is tighter than an equal split and no " +
				"market could hold its own share"},

		{"zero quote", func(p *Params) { p.S = 0 }, "S =",
			"H-CO-4b never sends a count of 0.00"},
		{"S_max below S", func(p *Params) { p.SMax = p.S - 1 }, "S_max",
			"the base quote would itself be over the aggregate cap"},
		{"inv_soft at zero", func(p *Params) { p.InvSoft = 0 }, "inv_soft",
			"§6.2's taper divides by (inv_hard - inv_soft)"},
		{"inv_soft above inv_hard", func(p *Params) { p.InvSoft = p.InvHard + 1 },
			"inv_soft", "a non-increasing ladder makes the skew discontinuous " +
				"or undefined"},
		{"inv_hard above inv_kill", func(p *Params) { p.InvHard = p.InvKill + 1 },
			"inv_hard", "REDUCING would begin only after the global kill"},
		{"pnl_kill at zero", func(p *Params) { p.PnLKill = 0 }, "pnl_kill",
			"a non-negative loss floor fires immediately (H-HALT-5)"},
		{"pnl_kill positive", func(p *Params) { p.PnLKill = num.MoneyFromDollars(15) },
			"pnl_kill", "sign inverted: the harness would halt at a profit"},

		{"negative debounce", func(p *Params) { p.Debounce = -time.Millisecond },
			"debounce_s", ""},
		{"no stale-bid ticks", func(p *Params) { p.StaleBidTicks = 0 },
			"stale_bid_ticks", "at 0 the H-Q-8 stranded brake fires on every " +
				"quote at the touch"},
		{"no queue age", func(p *Params) { p.MaxQueueAge = 0 }, "max_queue_age",
			"nothing is ever promoted and §6.6's anti-starvation is inert"},
		{"zero write rate", func(p *Params) { p.WriteRate = 0 }, "write_rate",
			"no write ever leaves, including a reducer"},
		{"zero write burst", func(p *Params) { p.WriteBurst = 0 }, "write_burst", ""},

		// H-CLOSE-0 / HR-017. The named sequence: at schedule_poll_s = 300 with
		// final_lead = 60s, a close_time that moves from 17:00 to 12:03 at
		// 12:00:01 is next observed at 12:05 -- after the close. Neither the
		// close lead nor the final cancel ever ran.
		{"schedule polled slower than the lead it enforces",
			func(p *Params) { p.SchedulePoll = 300 * time.Second },
			"schedule_poll_s", "a lead cannot be enforced unless the schedule " +
				"is read at least twice within it"},
		{"no final lead", func(p *Params) { p.FinalLead = 0 }, "final_lead",
			"H-CLOSE-3's verified final cancel would have no window"},
		{"close lead inside the final lead",
			func(p *Params) { p.CloseLead = p.FinalLead },
			"close_lead", "SETTLING would begin at or after the final cancel " +
				"and H-CLOSE-2a's window would not exist"},

		{"no position poll", func(p *Params) { p.PositionPoll = 0 },
			"position_poll_s", "§8.3's I2 monitor is the thing that must never " +
				"go quiet"},
		{"truth ages out inside two polls",
			func(p *Params) { p.TruthMaxAge = p.PositionPoll },
			"truth_max_age_s", "a single missed poll ages truth out and " +
				"H-FAIL-4 stops all dispatch"},
		{"no owner stall window", func(p *Params) { p.OwnerStall = 0 },
			"owner_stall_s", "at 0 every tick that reads the same snapshot " +
				"twice is an A5 stall"},
		{"no mark age", func(p *Params) { p.PnLMarkMaxAge = 0 },
			"pnl_mark_max_age_s", "H-HALT-5 would mark P&L against any age of " +
				"price"},
		{"hysteresis of one", func(p *Params) { p.Hysteresis = 1 }, "hysteresis", ""},
		{"negative hysteresis", func(p *Params) { p.Hysteresis = -0.1 },
			"hysteresis", ""},
		{"zero drift tolerance", func(p *Params) { p.PosDriftTol = 0 },
			"pos_drift_tol", "H-POS-2 would send the market to REDUCING on any " +
				"drift at all, including a rounding of the exchange's own"},
		{"drift tolerance above the hard bound",
			func(p *Params) { p.PosDriftTol = p.PosDriftHard + 1 },
			"pos_drift_tol", "the sustained trigger would fire only after the " +
				"immediate one, so the immediate one is unreachable"},

		// F1's three transport clocks are a ladder, not three numbers.
		{"pong not due before the next ping",
			func(p *Params) { p.PongTimeout = p.WSPingInterval },
			"pong_timeout_s", "a missed pong is never attributable to the ping " +
				"that went unanswered"},
		{"no pong timeout", func(p *Params) { p.PongTimeout = 0 },
			"pong_timeout_s", "F1's half-open detector never fires"},
		{"read deadline pre-empts the ping",
			func(p *Params) { p.ReadDeadline = p.WSPingInterval },
			"read_deadline_s", "the backstop would fire before the detector it " +
				"backs up, so every quiet interval reads as a dead socket"},
		{"no quiet window", func(p *Params) { p.Quiet = 0 }, "quiet_s",
			"F5's per-market silence detector fires on every market"},
		{"no disconnect window", func(p *Params) { p.DisconnectReduce = 0 },
			"disconnect_reduce_s", "F4 would send markets to REDUCING on any " +
				"disconnect, including a clean reconnect"},

		{"no coid retries", func(p *Params) { p.RetrySameCoidMax = 0 },
			"retry_same_coid_max", "H-ORD-2b's same-coid recovery is the only " +
				"thing that makes an ambiguous create safe to retry"},
		{"no unknown-resolve window", func(p *Params) { p.UnknownResolve = 0 },
			"unknown_resolve_s", ""},
		{"no unknown ping", func(p *Params) { p.UnknownPing = 0 },
			"unknown_ping_s", ""},
		{"no tenor bound", func(p *Params) { p.MaxTenor = 0 }, "max_tenor_d",
			"H-SEL-8 would admit a market of any length"},
		{"no drain timeout", func(p *Params) { p.DrainTimeout = 0 },
			"drain_timeout_h", ""},
		{"no heartbeat", func(p *Params) { p.Heartbeat = 0 }, "heartbeat_s",
			"the heartbeat is the dead-man's switch (§13.4) and F18 is " +
				"detected by its absence -- at 0 the operator's only " +
				"independent liveness signal is gone"},
		{"no backfill window", func(p *Params) { p.Backfill = 0 }, "backfill_h", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Default()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted the configuration: %s", tc.why)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() rejected, but not for %s -- got:\n%v\n"+
					"a rejection for an unrelated reason leaves this rule "+
					"unproven", tc.want, err)
			}
		})
	}
}

// TestValidateReportsEveryFailure: Validate accumulates.
//
// A validator that returns at the first bad field turns configuration repair
// into a serial edit-restart loop, and the operator is doing that against a
// live account at the point they most need the whole picture at once.
func TestValidateReportsEveryFailure(t *testing.T) {
	p := Default()
	p.NMarkets = 0
	p.PnLKill = 0
	p.Heartbeat = 0

	err := p.Validate()
	if err == nil {
		t.Fatal("Validate() accepted three broken parameters")
	}
	for _, want := range []string{"n_markets", "pnl_kill", "heartbeat_s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %s:\n%v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "3 invalid parameter(s)") {
		t.Errorf("the message does not count the failures:\n%v", err)
	}
}
