// Package cfg is harness-spec.md §16: every knob, one place.
//
// §15 requires that the `run` table records "every parameter in §16, verbatim"
// at startup, which only means anything if there is exactly one struct to
// record. A parameter that lives at its use site is a parameter that is not in
// the run row, and a run whose configuration cannot be reconstructed from
// harness.db is a run whose evidence cannot be interpreted afterwards.
//
// Pure leaf: it imports only num. No clock, no I/O.
package cfg

import (
	"fmt"
	"time"

	"lip/harness/num"
)

// Params is the §16 table. Field order follows the table so the two can be
// diffed by eye; each field names its spec parameter and the rule that owns it.
//
// Every duration-valued knob is a time.Duration rather than a float of seconds.
// F21 (clock step) and F7 (host sleep) both turn on comparing a wall delta
// against a monotonic one, and a bare float64 of seconds is the representation
// that lets those two be mixed up silently.
type Params struct {
	// --- selection and capital (§10) ---

	NMarkets       int       // n_markets      §10.3
	CapitalMax     num.Money // capital_max    H-CAP-1
	CapitalReserve float64   // capital_reserve H-CAP-3, fraction held unallocated
	Concentration  float64   // concentration  H-CAP-2, per-market multiple

	// --- quote sizing (§6.2) ---

	S       num.Qty // S        base quote size, contracts per side
	SMax    num.Qty // S_max    aggregate cap per side per market
	InvSoft num.Qty // inv_soft taper begins
	InvHard num.Qty // inv_hard taper reaches zero; market -> REDUCING
	InvKill num.Qty // inv_kill global WINDING_DOWN (F17)

	PnLKill num.Money // pnl_kill §12, H-HALT-5. Negative: it is a loss floor.

	// --- requote policy (§6.5) and the write queue (§6.6) ---

	Debounce        time.Duration // debounce_s          H-Q-7
	RequoteInterval time.Duration // requote_interval_s  §6.6, per side per market
	StaleBidTicks   int           // stale_bid_ticks     H-Q-8 stranded brake
	MaxQueueAge     time.Duration // max_queue_age       §6.6 anti-starvation
	WriteRate       float64       // write_rate          §6.6, writes/s global
	WriteBurst      int           // write_burst         §6.6

	// --- polling and freshness (§8, §9, §11) ---

	PositionPoll     time.Duration // position_poll_s     H-POS-1
	BalancePoll      time.Duration // balance_poll_s      §15
	SchedulePoll     time.Duration // schedule_poll_s     H-CLOSE-0
	GateFailDebounce time.Duration // gate_fail_debounce_s H-Q-4a
	OwnerStall       time.Duration // owner_stall_s       H-TOP-5 / I3
	TruthMaxAge      time.Duration // truth_max_age_s     H-FAIL-4
	PnLMarkMaxAge    time.Duration // pnl_mark_max_age_s  H-HALT-5
	Reselect         time.Duration // reselect_s          H-SEL-10
	Hysteresis       float64       // hysteresis          H-SEL-10

	PosDriftTol  num.Qty // pos_drift_tol  H-POS-2, sustained -> REDUCING
	PosDriftHard num.Qty // pos_drift_hard H-POS-2, immediate global

	// --- transport (F1) ---

	WSPingInterval time.Duration // ping_interval_s  F1
	PongTimeout    time.Duration // pong_timeout_s   F1
	ReadDeadline   time.Duration // read_deadline_s  F1, backstop
	Quiet          time.Duration // quiet_s          F5, per-market silence
	// DisconnectReduce is §16's `disconnect_halt_s` (F4), RENAMED.
	//
	// H-HALT-1 forbids an identifier named for a halt, and the parameter does
	// not name one: exceeding it sends the affected markets to REDUCING, which
	// keeps the exit alive. Calling that a halt is the misnomer the whole design
	// is built against. Recorded as a spec patch in §16 rather than as a local
	// deviation.
	DisconnectReduce time.Duration
	Stuck            time.Duration // stuck_s F16, informational only

	// --- order lifecycle (§7) ---

	UnknownResolve   time.Duration // unknown_resolve_s   §7.2
	RetrySameCoidMax int           // retry_same_coid_max H-ORD-2b
	UnknownPing      time.Duration // unknown_ping_s      §7.2

	// --- close handling (§9) ---

	CloseLead time.Duration // close_lead §16: 1h, cut from 4h to bound HR-011
	FinalLead time.Duration // final_lead H-CLOSE-3
	// CloseLeadKeepReducing is the one place the spec extends a locked decision
	// rather than implementing it literally (§19.1). Decided on argument, not
	// evidence, and instrumented by V7.11. Reversible.
	CloseLeadKeepReducing bool
	MaxTenor              time.Duration // max_tenor_d H-SEL-8

	// --- lifecycle and reporting ---

	// DrainTimeout ESCALATES; it never exits with q != 0 (H-HALT-3, HR-009).
	// A drain timeout is evidence the operator is needed, not authority to
	// abandon inventory.
	DrainTimeout time.Duration // drain_timeout_h
	Heartbeat    time.Duration // heartbeat_s  H-PING-1
	Backfill     time.Duration // backfill_h   §7.5 step 3
}

// Default is §10.3's first live configuration plus every §16 default, verbatim.
func Default() Params {
	return Params{
		NMarkets:       6,
		CapitalMax:     num.MoneyFromDollars(500),
		CapitalReserve: 0.25,
		Concentration:  2.0,

		S:       num.QtyFromFloat(100),
		SMax:    num.QtyFromFloat(400),
		InvSoft: num.QtyFromFloat(25),
		InvHard: num.QtyFromFloat(60),
		InvKill: num.QtyFromFloat(150),

		PnLKill: num.MoneyFromDollars(-75),

		Debounce:        250 * time.Millisecond,
		RequoteInterval: 5 * time.Second,
		StaleBidTicks:   8,
		MaxQueueAge:     30 * time.Second,
		WriteRate:       5,
		WriteBurst:      10,

		PositionPoll:     5 * time.Second,
		BalancePoll:      60 * time.Second,
		SchedulePoll:     30 * time.Second,
		GateFailDebounce: 30 * time.Second,
		OwnerStall:       3 * time.Second,
		TruthMaxAge:      60 * time.Second,
		PnLMarkMaxAge:    30 * time.Second,
		Reselect:         3600 * time.Second,
		Hysteresis:       0.30,

		PosDriftTol:  num.QtyFromFloat(0.01),
		PosDriftHard: num.QtyFromFloat(5),

		WSPingInterval:   10 * time.Second,
		PongTimeout:      5 * time.Second,
		ReadDeadline:     60 * time.Second,
		Quiet:            60 * time.Second,
		DisconnectReduce: 60 * time.Second,
		Stuck:            1800 * time.Second,

		UnknownResolve:   10 * time.Second,
		RetrySameCoidMax: 3,
		UnknownPing:      120 * time.Second,

		CloseLead:             1 * time.Hour,
		FinalLead:             60 * time.Second,
		CloseLeadKeepReducing: true,
		MaxTenor:              30 * 24 * time.Hour,

		DrainTimeout: 12 * time.Hour,
		Heartbeat:    3600 * time.Second,
		Backfill:     24 * time.Hour,
	}
}

// Validate checks the relations §16 states as rules rather than as values.
//
// It deliberately does NOT check fundability: H-CAP-8 needs a price bound and
// belongs with the capital model in harness/risk (V1.12). What is here is the
// internal consistency a configuration can be judged on with no market data at
// all, and every one of these is a condition under which some rule elsewhere in
// the spec silently cannot do its job.
func (p Params) Validate() error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}

	if p.NMarkets < 1 {
		bad("n_markets = %d, must be at least 1", p.NMarkets)
	}
	if p.CapitalMax <= 0 {
		bad("capital_max = %s, must be positive", p.CapitalMax)
	}
	if p.CapitalReserve < 0 || p.CapitalReserve >= 1 {
		bad("capital_reserve = %v, must be in [0,1)", p.CapitalReserve)
	}
	if p.Concentration < 1 {
		bad("concentration = %v, must be at least 1 -- below 1 it is tighter "+
			"than an equal split and no market could hold its own share",
			p.Concentration)
	}

	if p.S <= 0 {
		bad("S = %s, must be positive", p.S.Wire())
	}
	if p.SMax < p.S {
		bad("S_max = %s is below S = %s, so the base quote is itself over the "+
			"aggregate cap", p.SMax.Wire(), p.S.Wire())
	}
	if !(p.InvSoft > 0 && p.InvSoft < p.InvHard && p.InvHard < p.InvKill) {
		bad("require 0 < inv_soft < inv_hard < inv_kill, got %s / %s / %s -- "+
			"§6.2's taper divides by (inv_hard - inv_soft) and a non-increasing "+
			"ladder makes the skew discontinuous or undefined",
			p.InvSoft.Wire(), p.InvHard.Wire(), p.InvKill.Wire())
	}
	if p.PnLKill >= 0 {
		bad("pnl_kill = %s, must be negative -- it is a loss floor, and a "+
			"non-negative one fires immediately (H-HALT-5)", p.PnLKill)
	}

	if p.Debounce < 0 {
		bad("debounce_s = %v, must not be negative", p.Debounce)
	}
	if p.StaleBidTicks < 1 {
		bad("stale_bid_ticks = %d, must be at least 1 -- at 0 the stranded "+
			"brake (H-Q-8) fires on every quote at the touch", p.StaleBidTicks)
	}
	if p.MaxQueueAge <= 0 {
		bad("max_queue_age = %v, must be positive or nothing is ever promoted",
			p.MaxQueueAge)
	}
	if p.WriteRate <= 0 {
		bad("write_rate = %v, must be positive", p.WriteRate)
	}
	if p.WriteBurst < 1 {
		bad("write_burst = %d, must be at least 1", p.WriteBurst)
	}

	// H-CLOSE-0. "A poll interval five times longer than the lead it is
	// supposed to trigger cannot enforce that lead." HR-017's sequence: at
	// schedule_poll_s = 300 with final_lead = 60s, a close_time that moves from
	// 17:00 to 12:03 at 12:00:01 is next observed at 12:05 -- after the close.
	// Neither the close lead nor the final cancel ever ran.
	//
	// Stated as a sampling bound: a lead cannot be enforced unless the schedule
	// is read at least twice within it. The §16 defaults (30s poll, 60s lead)
	// sit exactly on this boundary, which is the tightest the spec's own numbers
	// permit and is recorded here so that raising schedule_poll_s without
	// raising final_lead is rejected rather than merely regretted.
	if p.FinalLead <= 0 {
		bad("final_lead = %v, must be positive", p.FinalLead)
	} else if p.SchedulePoll*2 > p.FinalLead {
		bad("schedule_poll_s = %v exceeds half of final_lead = %v (H-CLOSE-0): "+
			"a schedule read less than twice per lead cannot enforce it",
			p.SchedulePoll, p.FinalLead)
	}
	if p.CloseLead <= p.FinalLead {
		bad("close_lead = %v must exceed final_lead = %v -- SETTLING would "+
			"otherwise begin at or after the final cancel and H-CLOSE-2a's "+
			"window would not exist", p.CloseLead, p.FinalLead)
	}

	if p.PositionPoll <= 0 {
		bad("position_poll_s = %v, must be positive", p.PositionPoll)
	} else if p.TruthMaxAge < 2*p.PositionPoll {
		bad("truth_max_age_s = %v is under two position polls (%v): a single "+
			"missed poll ages truth out and H-FAIL-4 stops all dispatch",
			p.TruthMaxAge, p.PositionPoll)
	}
	if p.OwnerStall <= 0 {
		bad("owner_stall_s = %v, must be positive -- at 0 every tick that "+
			"reads the same snapshot twice is a stall (A5)", p.OwnerStall)
	}
	if p.PnLMarkMaxAge <= 0 {
		bad("pnl_mark_max_age_s = %v, must be positive", p.PnLMarkMaxAge)
	}
	if p.Hysteresis < 0 || p.Hysteresis >= 1 {
		bad("hysteresis = %v, must be in [0,1)", p.Hysteresis)
	}
	if !(p.PosDriftTol > 0 && p.PosDriftTol < p.PosDriftHard) {
		bad("require 0 < pos_drift_tol < pos_drift_hard, got %s / %s",
			p.PosDriftTol.Wire(), p.PosDriftHard.Wire())
	}

	// F1's three transport clocks are a ladder, not three independent numbers:
	// the pong must be due before the next ping is sent, or a missed pong is
	// never attributable to the ping that went unanswered; and the read
	// deadline is the backstop, so it must be the longest.
	if !(p.PongTimeout > 0 && p.PongTimeout < p.WSPingInterval) {
		bad("require 0 < pong_timeout_s < ping_interval_s, got %v / %v",
			p.PongTimeout, p.WSPingInterval)
	}
	if p.ReadDeadline <= p.WSPingInterval {
		bad("read_deadline_s = %v must exceed ping_interval_s = %v -- the "+
			"deadline is F1's backstop and must not pre-empt the ping/pong "+
			"detector it backs up", p.ReadDeadline, p.WSPingInterval)
	}
	if p.Quiet <= 0 {
		bad("quiet_s = %v, must be positive", p.Quiet)
	}
	if p.DisconnectReduce <= 0 {
		bad("disconnect_reduce_s = %v, must be positive", p.DisconnectReduce)
	}

	if p.RetrySameCoidMax < 1 {
		bad("retry_same_coid_max = %d, must be at least 1 (H-ORD-2b)",
			p.RetrySameCoidMax)
	}
	if p.UnknownResolve <= 0 {
		bad("unknown_resolve_s = %v, must be positive", p.UnknownResolve)
	}
	if p.UnknownPing <= 0 {
		bad("unknown_ping_s = %v, must be positive", p.UnknownPing)
	}
	if p.MaxTenor <= 0 {
		bad("max_tenor_d = %v, must be positive", p.MaxTenor)
	}
	if p.DrainTimeout <= 0 {
		bad("drain_timeout_h = %v, must be positive", p.DrainTimeout)
	}
	if p.Heartbeat <= 0 {
		bad("heartbeat_s = %v, must be positive -- the heartbeat is the "+
			"dead-man's switch (§13.4) and F18 is detected by its absence",
			p.Heartbeat)
	}
	if p.Backfill <= 0 {
		bad("backfill_h = %v, must be positive", p.Backfill)
	}

	if len(errs) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%d invalid parameter(s):", len(errs))
	for _, e := range errs {
		msg += "\n  - " + e.Error()
	}
	return fmt.Errorf("%s", msg)
}

// confidence: high
