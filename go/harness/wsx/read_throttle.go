package wsx

import (
	"errors"
	"lip/harness/rest"
)

// RateLimits returns the actual 429s in one poll cycle. The owner consumes
// these on its goroutine, including a 429 on a partial cursor walk. Synthetic
// failed walks while an endpoint awaits retry are not new throttle events.
func (r PortfolioRead) RateLimits() []*rest.RateLimitError {
	var out []*rest.RateLimitError
	for _, err := range []error{r.fills.Err, r.orders.Err, r.positions.Err} {
		var throttle *rest.RateLimitError
		if errors.As(err, &throttle) {
			copy := *throttle
			out = append(out, &copy)
		}
	}
	return out
}

// confidence: high
