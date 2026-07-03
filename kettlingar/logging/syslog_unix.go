//go:build !windows && !plan9

package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"log/syslog"
	"strings"
	"sync"
)

// newSyslogHandler connects to syslog (local, or remote over UDP when Addr is
// set) and returns a handler that formats each record with the configured
// text/json format and sends it at the syslog severity mapped from its level.
func newSyslogHandler(cfg Config) (slog.Handler, io.Closer, error) {
	fac, err := syslogFacility(cfg.Syslog.Facility)
	if err != nil {
		return nil, nil, err
	}
	tag := cfg.Syslog.Tag
	if tag == "" {
		tag = "kettlingar"
	}
	var w *syslog.Writer
	if cfg.Syslog.Addr != "" {
		w, err = syslog.Dial("udp", cfg.Syslog.Addr, fac|syslog.LOG_INFO, tag)
	} else {
		w, err = syslog.New(fac|syslog.LOG_INFO, tag)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("logging: syslog: %w", err)
	}
	buf := &bytes.Buffer{}
	inner := formatHandler(buf, cfg.Format, &slog.HandlerOptions{Level: levelAll, ReplaceAttr: replaceAttr})
	return &syslogHandler{w: w, buf: buf, inner: inner, mu: &sync.Mutex{}}, w, nil
}

// syslogHandler formats each record via inner (into the shared buf, guarded by
// mu) and ships it to syslog at the mapped severity. Derived handlers share
// w/buf/mu so concurrent writes stay serialized.
type syslogHandler struct {
	w     *syslog.Writer
	buf   *bytes.Buffer
	inner slog.Handler
	mu    *sync.Mutex
}

func (h *syslogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *syslogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	msg := strings.TrimRight(h.buf.String(), "\n")
	switch {
	case r.Level >= slog.LevelError:
		return h.w.Err(msg)
	case r.Level >= slog.LevelWarn:
		return h.w.Warning(msg)
	case r.Level >= slog.LevelInfo:
		return h.w.Info(msg)
	default:
		return h.w.Debug(msg)
	}
}

func (h *syslogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &syslogHandler{w: h.w, buf: h.buf, inner: h.inner.WithAttrs(as), mu: h.mu}
}

func (h *syslogHandler) WithGroup(name string) slog.Handler {
	return &syslogHandler{w: h.w, buf: h.buf, inner: h.inner.WithGroup(name), mu: h.mu}
}

// syslogFacility maps a facility name to its syslog priority. Empty defaults to
// daemon.
func syslogFacility(name string) (syslog.Priority, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "daemon":
		return syslog.LOG_DAEMON, nil
	case "user":
		return syslog.LOG_USER, nil
	case "local0":
		return syslog.LOG_LOCAL0, nil
	case "local1":
		return syslog.LOG_LOCAL1, nil
	case "local2":
		return syslog.LOG_LOCAL2, nil
	case "local3":
		return syslog.LOG_LOCAL3, nil
	case "local4":
		return syslog.LOG_LOCAL4, nil
	case "local5":
		return syslog.LOG_LOCAL5, nil
	case "local6":
		return syslog.LOG_LOCAL6, nil
	case "local7":
		return syslog.LOG_LOCAL7, nil
	default:
		return 0, fmt.Errorf("logging: unknown syslog facility %q", name)
	}
}
