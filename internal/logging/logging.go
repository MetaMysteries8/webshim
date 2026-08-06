// Package logging sets up webshim's logger.
//
// The TUI owns the terminal, so log output must never go to stdout or stderr
// while it is running: a stray log line would corrupt the rendered frame. Logs
// go to a file instead, and the file is what `webshim doctor` and `/log` point
// at.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	charmlog "charm.land/log/v2"

	"github.com/MetaMysteries8/webshim/internal/config"
)

// FileName is the log file inside the state directory.
const FileName = "webshim.log"

// Options configures the logger.
type Options struct {
	// Level is the minimum level to record.
	Level slog.Level

	// Console sends human-readable output to stderr as well. Safe only for
	// non-TUI subcommands.
	Console bool

	// Secrets are values that must never appear in the log. Any log line
	// containing one is rewritten before it is written.
	Secrets []string
}

// Logger is a configured logger plus the path it writes to.
type Logger struct {
	*slog.Logger

	// Path is the log file, for showing the operator where to look.
	Path string

	closers []io.Closer
}

// Close releases the log file.
func (l *Logger) Close() error {
	var firstErr error
	for _, c := range l.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// New builds a logger that writes to the state directory, and optionally to
// stderr.
func New(opts Options) (*Logger, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, fmt.Errorf("locating the state directory: %w", err)
	}
	path := filepath.Join(dir, FileName)

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// The file gets logfmt: greppable, and free of ANSI escapes that would
	// make the log unreadable in a pager.
	fileLogger := charmlog.NewWithOptions(file, charmlog.Options{
		Level:           charmLevel(opts.Level),
		ReportTimestamp: true,
		Formatter:       charmlog.LogfmtFormatter,
	})

	var handler slog.Handler = fileLogger
	if opts.Console {
		console := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
			Level:           charmLevel(opts.Level),
			ReportTimestamp: false,
		})
		handler = teeHandler{a: fileLogger, b: console}
	}

	// Redaction wraps everything, so nothing reaches either destination
	// without passing through it.
	handler = &redactingHandler{next: handler, secrets: filterSecrets(opts.Secrets)}

	return &Logger{
		Logger:  slog.New(handler),
		Path:    path,
		closers: []io.Closer{file},
	}, nil
}

// Discard returns a logger that writes nowhere, for tests.
func Discard() *Logger {
	return &Logger{Logger: slog.New(slog.DiscardHandler)}
}

func charmLevel(l slog.Level) charmlog.Level {
	switch {
	case l <= slog.LevelDebug:
		return charmlog.DebugLevel
	case l <= slog.LevelInfo:
		return charmlog.InfoLevel
	case l <= slog.LevelWarn:
		return charmlog.WarnLevel
	default:
		return charmlog.ErrorLevel
	}
}

// filterSecrets drops values too short to redact safely. Redacting a two-
// character string would mangle every log line.
func filterSecrets(in []string) []string {
	var out []string
	for _, s := range in {
		if len(strings.TrimSpace(s)) >= 8 {
			out = append(out, s)
		}
	}
	return out
}

// teeHandler fans a record out to two handlers.
type teeHandler struct{ a, b slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if t.a.Enabled(ctx, r.Level) {
		if err := t.a.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	if t.b.Enabled(ctx, r.Level) {
		return t.b.Handle(ctx, r.Clone())
	}
	return nil
}

func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return teeHandler{a: t.a.WithAttrs(as), b: t.b.WithAttrs(as)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}
