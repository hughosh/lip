// Package core is the pure, deterministic half of the rig: book
// reconstruction, trade attribution and the LIP qualifying walk. It performs no
// I/O, reads no clock, and starts no goroutines, so its output is a function of
// the frames fed to it and nothing else. That is what makes it differentially
// testable against the Python original.
package core

import (
	"fmt"
	"math"
	"strconv"
)

// Kalshi sends every price and size as a decimal string: prices with four
// decimal places of a dollar ("0.5800"), sizes with two ("9.23"). The Python
// rig converts them with float() and round(), and this port is required to be
// bug-for-bug identical to it, so both quirks below are deliberate.

// ParsePriceCents converts a dollar string to integer cents exactly as
// CPython's round(float(p) * 100) does.
//
// Two traps, both load-bearing:
//
//  1. Python's round() is BANKER'S rounding — ties go to the even integer, not
//     away from zero. math.Round would give 3 for 2.5 where Python gives 2.
//     math.RoundToEven is the matching primitive. 46 of the distinct price
//     strings observed on the wire discriminate the two (0.0050, 0.0250,
//     0.0450, ...), and 8.8% of all price occurrences are fractional cents, so
//     this is not a theoretical hazard.
//
//  2. The tie is decided on the EXACT binary value of the product, not on the
//     decimal text. float64("0.0150")*100 is 1.4999999999999998, so it rounds
//     DOWN to 1 under either mode — while 0.0250*100 is exactly 2.5 and only
//     banker's rounding gives 2. Parsing the decimal digits directly would get
//     0.0150 wrong. Multiply in float64 and let IEEE754 decide.
//
// The expression is a bare multiply with no addition, so Go cannot fuse it into
// an FMA and change the result.
func ParsePriceCents(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("price %q: %w", s, err)
	}
	return int(math.RoundToEven(f * 100)), nil
}

// ParseSize converts a size string to the same float64 CPython's float() would
// produce. Both parsers are correctly rounded, so this is exact.
func ParseSize(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return f, nil
}

// confidence: high
