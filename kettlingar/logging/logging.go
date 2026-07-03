// Package logging builds the base slog.Handler a kettlingar service writes its
// logs to, from configuration: a destination (stderr, a rotating file, syslog,
// or nothing), a format (text or json) and a level. It also provides
// BufferHandler, the bootstrap in-memory sink that captures records emitted
// before logging is configured so they can be flushed into the real destination
// once it is known.
//
// It composes with logtee's FanoutHandler: the handler built here is installed
// as the fan-out's base (all logs), while the fan-out's subscribers (a service's
// log-streaming API methods) stay independent -- a record reaches both.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/mailpile/kettlingar-go/kettlingar/logtee"
)

// Config is the resolved logging configuration BaseHandler turns into a handler.
type Config struct {
	Target string // "stderr", "file", "syslog", "none"
	Format string // "text" or "json"
	Level  slog.Level
	Source bool // add a source file:line attribute to each record
	File   FileConfig
	Syslog SyslogConfig
}

// secretKeys lists attribute keys whose values must never be written to a log,
// as a safety net against a careless call site: any attr with one of these keys
// is replaced with a redacted marker before it reaches a destination handler.
// The base set is generic; applications extend it with RegisterSecretKey.
var secretKeys = map[string]bool{
	"secret":        true,
	"password":      true,
	"authorization": true,
}

// RegisterSecretKey adds attribute keys to the redaction denylist, so an
// application can protect its own domain-specific secret-valued keys (e.g. a
// service that logs a "dns_update_secret"). Call it during initialization,
// before logging starts.
func RegisterSecretKey(keys ...string) {
	for _, k := range keys {
		secretKeys[k] = true
	}
}

// Redact reports whether an attribute key holds a secret and so must have its
// value replaced before logging. Exported so the API log-streaming path can
// apply the same denylist to records it forwards to a watching client.
func Redact(key string) bool { return secretKeys[key] }

// replaceAttr normalizes records for every destination handler: it labels the
// trace level as "TRACE" (slog would otherwise print "DEBUG-4") and redacts any
// secret-valued attribute. It is applied to both the text and json handlers.
func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		if lv, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(logtee.LevelString(lv))
		}
		return a
	}
	if Redact(a.Key) {
		a.Value = slog.StringValue("[redacted]")
	}
	return a
}

// FileConfig configures the rotating file destination. A zero MaxSizeMB disables
// size rotation; zero MaxBackups/MaxAgeDays disable those limits.
type FileConfig struct {
	Path       string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Compress   bool
}

// SyslogConfig configures the syslog destination. Addr empty means the local
// syslog daemon; otherwise it is a "host:port" for a remote server (UDP).
type SyslogConfig struct {
	Addr     string
	Facility string
	Tag      string
}

// BaseHandler builds the base handler for cfg and a Closer that releases its
// sink (file handle or syslog connection) when the handler is swapped out. The
// Closer is a no-op for stderr and none.
func BaseHandler(cfg Config) (slog.Handler, io.Closer, error) {
	opts := &slog.HandlerOptions{Level: cfg.Level, AddSource: cfg.Source, ReplaceAttr: replaceAttr}
	switch cfg.Target {
	case "none", "off", "discard":
		return discardHandler{}, noopCloser{}, nil
	case "", "stderr":
		return formatHandler(os.Stderr, cfg.Format, opts), noopCloser{}, nil
	case "stdout":
		return formatHandler(os.Stdout, cfg.Format, opts), noopCloser{}, nil
	case "file":
		w, err := newRotatingWriter(cfg.File)
		if err != nil {
			return nil, nil, fmt.Errorf("logging: open log file: %w", err)
		}
		return formatHandler(w, cfg.Format, opts), w, nil
	case "syslog":
		return newSyslogHandler(cfg)
	default:
		return nil, nil, fmt.Errorf("logging: unknown log target %q", cfg.Target)
	}
}

// levelAll is below every real level, so a handler built with it formats every
// record (used for the syslog inner handler, where the syslog severity, not a
// handler-level filter, decides what is sent).
const levelAll = slog.Level(-128)

// formatHandler builds a text or json handler over w.
func formatHandler(w io.Writer, format string, opts *slog.HandlerOptions) slog.Handler {
	if format == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// discardHandler drops every record; used for the "none" target. Subscribers on
// the fan-out are unaffected, so API log streaming still works with logging off.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
