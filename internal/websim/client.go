// Package websim is a client for WebSim's internal web-application API.
//
// It is a direct transliteration of docs/WEBSIM_API_AGENT_PLAYBOOK.txt: every
// non-negotiable rule in that document is enforced here in code rather than
// left to the caller. The package deliberately depends on nothing but the
// standard library so that it can be tested in isolation and reused from the
// CLI, the agent toolbelt, and the TUI without pulling in a UI or an LLM.
//
// The endpoints are not a versioned public API. When a response does not match
// a documented shape, this package stops and reports rather than guessing --
// see ErrUnexpectedShape.
package websim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the WebSim web-application API root.
const DefaultBaseURL = "https://websim.com/api/v1"

// origin is sent on every API request, matching the browser app.
const origin = "https://websim.com"

// ErrDryRun is returned instead of performing a mutation when the client is in
// dry-run mode. The intended request is logged first, so a dry run shows the
// exact point at which a flow would have written.
var ErrDryRun = errors.New("websim: dry-run mode; mutation not sent")

// Options configures a Client.
type Options struct {
	// Token is the bearer token. Required for everything except raw content
	// downloads.
	Token Token

	// BaseURL defaults to DefaultBaseURL. Overridden in tests.
	BaseURL string

	// ContentHostFn returns the raw content host for a project, without a
	// trailing slash. It defaults to https://{project_id}.c.websim.com.
	// Overridden in tests.
	ContentHostFn func(projectID string) string

	// HTTPClient defaults to a client with no global timeout; per-request
	// timeouts come from RetryPolicy.RequestTimeout.
	HTTPClient *http.Client

	// Logger defaults to a discarding logger. The client never logs
	// Authorization headers or token values.
	Logger *slog.Logger

	// Retry defaults to DefaultRetryPolicy.
	Retry RetryPolicy

	// UserAgent is sent on every request.
	UserAgent string

	// DryRun makes every mutating request fail with ErrDryRun after logging
	// what it would have sent.
	DryRun bool
}

// Client talks to the WebSim API.
type Client struct {
	token         Token
	baseURL       string
	contentHostFn func(string) string
	http          *http.Client
	log           *slog.Logger
	retry         RetryPolicy
	userAgent     string
	dryRun        bool
	locks         *lockSet
	san           *sanitizer
}

// New builds a Client.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")

	if opts.ContentHostFn == nil {
		opts.ContentHostFn = func(projectID string) string {
			return "https://" + projectID + ".c.websim.com"
		}
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Retry.MaxAttempts == 0 {
		opts.Retry = DefaultRetryPolicy()
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "webshim"
	}

	return &Client{
		token:         opts.Token,
		baseURL:       opts.BaseURL,
		contentHostFn: opts.ContentHostFn,
		http:          opts.HTTPClient,
		log:           opts.Logger,
		retry:         opts.Retry,
		userAgent:     opts.UserAgent,
		dryRun:        opts.DryRun,
		locks:         newLockSet(),
		san:           newSanitizer(opts.Token.Reveal()),
	}, nil
}

// HasToken reports whether a bearer token was supplied.
func (c *Client) HasToken() bool { return c.token != "" }

// DryRun reports whether the client is in dry-run mode.
func (c *Client) DryRun() bool { return c.dryRun }

// Sanitize strips known secrets from a string. Exported so callers can clean
// text before logging or displaying it.
func (c *Client) Sanitize(s string) string { return c.san.clean(s) }

// ---------------------------------------------------------------------------
// Request plumbing
// ---------------------------------------------------------------------------

// request describes one logical HTTP call. newBody is a factory rather than a
// reader so that a retry can rebuild the body -- a reader would already be
// drained.
type request struct {
	op          string
	method      string
	url         string
	contentType string // empty means: do not set the header
	newBody     func() (io.Reader, error)
	referer     string
	noAuth      bool
}

// mutating reports whether this request can change server state.
func (r request) mutating() bool {
	switch r.method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// do executes a request with the configured retry policy and returns the raw
// response body.
func (c *Client) do(ctx context.Context, r request) ([]byte, error) {
	if !r.noAuth && c.token == "" {
		return nil, ErrNoToken
	}
	if c.dryRun && r.mutating() {
		c.log.Warn("dry-run: mutation suppressed",
			"op", r.op, "method", r.method, "url", c.san.clean(r.url))
		return nil, fmt.Errorf("%s: %w", r.op, ErrDryRun)
	}

	var lastErr error
	for attempt := 1; attempt <= c.retry.MaxAttempts; attempt++ {
		body, status, retryAfter, err := c.attempt(ctx, r)

		// A cancelled or expired parent context is final, never a retry.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s: %w", r.op, ctxErr)
		}

		switch {
		case err != nil:
			lastErr = fmt.Errorf("%s: %s %s: %w", r.op, r.method, c.san.clean(r.url), err)
			if attempt < c.retry.MaxAttempts && shouldRetryTransport(r.method, err) {
				c.logRetry(r, attempt, 0, err)
				if sleepErr := sleep(ctx, c.retry.delayFor(attempt)); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, lastErr

		case status >= 200 && status < 300:
			c.log.Debug("websim request",
				"op", r.op, "method", r.method, "url", c.san.clean(r.url),
				"status", status, "attempt", attempt, "bytes", len(body))
			return body, nil

		default:
			apiErr := &APIError{
				Op:         r.op,
				Method:     r.method,
				URL:        c.san.clean(r.url),
				StatusCode: status,
				Body:       c.san.clean(truncate(string(body), maxErrorBodyBytes)),
				RetryAfter: retryAfter,
			}
			lastErr = apiErr
			if attempt < c.retry.MaxAttempts && shouldRetryStatus(r.method, status) {
				delay := retryAfterDelay(retryAfter, time.Now())
				if delay <= 0 {
					delay = c.retry.delayFor(attempt)
				}
				c.logRetry(r, attempt, status, nil)
				if sleepErr := sleep(ctx, delay); sleepErr != nil {
					return nil, sleepErr
				}
				continue
			}
			return nil, apiErr
		}
	}
	return nil, lastErr
}

// attempt performs a single HTTP round trip.
func (c *Client) attempt(ctx context.Context, r request) (body []byte, status int, retryAfter string, err error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.retry.RequestTimeout)
	defer cancel()

	var reader io.Reader
	if r.newBody != nil {
		reader, err = r.newBody()
		if err != nil {
			return nil, 0, "", fmt.Errorf("building request body: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(attemptCtx, r.method, r.url, reader)
	if err != nil {
		return nil, 0, "", fmt.Errorf("building request: %w", err)
	}
	c.setHeaders(req, r)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Retry-After"), fmt.Errorf("reading response body: %w", err)
	}
	return body, resp.StatusCode, resp.Header.Get("Retry-After"), nil
}

// setHeaders applies the standard authenticated header set.
//
// Playbook rule 7 says never to set Content-Type manually for multipart bodies.
// That rule is written against JavaScript's FormData, where the fetch runtime
// generates the header and its boundary. Go's multipart.Writer does not: it
// only exposes FormDataContentType(). Sending no Content-Type at all would be a
// malformed multipart request. The rule's intent -- the boundary in the header
// must match the one in the body, and must never be hardcoded -- is honored in
// asset.go, which takes the header verbatim from the writer that wrote the
// body.
func (c *Client) setHeaders(req *http.Request, r request) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", c.userAgent)
	if !r.noAuth {
		req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
		req.Header.Set("Origin", origin)
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.referer != "" {
		req.Header.Set("Referer", r.referer)
	}
}

func (c *Client) logRetry(r request, attempt, status int, err error) {
	attrs := []any{
		"op", r.op,
		"method", r.method,
		"url", c.san.clean(r.url),
		"attempt", attempt,
		"max_attempts", c.retry.MaxAttempts,
	}
	if status != 0 {
		attrs = append(attrs, "status", status)
	}
	if err != nil {
		attrs = append(attrs, "error", c.san.clean(err.Error()))
	}
	c.log.Warn("websim request failed; retrying", attrs...)
}

// sleep waits for d unless ctx is done first.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// JSON helpers
// ---------------------------------------------------------------------------

// apiURL joins a path and optional query onto the API base.
func (c *Client) apiURL(pathAndQuery string) string {
	return c.baseURL + pathAndQuery
}

// getJSON issues an authenticated GET.
func (c *Client) getJSON(ctx context.Context, op, pathAndQuery string) ([]byte, error) {
	return c.do(ctx, request{
		op:     op,
		method: http.MethodGet,
		url:    c.apiURL(pathAndQuery),
	})
}

// sendJSON issues an authenticated request with a JSON body.
func (c *Client) sendJSON(ctx context.Context, op, method, pathAndQuery string, payload any) ([]byte, error) {
	encoded, err := jsonBytes(payload)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return c.do(ctx, request{
		op:          op,
		method:      method,
		url:         c.apiURL(pathAndQuery),
		contentType: "application/json",
		newBody:     bytesReaderFn(encoded),
	})
}

// jsonBytes encodes a payload for a request body.
func jsonBytes(payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}
	return encoded, nil
}

// bytesReaderFn returns a body factory that yields a fresh reader per attempt,
// so a retry does not send an already-drained body.
func bytesReaderFn(b []byte) func() (io.Reader, error) {
	return func() (io.Reader, error) { return bytes.NewReader(b), nil }
}

// commentReferer builds the Referer header that comment endpoints require.
func commentReferer(slug string) string {
	return origin + "/" + url.PathEscape(slug)
}

// GetSession returns the account behind the current token. Playbook: if this
// returns 401 or 403, stop; do not attempt mutations.
func (c *Client) GetSession(ctx context.Context) (*Session, error) {
	body, err := c.getJSON(ctx, "get session", "/session")
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("%w: decoding session: %v", ErrUnexpectedShape, err)
	}
	return &s, nil
}
