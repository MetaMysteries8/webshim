package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const secret = "sk-hyper-supersecretvalue123456"

// newTestLogger builds a redacting logger over an in-memory buffer.
func newTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(&redactingHandler{next: base, secrets: []string{secret}}), &buf
}

// TestRedactionCoversEveryPath is the real guarantee: a secret cannot reach the
// log through the message, an attribute, a wrapped error, a group, or a
// pre-bound With attribute.
func TestRedactionCoversEveryPath(t *testing.T) {
	t.Parallel()

	cases := map[string]func(l *slog.Logger){
		"message": func(l *slog.Logger) {
			l.Info("token is " + secret)
		},
		"string attribute": func(l *slog.Logger) {
			l.Info("request failed", "authorization", "Bearer "+secret)
		},
		"wrapped error": func(l *slog.Logger) {
			err := fmt.Errorf("upstream: %w", errors.New("rejected Bearer "+secret))
			l.Error("boom", "error", err)
		},
		"group": func(l *slog.Logger) {
			l.Info("headers", slog.Group("http", slog.String("auth", secret)))
		},
		"With attrs": func(l *slog.Logger) {
			l.With("bound", secret).Info("something happened")
		},
		"WithGroup": func(l *slog.Logger) {
			l.WithGroup("req").Info("sent", "auth", secret)
		},
	}

	for name, emit := range cases {
		t.Run(name, func(t *testing.T) {
			logger, buf := newTestLogger(t)
			emit(logger)

			out := buf.String()
			if out == "" {
				t.Fatal("nothing was logged")
			}
			if strings.Contains(out, secret) {
				t.Errorf("the secret reached the log: %s", out)
			}
			if !strings.Contains(out, redactionMarker) {
				t.Errorf("expected a redaction marker in: %s", out)
			}
		})
	}
}

// TestNonSecretsSurviveIntact guards against over-redaction making logs useless.
func TestNonSecretsSurviveIntact(t *testing.T) {
	t.Parallel()

	logger, buf := newTestLogger(t)
	logger.Info("published", "project", "demo", "version", 12)

	out := buf.String()
	for _, want := range []string{"published", "demo", "12"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in the log: %s", want, out)
		}
	}
	if strings.Contains(out, redactionMarker) {
		t.Errorf("redacted something it should not have: %s", out)
	}
}

// TestShortSecretsAreIgnored prevents a stray short value from mangling every
// line of the log.
func TestShortSecretsAreIgnored(t *testing.T) {
	t.Parallel()

	if got := filterSecrets([]string{"abc", "", "   ", "long-enough-value"}); len(got) != 1 {
		t.Errorf("filterSecrets kept %v, want only the long value", got)
	}
}

func TestNoSecretsIsAPassThrough(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, nil)
	h := &redactingHandler{next: base}

	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled should delegate to the wrapped handler")
	}
	slog.New(h).Info("plain message", "k", "v")
	if !strings.Contains(buf.String(), "plain message") {
		t.Errorf("message did not reach the handler: %s", buf.String())
	}
}
