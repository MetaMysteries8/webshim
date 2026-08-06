package websim

import (
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// RetryPolicy matches the playbook's default policy: four attempts, a 30 second
// per-request timeout, and exponential backoff of 750ms, 1500ms, 3000ms capped
// at 15 seconds. Jitter is added because the playbook asks for it in
// multi-agent systems.
type RetryPolicy struct {
	MaxAttempts    int
	RequestTimeout time.Duration
	BaseDelay      time.Duration
	MaxDelay       time.Duration
	// Jitter is the fraction of a delay that may be added at random, in the
	// range [0, Jitter]. 0.2 means "up to 20% longer".
	Jitter float64
}

// DefaultRetryPolicy returns the playbook defaults.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:    4,
		RequestTimeout: 30 * time.Second,
		BaseDelay:      750 * time.Millisecond,
		MaxDelay:       15 * time.Second,
		Jitter:         0.2,
	}
}

// delayFor returns the backoff before the given attempt number, where attempt 1
// is the first retry. The sequence is 750ms, 1500ms, 3000ms, ... capped at
// MaxDelay, plus jitter.
func (p RetryPolicy) delayFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := p.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= p.MaxDelay {
			d = p.MaxDelay
			break
		}
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if p.Jitter > 0 {
		d += time.Duration(rand.Float64() * p.Jitter * float64(d))
	}
	return d
}

// Retry classification
//
// The playbook lists 408, 409, 425, 429, and 5xx as retry candidates, but also
// warns that "POST may create duplicates" and that after an ambiguous POST the
// agent must inspect server state rather than retry. Those two rules are in
// tension, so this package resolves it by method:
//
//	GET, HEAD   fully safe to repeat            -> 408, 409, 425, 429, 5xx
//	PATCH       sets a desired final value      -> 408, 409, 425, 429, 5xx
//	POST        may commit before failing       -> 408, 425, 429 only
//	DELETE      may commit before failing       -> 408, 425, 429 only
//
// 408, 425, and 429 all mean the server explicitly declined to process the
// request, so repeating them cannot duplicate work. A 5xx or a 409 on a POST or
// DELETE may have been committed server-side, so those surface to the caller,
// which re-reads state instead of guessing.

// repeatableMethod reports whether repeating a method cannot duplicate an
// effect.
func repeatableMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPatch, http.MethodPut:
		return true
	}
	return false
}

// shouldRetryStatus applies the table above.
func shouldRetryStatus(method string, status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,        // 425
		http.StatusTooManyRequests: // 429
		return true
	case http.StatusConflict: // 409
		return repeatableMethod(method)
	}
	if status >= 500 && status <= 599 {
		return repeatableMethod(method)
	}
	return false
}

// shouldRetryTransport decides whether a transport-level failure may be
// retried.
//
// A failure that provably happened before the request reached the server (DNS
// resolution failure, connection refused, no route to host) is safe to repeat
// for any method: the server never saw it. Any other transport failure is
// ambiguous -- the request may have been delivered and applied while the
// response was lost -- so only repeatable methods retry. This is the concrete
// implementation of the playbook's rule that after a POST timeout you inspect
// server state instead of retrying.
func shouldRetryTransport(method string, err error) bool {
	if err == nil {
		return false
	}
	if isPreSendError(err) {
		return true
	}
	return repeatableMethod(method)
}

// isPreSendError reports whether err proves the request never reached the
// server.
func isPreSendError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	return false
}

// retryAfterDelay parses a Retry-After header, which may be a number of seconds
// or an HTTP date. It returns 0 when the header is absent or unparseable.
func retryAfterDelay(h string, now time.Time) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
