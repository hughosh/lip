package rest

import (
	"fmt"
	"strconv"
	"strings"

	"lip/harness/quote"
)

// ---------------------------------------------------------------------------
// §7.1 — client order IDs
// ---------------------------------------------------------------------------

// CoidPrefix is load-bearing: it is how §7.5's startup adoption distinguishes
// our orders from anything else on the account.
//
// The account is dedicated (§14), so in principle everything on it is ours --
// but "in principle" is what the foreign-activity assertion exists to check,
// and a restarted process that adopted a stranger's order would size a reducer
// against a position it does not hold.
const CoidPrefix = "lipH"

// Coid builds H-ORD-1's deterministic client order id:
//
//	lipH-{runID}-{marketIdx:03d}-{side}-{seq:08d}
//
// **Deterministic, not random, and that turned out to matter far more than the
// greppability it was specified for.**
//
// `probebot.py` used a fresh uuid4() per order. That makes a timed-out write
// unresolvable without a full order scan -- but worse, it makes H-ORD-2b's
// recovery mechanism unavailable in principle: a retry under a new coid IS a
// genuinely new order, and the exchange cannot report it as a duplicate because
// it is not one. With a stable coid, an ambiguous create is retried with the
// SAME coid and the exchange answers definitively in one round trip:
//
//	2xx                     -> the original never landed; this one is the order
//	409 order_already_exists -> the original DID land
//
// The 409 is a POSITIVE IDENTIFICATION, not an absence. That is what removes
// the need to prove a negative -- and proving a negative is exactly what
// H-ORD-2a deleted, because a single-page read cannot establish absence and an
// exhaustive walk cannot either without a snapshot-consistency contract that is
// not known to exist. This repository has already been bitten by that: the
// count of active programs grew past 1,000 mid-probe, a one-shot limit=200
// fetch pushed the live market outside the window, and a restart died claiming
// "no active liquidity program" while the program was still running.
//
// At most one order can exist per coid, which is what H-ORD-1's determinism was
// designed to buy.
func Coid(runID string, marketIdx int, side quote.Side, seq uint64) (string, error) {
	if err := ValidRunID(runID); err != nil {
		return "", err
	}
	if marketIdx < 0 || marketIdx > 999 {
		return "", fmt.Errorf("market index %d does not fit the %%03d field",
			marketIdx)
	}
	if seq > 99_999_999 {
		return "", fmt.Errorf("sequence %d does not fit the %%08d field", seq)
	}
	return fmt.Sprintf("%s-%s-%03d-%s-%08d",
		CoidPrefix, runID, marketIdx, side, seq), nil
}

// ValidRunID rejects a run id that would make a coid ambiguous to parse.
//
// The separator is "-", so a run id containing one would shift every field
// after it. That matters because ParseCoid is how a reconciliation walk decides
// whether an order found on the account is ours and which market it belongs to:
// a run id of "a-b" would make an order look like it came from a different
// market index, and the adoption would attribute somebody's position to the
// wrong ticker.
func ValidRunID(runID string) error {
	if runID == "" {
		return fmt.Errorf("empty run id")
	}
	if strings.ContainsRune(runID, '-') {
		return fmt.Errorf("run id %q contains the field separator: every "+
			"field after it would shift, and §7.5's adoption would attribute "+
			"an order to the wrong market", runID)
	}
	for _, r := range runID {
		ok := (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z')
		if !ok {
			return fmt.Errorf("run id %q contains %q; ULID-like alphanumerics "+
				"only", runID, r)
		}
	}
	return nil
}

// ParsedCoid is one of our client order ids, taken apart.
type ParsedCoid struct {
	RunID     string
	MarketIdx int
	Side      quote.Side
	Seq       uint64
}

// ParseCoid is the adoption-side read of H-ORD-1.
//
// It returns ok = false for anything that is not one of ours, which §7.5 treats
// as foreign activity on a dedicated account rather than as an order to adopt.
// Being strict here is the safe direction: an unrecognised order is escalated
// to the operator, whereas a mis-parsed one is silently attributed to a market
// it has nothing to do with.
func ParseCoid(coid string) (ParsedCoid, bool) {
	parts := strings.Split(coid, "-")
	if len(parts) != 5 || parts[0] != CoidPrefix {
		return ParsedCoid{}, false
	}
	if ValidRunID(parts[1]) != nil {
		return ParsedCoid{}, false
	}
	// %03d and %08d are fixed-width, so a field of any other length was not
	// produced by Coid and must not be read as though it were.
	if len(parts[2]) != 3 || len(parts[4]) != 8 {
		return ParsedCoid{}, false
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil || idx < 0 {
		return ParsedCoid{}, false
	}
	seq, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil {
		return ParsedCoid{}, false
	}
	var side quote.Side
	switch parts[3] {
	case "yes":
		side = quote.SideYes
	case "no":
		side = quote.SideNo
	default:
		return ParsedCoid{}, false
	}
	return ParsedCoid{RunID: parts[1], MarketIdx: idx, Side: side, Seq: seq}, true
}

// IsOurs reports whether an order found on the account was placed by this
// harness, in any run. §7.5's foreign-activity assertion is the caller.
func IsOurs(coid string) bool {
	_, ok := ParseCoid(coid)
	return ok
}

// confidence: high
