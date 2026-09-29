package rest

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError is an exchange 429. The request was answered, but the answer
// does not establish that a create was absent or a cancel took effect. A
// dispatcher can use Delay when HasDelay is true; otherwise it must choose its
// own bounded exponential backoff. No REST method sleeps on this error.
type RateLimitError struct {
	RetryAfter string
	Delay      time.Duration
	HasDelay   bool
	body       string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("HTTP 429 rate limited: %s", e.body)
}

func rateLimitError(resp Response) *RateLimitError {
	return rateLimitErrorAt(resp, time.Now())
}

func rateLimitErrorAt(resp Response, now time.Time) *RateLimitError {
	e := &RateLimitError{body: snippet(resp.Body)}
	if resp.Header != nil {
		e.RetryAfter = resp.Header.Get("Retry-After")
	}
	e.Delay, e.HasDelay = parseRetryAfter(e.RetryAfter, now)
	return e
}

// parseRetryAfter accepts RFC 9110 delay-seconds or an HTTP date. An absent,
// malformed, signed, or fractional value leaves policy to the dispatcher. A
// huge valid delay saturates instead of wrapping negative and retrying early.
func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(raw, 10, 64); err == nil {
		const maxSeconds = uint64(math.MaxInt64 / int64(time.Second))
		if seconds > maxSeconds {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		if !when.After(now) {
			return 0, true
		}
		return when.Sub(now), true
	}
	return 0, false
}

// confidence: high
