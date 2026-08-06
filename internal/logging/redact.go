package logging

import (
	"context"
	"log/slog"
	"strings"
)

// redactionMarker replaces a secret wherever one is found.
const redactionMarker = "[redacted]"

// redactingHandler scrubs known secrets from every record before it reaches the
// underlying handler.
//
// The typed credentials (websim.Token, auth.ProviderKey) already redact
// themselves when formatted, so this is a second line of defence: it catches
// raw strings that were never wrapped in a credential type, such as a token
// echoed back inside an upstream error body.
type redactingHandler struct {
	next    slog.Handler
	secrets []string
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	if len(h.secrets) == 0 {
		return h.next.Handle(ctx, r)
	}

	clean := slog.NewRecord(r.Time, r.Level, h.scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(h.scrubAttr(a))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(as))
	for i, a := range as {
		scrubbed[i] = h.scrubAttr(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(scrubbed), secrets: h.secrets}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), secrets: h.secrets}
}

// scrubAttr rewrites an attribute's value, recursing into groups.
func (h *redactingHandler) scrubAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.scrub(v.String()))
	case slog.KindGroup:
		items := v.Group()
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, h.scrubAttr(item))
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		// Anything that stringifies could carry a secret; the common case
		// is an error value wrapping an upstream response body.
		if s, ok := v.Any().(interface{ Error() string }); ok {
			return slog.String(a.Key, h.scrub(s.Error()))
		}
		return a
	default:
		return a
	}
}

func (h *redactingHandler) scrub(s string) string {
	for _, secret := range h.secrets {
		if secret != "" && strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redactionMarker)
		}
	}
	return s
}
