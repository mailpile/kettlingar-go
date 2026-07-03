package logtee

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func recv(t *testing.T, c <-chan slog.Record) (slog.Record, bool) {
	t.Helper()
	select {
	case r, ok := <-c:
		return r, ok
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a record")
		return slog.Record{}, false
	}
}

func TestSubscribeAllAndLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	logger, h := New(&buf, slog.LevelInfo)

	sub := h.Subscribe(slog.LevelInfo)
	defer sub.Close()

	logger.Debug("dbg")            // below sub level -> not delivered
	logger.Info("hello", "k", "v") // delivered

	r, ok := recv(t, sub.C)
	if !ok || r.Message != "hello" {
		t.Fatalf("record = %+v ok=%v, want hello", r, ok)
	}
	select {
	case extra := <-sub.C:
		t.Errorf("unexpected extra record %q (debug should be filtered)", extra.Message)
	default:
	}
}

func TestSubscriberSeesBelowBaseLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, h := New(&buf, slog.LevelInfo) // base writes Info+
	sub := h.Subscribe(slog.LevelDebug)    // subscriber wants Debug+
	defer sub.Close()

	logger.Debug("only-for-sub")
	r, _ := recv(t, sub.C)
	if r.Message != "only-for-sub" {
		t.Errorf("subscriber should receive Debug below base level; got %q", r.Message)
	}
	if strings.Contains(buf.String(), "only-for-sub") {
		t.Errorf("base handler should not write below its level; got %q", buf.String())
	}
}

func TestSubscribeContextRouting(t *testing.T) {
	logger, h := New(&bytes.Buffer{}, slog.LevelDebug)

	sub, ctx := h.SubscribeContext(context.Background(), slog.LevelDebug)
	defer sub.Close()

	logger.InfoContext(context.Background(), "other") // no sink -> not to this sub
	logger.InfoContext(ctx, "mine")                   // routed to this sub

	r, _ := recv(t, sub.C)
	if r.Message != "mine" {
		t.Errorf("context sub received %q, want only the context-routed record", r.Message)
	}
	select {
	case extra := <-sub.C:
		t.Errorf("context sub got unrouted record %q", extra.Message)
	default:
	}
}

func TestNonBlockingDrop(t *testing.T) {
	logger, h := New(&bytes.Buffer{}, slog.LevelInfo)
	sub := h.Subscribe(slog.LevelInfo)
	defer sub.Close()
	// Fill far beyond the buffer without draining; must not block.
	for i := 0; i < subBuffer*3; i++ {
		logger.Info("flood")
	}
	if sub.Dropped() == 0 {
		t.Error("expected some records to be dropped when the channel is full")
	}
}

func TestCloseDeregisters(t *testing.T) {
	logger, h := New(&bytes.Buffer{}, slog.LevelInfo)
	sub := h.Subscribe(slog.LevelInfo)
	sub.Close()
	if _, ok := <-sub.C; ok {
		t.Error("C should be closed after Close")
	}
	// Logging after close must not panic.
	logger.Info("after-close")
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"trace": LevelTrace, "debug": slog.LevelDebug, "info": slog.LevelInfo,
		"": slog.LevelInfo, "WARN": slog.LevelWarn, "error": slog.LevelError,
	}
	for in, want := range cases {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("bogus"); err == nil {
		t.Error("ParseLevel(bogus) should error")
	}
}

func TestFormat(t *testing.T) {
	r := slog.NewRecord(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC), slog.LevelWarn, "msg", 0)
	r.AddAttrs(slog.String("k", "v"))
	got := Format(r)
	if !strings.Contains(got, "WARN") || !strings.Contains(got, "msg") || !strings.Contains(got, "k=v") || !strings.Contains(got, "03:04:05") {
		t.Errorf("Format = %q, want time+WARN+msg+k=v", got)
	}
}

func TestSetLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, h := New(&buf, slog.LevelInfo)
	logger.Debug("before") // not written (Info level)
	h.SetLevel(slog.LevelDebug)
	logger.Debug("after") // now written
	if strings.Contains(buf.String(), "before") || !strings.Contains(buf.String(), "after") {
		t.Errorf("SetLevel did not adjust base verbosity: %q", buf.String())
	}
}

func TestSetBaseSwapsSinkLive(t *testing.T) {
	var a, b bytes.Buffer
	logger, h := New(&a, slog.LevelInfo)
	logger.Info("to-a")
	h.SetBase(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("to-b")
	if !strings.Contains(a.String(), "to-a") || strings.Contains(a.String(), "to-b") {
		t.Errorf("first sink should hold only pre-swap records: %q", a.String())
	}
	if !strings.Contains(b.String(), "to-b") || strings.Contains(b.String(), "to-a") {
		t.Errorf("second sink should hold only post-swap records: %q", b.String())
	}
}

func TestSetBaseKeepsSubscribers(t *testing.T) {
	var a, b bytes.Buffer
	logger, h := New(&a, slog.LevelInfo)
	sub := h.Subscribe(slog.LevelInfo)
	defer sub.Close()
	h.SetBase(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("after-swap")
	rec, ok := recv(t, sub.C)
	if !ok || rec.Message != "after-swap" {
		t.Errorf("subscriber should keep receiving across a base swap, got %v/%q", ok, rec.Message)
	}
}

func TestSetBaseFlushingReplaysBuffered(t *testing.T) {
	// Bootstrap with a base that only captures into a slice; then flush into a
	// real sink, honoring the configured min level.
	var captured []slog.Record
	lv := &slog.LevelVar{}
	lv.Set(LevelTrace) // capture everything during bootstrap
	cap := &sliceHandler{recs: &captured}
	h := NewHandler(cap, lv)
	logger := slog.New(h)
	logger.Debug("dbg")
	logger.Info("nfo")

	var out bytes.Buffer
	drain := func() []slog.Record { r := captured; captured = nil; return r }
	h.SetBaseFlushing(slog.NewTextHandler(&out, nil), drain, slog.LevelInfo)
	h.SetLevel(slog.LevelInfo)

	if strings.Contains(out.String(), "dbg") {
		t.Errorf("debug record should be filtered by the configured min level: %q", out.String())
	}
	if !strings.Contains(out.String(), "nfo") {
		t.Errorf("info record should be flushed to the new base: %q", out.String())
	}
	logger.Info("later")
	if !strings.Contains(out.String(), "later") {
		t.Errorf("post-flush records should reach the new base: %q", out.String())
	}
}

// sliceHandler is a minimal capture handler for the flush test.
type sliceHandler struct{ recs *[]slog.Record }

func (s *sliceHandler) Enabled(context.Context, slog.Level) bool { return true }
func (s *sliceHandler) Handle(_ context.Context, r slog.Record) error {
	*s.recs = append(*s.recs, r.Clone())
	return nil
}
func (s *sliceHandler) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *sliceHandler) WithGroup(string) slog.Handler      { return s }
