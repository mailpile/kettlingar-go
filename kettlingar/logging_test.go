package kettlingar

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mailpile/kettlingar-go/kettlingar/logging"
)

func TestResolveLogTarget(t *testing.T) {
	for _, in := range []string{"", "-", "auto"} {
		if got := resolveLogTarget(in); got != "file" {
			t.Errorf("resolveLogTarget(%q) = %q, want file", in, got)
		}
	}
	if got := resolveLogTarget("syslog"); got != "syslog" {
		t.Errorf("explicit target should pass through, got %q", got)
	}
}

func TestResolveLogFormat(t *testing.T) {
	// Explicit format always wins over the destination heuristic.
	if got := resolveLogFormat("text", "file"); got != "text" {
		t.Errorf("explicit text should win, got %q", got)
	}
	if got := resolveLogFormat("json", "stderr"); got != "json" {
		t.Errorf("explicit json should win, got %q", got)
	}
	// auto: machine sinks get json.
	for _, target := range []string{"file", "syslog"} {
		if got := resolveLogFormat("auto", target); got != "json" {
			t.Errorf("auto+%s = %q, want json", target, got)
		}
	}
	// auto + stderr follows the tty check (stubbed via the seam).
	defer func(orig func() bool) { stderrIsTTY = orig }(stderrIsTTY)
	stderrIsTTY = func() bool { return true }
	if got := resolveLogFormat("auto", "stderr"); got != "text" {
		t.Errorf("auto+stderr on a tty = %q, want text", got)
	}
	stderrIsTTY = func() bool { return false }
	if got := resolveLogFormat("auto", "stderr"); got != "json" {
		t.Errorf("auto+stderr piped = %q, want json", got)
	}
}

func TestConfigureLoggingFlushesBufferToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	ks := &KettlingarService{Name: "svc"}
	ks.InstallLogging()
	logger := ks.Logger

	// A subscriber attached during the unconfigured window must keep working
	// across the base swap (the log-streaming feature).
	sub := ks.logs.Subscribe(slog.LevelDebug)
	defer sub.Close()

	// Emitted before logging is configured: captured in the buffer.
	logger.Debug("early-debug")
	logger.Info("early-info")

	// First apply: build the file sink and flush the buffer at the configured level.
	ks.ConfigureLogging(LogSettings{
		Target: "file", Level: slog.LevelInfo, Format: "text",
		File: logging.FileConfig{Path: path},
	})

	body := readFile(t, path)
	if !strings.Contains(body, "early-info") {
		t.Errorf("buffered info record should be flushed to the file: %q", body)
	}
	if strings.Contains(body, "early-debug") {
		t.Errorf("buffered debug record should be dropped at the configured info level: %q", body)
	}

	// Post-configuration records reach the file too.
	logger.Info("later")
	if body = readFile(t, path); !strings.Contains(body, "later") {
		t.Errorf("post-flush record missing from file: %q", body)
	}

	// The subscriber saw the records regardless of where the base wrote.
	var seen []string
	for len(sub.C) > 0 {
		seen = append(seen, (<-sub.C).Message)
	}
	if !strSliceContains(seen, "early-info") || !strSliceContains(seen, "later") {
		t.Errorf("subscriber should receive records across the swap: %v", seen)
	}
}

func TestConfigureLoggingReconfigureAndKey(t *testing.T) {
	dir := t.TempDir()
	ks := &KettlingarService{Name: "svc"}
	ks.InstallLogging()
	logger := ks.Logger

	apply := func(path, level string) {
		lvl, _ := parseLevelOrInfo(level)
		ks.ConfigureLogging(LogSettings{
			Target: "file", Level: lvl, Format: "text",
			File: logging.FileConfig{Path: path},
		})
	}

	apply(filepath.Join(dir, "a.log"), "info")
	firstKey := ks.logKey

	// Re-applying with no change keeps the same key (sink not reopened).
	apply(filepath.Join(dir, "a.log"), "info")
	if ks.logKey != firstKey {
		t.Errorf("unchanged config should keep the same key")
	}

	// Point at a new file and drop to debug; records now land there.
	apply(filepath.Join(dir, "b.log"), "debug")
	if ks.logKey == firstKey {
		t.Errorf("changed config should change the key")
	}
	logger.Debug("to-b")
	if body := readFile(t, filepath.Join(dir, "b.log")); !strings.Contains(body, "to-b") {
		t.Errorf("debug record should reach the reconfigured file: %q", body)
	}
}

func TestResolveLogConfigNeverEmptyFilePath(t *testing.T) {
	// A file target must resolve to a real path (the service default) or downgrade
	// to stderr, never a file target with an empty path.
	ks := &KettlingarService{Name: "svc"}
	cfg, _ := ks.resolveLogConfig(LogSettings{Target: "file", Level: slog.LevelInfo})
	if cfg.Target == "file" && cfg.File.Path == "" {
		t.Errorf("file target must never resolve to an empty path")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func strSliceContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func parseLevelOrInfo(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, true
	}
}
