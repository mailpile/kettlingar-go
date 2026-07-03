package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func rec(level slog.Level, msg string) slog.Record {
	return slog.NewRecord(time.Now(), level, msg, 0)
}

func TestBaseHandlerFormats(t *testing.T) {
	dir := t.TempDir()
	for _, format := range []string{"text", "json"} {
		path := filepath.Join(dir, format+".log")
		h, closer, err := BaseHandler(Config{Target: "file", Format: format, Level: slog.LevelInfo,
			File: FileConfig{Path: path}})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		_ = h.Handle(context.Background(), rec(slog.LevelInfo, "hello"))
		closer.Close()
		data, _ := os.ReadFile(path)
		body := string(data)
		if !strings.Contains(body, "hello") {
			t.Errorf("%s: message missing: %q", format, body)
		}
		if format == "json" && !strings.Contains(body, `"msg":"hello"`) {
			t.Errorf("json format not used: %q", body)
		}
	}
}

func TestBaseHandlerJSONShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.log")
	h, closer, err := BaseHandler(Config{Target: "file", Format: "json", Level: slog.LevelInfo,
		File: FileConfig{Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Handle(context.Background(), rec(slog.LevelInfo, "hi"))
	closer.Close()
	body := string(mustRead(t, path))
	for _, want := range []string{`"time":`, `"level":"INFO"`, `"msg":"hi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("json line missing %s: %q", want, body)
		}
	}
}

func TestReplaceAttrRedactsAndLabelsTrace(t *testing.T) {
	dir := t.TempDir()

	// A secret-valued attribute is redacted, not written verbatim.
	secPath := filepath.Join(dir, "sec.log")
	h, closer, _ := BaseHandler(Config{Target: "file", Format: "json", Level: slog.LevelInfo,
		File: FileConfig{Path: secPath}})
	r := rec(slog.LevelInfo, "with secret")
	r.AddAttrs(slog.String("secret", "hunter2"), slog.String("password", "swordfish"))
	_ = h.Handle(context.Background(), r)
	closer.Close()
	body := string(mustRead(t, secPath))
	if strings.Contains(body, "hunter2") || strings.Contains(body, "swordfish") {
		t.Errorf("secret leaked into log: %q", body)
	}
	if !strings.Contains(body, "[redacted]") {
		t.Errorf("expected redaction marker: %q", body)
	}

	// The trace level is labeled TRACE, not the raw DEBUG-4.
	tracePath := filepath.Join(dir, "trace.log")
	th, tcloser, _ := BaseHandler(Config{Target: "file", Format: "json", Level: slog.Level(-128),
		File: FileConfig{Path: tracePath}})
	_ = th.Handle(context.Background(), rec(slog.LevelDebug-4, "noisy"))
	tcloser.Close()
	tbody := string(mustRead(t, tracePath))
	if !strings.Contains(tbody, `"level":"TRACE"`) {
		t.Errorf("trace level not labeled TRACE: %q", tbody)
	}
}

func TestBaseHandlerAddSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.log")
	h, closer, _ := BaseHandler(Config{Target: "file", Format: "json", Level: slog.LevelInfo,
		Source: true, File: FileConfig{Path: path}})
	// A record needs a real PC for source resolution; capture this call site.
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "traced", callerPC())
	_ = h.Handle(context.Background(), r)
	closer.Close()
	if body := string(mustRead(t, path)); !strings.Contains(body, `"source":`) {
		t.Errorf("AddSource should add a source attribute: %q", body)
	}
}

func callerPC() uintptr {
	var pcs [1]uintptr
	runtime.Callers(2, pcs[:])
	return pcs[0]
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func TestBaseHandlerNoneDiscards(t *testing.T) {
	h, closer, err := BaseHandler(Config{Target: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if h.Enabled(context.Background(), slog.LevelError) {
		t.Error("none target should report disabled")
	}
}

func TestBaseHandlerUnknownTarget(t *testing.T) {
	if _, _, err := BaseHandler(Config{Target: "bogus"}); err == nil {
		t.Error("unknown target should error")
	}
}

func TestRotatingWriterRotatesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := newRotatingWriter(FileConfig{Path: path, MaxSizeMB: 0, MaxBackups: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Force rotation by using a tiny threshold directly.
	w.maxBytes = 20
	for i := 0; i < 6; i++ {
		if _, err := w.Write([]byte("0123456789ABCDEF\n")); err != nil { // 17 bytes
			t.Fatal(err)
		}
	}
	w.Close()

	backups := backupsFor(path)
	// MaxBackups=2 -> at most name.1 and name.2 (plus the live file, which
	// backupsFor also matches by prefix only if it had a dot; it does not).
	if len(backups) > 2 {
		t.Errorf("expected at most 2 backups, got %v", backups)
	}
	if len(backups) == 0 {
		t.Errorf("expected rotation to have produced backups")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("live log file should exist after rotation: %v", err)
	}
}

func TestRotatingWriterCompress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, _ := newRotatingWriter(FileConfig{Path: path, MaxBackups: 2, Compress: true})
	w.maxBytes = 10
	for i := 0; i < 4; i++ {
		w.Write([]byte("some log line\n"))
	}
	w.Close()
	var gz bool
	for _, b := range backupsFor(path) {
		if strings.HasSuffix(b, ".gz") {
			gz = true
		}
	}
	if !gz {
		t.Errorf("compressed rotation should produce a .gz backup, got %v", backupsFor(path))
	}
}

func TestBufferHandlerCapAndDrain(t *testing.T) {
	b := NewBufferHandler(2)
	for _, m := range []string{"a", "b", "c"} { // cap 2 -> "a" dropped
		_ = b.Handle(context.Background(), rec(slog.LevelInfo, m))
	}
	if b.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", b.Dropped())
	}
	got := b.Drain()
	if len(got) != 2 || got[0].Message != "b" || got[1].Message != "c" {
		t.Fatalf("drain order/content wrong: %+v", got)
	}
	// After drain the buffer is closed and ignores further records.
	_ = b.Handle(context.Background(), rec(slog.LevelInfo, "d"))
	if len(b.Drain()) != 0 {
		t.Error("buffer should be closed after drain")
	}
}

func TestTeeRoutesByEnabled(t *testing.T) {
	buf := NewBufferHandler(10)
	var errOut testHandler
	tee := Tee(buf, &errOut) // errOut only enabled at >= WARN
	_ = tee.Handle(context.Background(), rec(slog.LevelInfo, "info"))
	_ = tee.Handle(context.Background(), rec(slog.LevelWarn, "warn"))
	if len(buf.Drain()) != 2 {
		t.Error("buffer branch should capture both records")
	}
	if errOut.got != 1 || errOut.last != "warn" {
		t.Errorf("stderr branch should only see the warn record, got %d/%q", errOut.got, errOut.last)
	}
}

// testHandler stands in for the bootstrap stderr@WARN handler.
type testHandler struct {
	got  int
	last string
}

func (h *testHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (h *testHandler) Handle(_ context.Context, r slog.Record) error {
	h.got++
	h.last = r.Message
	return nil
}
func (h *testHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *testHandler) WithGroup(string) slog.Handler      { return h }
