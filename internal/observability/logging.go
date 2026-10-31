// Package observability provides structured logging with secret redaction.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// LogOptions configures NewLogger.
type LogOptions struct {
	Level  string // debug, info, warn, error
	Format string // json, text
}

// NewLogger builds a slog.Logger whose handler redacts sensitive attributes.
func NewLogger(w io.Writer, opts LogOptions) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(defaultString(opts.Level, "info")))); err != nil {
		return nil, fmt.Errorf("invalid log level %q", opts.Level)
	}
	hopts := &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr}
	switch defaultString(opts.Format, "json") {
	case "json":
		return slog.New(slog.NewJSONHandler(w, hopts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, hopts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q", opts.Format)
	}
}

func defaultString(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

const redacted = "[REDACTED]"

var (
	sensitiveKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|credential|private.?key|api.?key|authorization|cookie|dsn)`)
	// Credentials that must never reach a log even when embedded in free text.
	// The patterns are generic shapes (bearer headers, JWTs, URL userinfo, PEM
	// blocks and long prefixed tokens), not vendor integrations.
	sensitiveText = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(bearer|token|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+`),
		regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`),
		regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
		regexp.MustCompile(`\b[a-z]{2,8}[-_][A-Za-z0-9]{0,8}[-_]?[A-Za-z0-9_-]{32,}\b`),
	}
)

// IsSensitiveKey reports whether a key name likely designates a secret.
func IsSensitiveKey(key string) bool { return sensitiveKey.MatchString(key) }

// RedactText removes credential-shaped substrings from free text. It is a
// defense-in-depth measure for agent and worker output.
func RedactText(s string) string {
	for _, re := range sensitiveText {
		s = re.ReplaceAllString(s, redacted)
	}
	return s
}

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if v := a.Value.String(); v != "" {
			if r := RedactText(v); r != v {
				return slog.String(a.Key, r)
			}
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, RedactText(err.Error()))
		}
	}
	return a
}

type loggerKey struct{}

// WithLogger stores a logger in ctx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger returns the logger stored in ctx, or slog.Default.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
