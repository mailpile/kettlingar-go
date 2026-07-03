// Package logtee provides an slog.Handler that fans log records out to live
// in-process subscribers, so an application can watch its own logs at runtime
// (for example, to stream an API call's progress) while still writing them
// normally. See logtee/README.md.
package logtee

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// LevelTrace is a level below Debug, for the noisiest detail.
const LevelTrace = slog.LevelDebug - 4

// subBuffer bounds each subscription's channel; sends beyond it are dropped so
// logging never blocks on a slow watcher.
const subBuffer = 256

// noSubSentinel is the "no subscribers" minimum level, above any real level.
const noSubSentinel = int64(127)

// hub is the shared state of all handlers derived via WithAttrs / WithGroup, so
// they fan out to the same subscribers and write to the same base handler. The
// base lives here (not on FanoutHandler) so SetBase can swap it for every
// derived handler at once, under mu.
type hub struct {
	level  *slog.LevelVar
	mu     sync.Mutex
	base   slog.Handler
	subs   map[int]*Subscription
	seq    int
	minSub atomic.Int64 // lowest subscriber level, for Enabled
}

// FanoutHandler forwards each record to a base handler and to matching
// subscribers. It is safe for concurrent use. A derived handler records its
// WithAttrs/WithGroup operations (baseOps) so the shared, swappable base can be
// re-derived on each Handle; attrs is the flat attribute set used to annotate
// subscriber records.
type FanoutHandler struct {
	hub     *hub
	attrs   []slog.Attr
	baseOps []func(slog.Handler) slog.Handler
}

// NewHandler wraps base. level is the runtime-adjustable global level that gates
// what reaches the base handler (subscribers may ask for more detail).
func NewHandler(base slog.Handler, level *slog.LevelVar) *FanoutHandler {
	h := &hub{level: level, base: base, subs: map[int]*Subscription{}}
	h.minSub.Store(noSubSentinel)
	return &FanoutHandler{hub: h}
}

// New builds a text-logging FanoutHandler over w at an initial level and returns
// a Logger using it plus the handler (for Subscribe).
func New(w io.Writer, level slog.Level) (*slog.Logger, *FanoutHandler) {
	lv := &slog.LevelVar{}
	lv.Set(level)
	base := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	h := NewHandler(base, lv)
	return slog.New(h), h
}

// SetLevel changes the global base level at runtime.
func (h *FanoutHandler) SetLevel(l slog.Level) { h.hub.level.Set(l) }

// SetBase swaps the base handler for every handler sharing this hub (the process
// default and any derived logger), so a service can reconfigure where logs are
// written at runtime without disturbing live subscribers or SetLevel.
func (h *FanoutHandler) SetBase(base slog.Handler) {
	h.hub.mu.Lock()
	h.hub.base = base
	h.hub.mu.Unlock()
}

// SetBaseFlushing swaps the base and replays records captured before the base
// was known (drain returns them, e.g. from a bootstrap BufferHandler), writing
// those at or above min into the new base first. drain runs and the swap happens
// under the hub lock, so no record is lost, duplicated, or ordered before the
// replayed ones: records written by a concurrent Handle either land in the old
// base and are picked up by drain, or block until the new base is installed.
func (h *FanoutHandler) SetBaseFlushing(base slog.Handler, drain func() []slog.Record, min slog.Level) {
	ctx := context.Background()
	h.hub.mu.Lock()
	defer h.hub.mu.Unlock()
	for _, r := range drain() {
		if r.Level >= min {
			_ = base.Handle(ctx, r)
		}
	}
	h.hub.base = base
}

// baseView applies this handler's recorded WithAttrs/WithGroup ops to the hub's
// current base. Callers hold hub.mu. The process-default handler has no ops, so
// this returns the base directly with no allocation.
func (h *FanoutHandler) baseView() slog.Handler {
	b := h.hub.base
	for _, op := range h.baseOps {
		b = op(b)
	}
	return b
}

func (h *FanoutHandler) Enabled(_ context.Context, l slog.Level) bool {
	eff := h.hub.level.Level()
	if m := slog.Level(h.hub.minSub.Load()); m < eff {
		eff = m
	}
	return l >= eff
}

func (h *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	sink := sinkFromContext(ctx)
	h.hub.mu.Lock()
	defer h.hub.mu.Unlock()
	for _, s := range h.hub.subs {
		if r.Level < s.level {
			continue
		}
		if !s.all && s != sink {
			continue
		}
		rec := r.Clone()
		rec.AddAttrs(h.attrs...) // include attrs added via With (groups not nested)
		select {
		case s.C <- rec:
		default:
			s.dropped.Add(1)
		}
	}

	// Only records at or above the global level reach the base handler; lower
	// records are produced solely for subscribers. The base write is done under
	// hub.mu so a concurrent SetBaseFlushing sees a coherent buffer and the swap
	// orders cleanly; log volume is low (lifecycle events), so this does not
	// matter for throughput.
	if r.Level >= h.hub.level.Level() {
		return h.baseView().Handle(ctx, r)
	}
	return nil
}

func (h *FanoutHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	nh := h.clone()
	nh.attrs = append(nh.attrs, as...)
	attrs := append([]slog.Attr(nil), as...)
	nh.baseOps = append(nh.baseOps, func(b slog.Handler) slog.Handler { return b.WithAttrs(attrs) })
	return nh
}

func (h *FanoutHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := h.clone()
	nh.baseOps = append(nh.baseOps, func(b slog.Handler) slog.Handler { return b.WithGroup(name) })
	return nh
}

func (h *FanoutHandler) clone() *FanoutHandler {
	return &FanoutHandler{
		hub:     h.hub,
		attrs:   append([]slog.Attr(nil), h.attrs...),
		baseOps: append([]func(slog.Handler) slog.Handler(nil), h.baseOps...),
	}
}

// Subscription is a live feed of log records. Read from C; call Close when done.
type Subscription struct {
	C       chan slog.Record
	level   slog.Level
	all     bool
	hub     *hub
	id      int
	dropped atomic.Int64
}

// Subscribe returns a subscription receiving every record at or above level.
func (h *FanoutHandler) Subscribe(level slog.Level) *Subscription {
	return h.hub.add(level, true)
}

// SubscribeContext returns a subscription that receives only records logged with
// the returned context (via slog's ...Context methods). The context derives from
// parent, so a caller can keep its own cancellation/deadline.
func (h *FanoutHandler) SubscribeContext(parent context.Context, level slog.Level) (*Subscription, context.Context) {
	s := h.hub.add(level, false)
	return s, withSink(parent, s)
}

func (h *hub) add(level slog.Level, all bool) *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	s := &Subscription{C: make(chan slog.Record, subBuffer), level: level, all: all, hub: h, id: h.seq}
	h.subs[s.id] = s
	h.recomputeMinLocked()
	return s
}

// Close deregisters the subscription and closes C. Sends and Close are mutually
// excluded by hub.mu, so no send races the close.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	if _, ok := s.hub.subs[s.id]; ok {
		delete(s.hub.subs, s.id)
		s.hub.recomputeMinLocked()
		close(s.C)
	}
	s.hub.mu.Unlock()
}

// Dropped reports records dropped because C was full.
func (s *Subscription) Dropped() int64 { return s.dropped.Load() }

func (h *hub) recomputeMinLocked() {
	m := noSubSentinel
	for _, s := range h.subs {
		if int64(s.level) < m {
			m = int64(s.level)
		}
	}
	h.minSub.Store(m)
}

type sinkKey struct{}

func withSink(ctx context.Context, s *Subscription) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

func sinkFromContext(ctx context.Context) *Subscription {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sinkKey{}).(*Subscription)
	return s
}

// ParseLevel maps a CLI level name to an slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("logtee: unknown level %q", s)
	}
}

// LevelString renders a level as a short upper-case name, including "TRACE" for
// the sub-debug LevelTrace (which slog would otherwise print as "DEBUG-4").
func LevelString(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "TRACE"
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// Format renders a record as a single line including the time (helpful for
// long-running CLI displays).
func Format(r slog.Record) string {
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(r.Time.Format("15:04:05.000"))
		b.WriteByte(' ')
	}
	b.WriteString(LevelString(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		fmt.Fprintf(&b, "%v", a.Value.Any())
		return true
	})
	return b.String()
}
