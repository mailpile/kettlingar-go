package logging

import (
	"context"
	"log/slog"
	"sync"
)

// BufferHandler is the bootstrap sink: a bounded, in-memory ring of records
// emitted before logging is configured. It writes nowhere itself; Drain hands
// the captured records to the real base handler once it is known (see
// logtee.FanoutHandler.SetBaseFlushing). Records are cloned on capture because a
// slog.Record's attributes are only valid during Handle.
type BufferHandler struct {
	mu      sync.Mutex
	cap     int
	recs    []slog.Record
	dropped int
	closed  bool
}

// NewBufferHandler returns a buffer holding at most capacity records; once full
// the oldest is dropped (counted by Dropped) so an app that never configures
// logging cannot grow memory without bound.
func NewBufferHandler(capacity int) *BufferHandler {
	if capacity < 1 {
		capacity = 1
	}
	return &BufferHandler{cap: capacity}
}

// Enabled captures at every level; the fan-out gates what is produced.
func (b *BufferHandler) Enabled(context.Context, slog.Level) bool { return true }

func (b *BufferHandler) Handle(_ context.Context, r slog.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	if len(b.recs) >= b.cap {
		copy(b.recs, b.recs[1:])
		b.recs[len(b.recs)-1] = r.Clone()
		b.dropped++
		return nil
	}
	b.recs = append(b.recs, r.Clone())
	return nil
}

// A buffer holds raw records; With* are no-ops (bootstrap logging adds no attrs).
func (b *BufferHandler) WithAttrs([]slog.Attr) slog.Handler { return b }
func (b *BufferHandler) WithGroup(string) slog.Handler      { return b }

// Drain returns the captured records in emission order and marks the buffer
// closed, so later Handle calls are ignored. Safe to pass as the drain callback
// to SetBaseFlushing.
func (b *BufferHandler) Drain() []slog.Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.recs
	b.recs = nil
	b.closed = true
	return out
}

// Dropped reports how many records were discarded because the buffer was full.
func (b *BufferHandler) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Tee returns a handler that forwards each record to all of hs (each gated by
// its own Enabled). It is used for the bootstrap base: a BufferHandler capturing
// everything plus a stderr handler gated at WARN, so urgent startup problems are
// visible on the console even before logging is configured.
func Tee(hs ...slog.Handler) slog.Handler { return teeHandler{hs: hs} }

type teeHandler struct{ hs []slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t.hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range t.hs {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r)
		}
	}
	return nil
}

func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		out[i] = h.WithAttrs(as)
	}
	return teeHandler{hs: out}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(t.hs))
	for i, h := range t.hs {
		out[i] = h.WithGroup(name)
	}
	return teeHandler{hs: out}
}
