package websim

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Sentinel errors. Callers use errors.Is to branch on the playbook's error
// decision table without string matching.
var (
	// ErrUnauthorized is a 401. The token is missing, expired, malformed, or
	// rejected. Playbook: stop, do not retry, do not rotate accounts.
	ErrUnauthorized = errors.New("websim: unauthorized")

	// ErrForbidden is a 403. The account lacks permission. Stop.
	ErrForbidden = errors.New("websim: forbidden")

	// ErrNotFound is a 404. Re-check IDs, version, and paths before acting.
	ErrNotFound = errors.New("websim: not found")

	// ErrConflict is a 409. Re-read state before retrying.
	ErrConflict = errors.New("websim: conflict")

	// ErrRateLimited is a 429.
	ErrRateLimited = errors.New("websim: rate limited")

	// ErrUnexpectedShape means a response was missing a required identifier or
	// did not match a documented shape. Playbook rule 15: stop mutation,
	// retain the last known live version, and report.
	ErrUnexpectedShape = errors.New("websim: unexpected response shape")

	// ErrUnsafePath means a path was absolute, contained a ".." component, or
	// matched the forbidden "index (n).html" pattern.
	ErrUnsafePath = errors.New("websim: unsafe path")

	// ErrConcurrentModification means another actor changed current_version
	// during an edit flow. Playbook: stop and rebase from the new current
	// version; do not promote a stale branch.
	ErrConcurrentModification = errors.New("websim: project changed during edit flow")

	// ErrNotPromoted means a flow stopped before promotion, leaving the
	// previous live version intact.
	ErrNotPromoted = errors.New("websim: revision was not promoted")

	// ErrNoToken means no bearer token could be resolved.
	ErrNoToken = errors.New("websim: no bearer token available; run `websim-cli login`")
)

// maxErrorBodyBytes caps how much of a failing response body is retained. The
// playbook asks for a "sanitized short error body".
const maxErrorBodyBytes = 500

// APIError describes a non-2xx response. It never contains request headers.
type APIError struct {
	Op         string // logical operation, e.g. "get project"
	Method     string
	URL        string
	StatusCode int
	Body       string // truncated and sanitized
	RetryAfter string // raw Retry-After header, when present
}

func (e *APIError) Error() string {
	b := e.Body
	if b == "" {
		b = "(empty body)"
	}
	return fmt.Sprintf("%s: %s %s -> %d %s: %s",
		e.Op, e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode), b)
}

// Unwrap maps status codes onto the sentinel errors so callers can branch with
// errors.Is.
func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}

// emailRe matches email addresses.
//
// WebSim's error bodies can embed decoded JWT claims. An expired-token response
// carries the account's email, so echoing a raw error body into a log file
// leaks personal data that the operator never chose to record. Rule 14 asks for
// a sanitized error body; this is part of what that means in practice.
var emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// jwtRe matches a compact JWT, which may appear inside an error body even
// though it never appears in a header we wrote.
var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]+`)

// sanitizer removes secrets from strings bound for logs or error messages.
type sanitizer struct {
	secrets []string
}

func newSanitizer(secrets ...string) *sanitizer {
	s := &sanitizer{}
	for _, sec := range secrets {
		// Very short values would cause absurd over-redaction; a real bearer
		// token is far longer than this floor.
		if len(sec) >= 8 {
			s.secrets = append(s.secrets, sec)
		}
	}
	return s
}

// clean replaces every known secret in v with a redaction marker, then strips
// credential material and personal data that may have arrived from upstream.
func (s *sanitizer) clean(v string) string {
	if s != nil {
		for _, sec := range s.secrets {
			if sec != "" && strings.Contains(v, sec) {
				v = strings.ReplaceAll(v, sec, "[redacted]")
			}
		}
	}
	// These run even when the sanitizer has no configured secrets, because
	// the value being scrubbed came from the server rather than from us.
	v = jwtRe.ReplaceAllString(v, "[redacted-jwt]")
	v = emailRe.ReplaceAllString(v, "[redacted-email]")
	return v
}

// truncate shortens a body for inclusion in an error message.
func truncate(v string, n int) string {
	v = strings.TrimSpace(v)
	if len(v) <= n {
		return v
	}
	return v[:n] + "... (truncated)"
}
