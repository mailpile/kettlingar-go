package kettlingar

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mailpile/kettlingar-go/kettlingar/logging"
	"github.com/mailpile/kettlingar-go/kettlingar/logtee"
)

// logBufferCapacity bounds the bootstrap in-memory log buffer (records emitted
// before logging is configured). Generous, but capped so an unconfigured process
// cannot grow memory without bound.
const logBufferCapacity = 2048

// LogSettings is the raw, user-facing logging configuration a service exposes
// through its config file / flags. ConfigureLogging resolves the "auto"
// sentinels (target, format) and the default file path before applying it. Every
// field is optional; the zero value means "use the default".
type LogSettings struct {
	Target string // "auto" (default), "stderr", "stdout", "file", "syslog", "none"
	Level  slog.Level
	Format string // "auto" (default), "text", "json"
	Source bool   // add a source file:line attribute to each record
	File   logging.FileConfig
	Syslog logging.SyslogConfig
}

// InstallLogging sets up the process logging fan-out and makes it the default
// slog logger, so a service already logs before its destination is configured.
// The fan-out's base is a bootstrap tee: a BufferHandler capturing every record
// (flushed into the real sink by the first ConfigureLogging) plus a stderr
// handler gated at WARN, so urgent startup problems are visible immediately.
// Every line is tagged with the service name. Idempotent; called from
// MakeService, and safe for a caller to rely on being in effect.
func (ks *KettlingarService) InstallLogging() {
	ks.logMu.Lock()
	defer ks.logMu.Unlock()
	if ks.logs != nil {
		return
	}
	buf := logging.NewBufferHandler(logBufferCapacity)
	stderrWarn := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	lv := &slog.LevelVar{}
	lv.Set(logtee.LevelTrace) // capture everything until the level is configured
	fan := logtee.NewHandler(logging.Tee(buf, stderrWarn), lv)
	ks.logs = fan
	ks.logBuffer = buf
	logger := slog.New(fan).With("service", ks.Name)
	slog.SetDefault(logger)
	ks.Logger = logger
}

// LogFanout returns the process log fan-out, so a service's log-streaming API
// methods can subscribe to records by level. It is nil only if InstallLogging
// was never called.
func (ks *KettlingarService) LogFanout() *logtee.FanoutHandler {
	ks.logMu.Lock()
	defer ks.logMu.Unlock()
	return ks.logs
}

// ConfigureLogging resolves s and installs the resulting sink as the fan-out
// base. On the first call it flushes the bootstrap buffer into the sink (at the
// configured level); later calls swap the base only when the settings actually
// changed, so an unrelated reconfigure does not reopen the file or reconnect
// syslog. A bad target (e.g. syslog unavailable) falls back to stderr with a
// warning rather than silencing the service. It is a no-op when InstallLogging
// was not called.
func (ks *KettlingarService) ConfigureLogging(s LogSettings) {
	ks.logMu.Lock()
	defer ks.logMu.Unlock()
	if ks.logs == nil {
		return
	}
	cfg, key := ks.resolveLogConfig(s)
	if ks.logApplied && key == ks.logKey {
		return
	}
	base, closer, err := logging.BaseHandler(cfg)
	if err != nil {
		ks.Logger.Warn(ks.Name+": logging falling back to stderr", "target", cfg.Target, "err", err)
		cfg.Target = "stderr"
		base, closer, _ = logging.BaseHandler(cfg)
	}
	if ks.logApplied {
		ks.logs.SetBase(base)
	} else {
		drain := func() []slog.Record { return nil }
		if ks.logBuffer != nil {
			drain = ks.logBuffer.Drain
		}
		ks.logs.SetBaseFlushing(base, drain, cfg.Level)
		ks.logBuffer = nil
	}
	ks.logs.SetLevel(cfg.Level)
	if ks.logCloser != nil {
		ks.logCloser.Close()
	}
	ks.logCloser = closer
	ks.logApplied = true
	ks.logKey = key
}

// SetupClientLogging routes a short-lived client process's own logs to a file
// (Name-cli.log alongside the default log dir) at TRACE, so the console stays
// clean while a command runs, with WARN and above additionally echoed to stderr.
// Unlike ConfigureLogging it installs a standalone logger (it does not touch the
// fan-out), because a client process may still spawn/configure the daemon's own
// sink on the fan-out. Returns a Closer for the file, or nil if none could be
// opened (the bootstrap logger then stays in effect).
func (ks *KettlingarService) SetupClientLogging() io.Closer {
	path := ks.clientLogFile()
	if path == "" {
		return nil
	}
	fileH, closer, err := logging.BaseHandler(logging.Config{
		Target: "file", Format: "text", Level: logtee.LevelTrace,
		File: logging.FileConfig{Path: path},
	})
	if err != nil {
		return nil
	}
	stderrWarn := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(logging.Tee(fileH, stderrWarn)).With("service", ks.Name+"-cli")
	slog.SetDefault(logger)
	ks.logMu.Lock()
	ks.Logger = logger
	ks.logMu.Unlock()
	return closer
}

// resolveLogConfig turns the user-facing LogSettings into a concrete
// logging.Config (resolving auto/default sentinels) and a key that summarizes it
// for change detection. Callers hold ks.logMu.
func (ks *KettlingarService) resolveLogConfig(s LogSettings) (logging.Config, string) {
	target := resolveLogTarget(s.Target)
	format := resolveLogFormat(s.Format, target)
	file := s.File
	if file.Path == "" {
		file.Path = ks.defaultLogFile()
	}
	// auto/file with no usable path falls back to stderr rather than failing;
	// re-resolve the format since the destination changed.
	if target == "file" && file.Path == "" {
		target = "stderr"
		format = resolveLogFormat(s.Format, target)
	}
	cfg := logging.Config{
		Target: target, Format: format, Level: s.Level, Source: s.Source,
		File: file, Syslog: s.Syslog,
	}
	key := logConfigKey(cfg)
	return cfg, key
}

func logConfigKey(c logging.Config) string {
	return fmt.Sprintf("%s|%s|%d|%t|%s|%d|%d|%d|%t|%s|%s|%s",
		c.Target, c.Format, int(c.Level), c.Source, c.File.Path, c.File.MaxSizeMB,
		c.File.MaxBackups, c.File.MaxAgeDays, c.File.Compress,
		c.Syslog.Addr, c.Syslog.Facility, c.Syslog.Tag)
}

// stderrIsTTY reports whether stderr is a character device (an interactive
// terminal). It is a var so tests can stub the check without a real tty.
var stderrIsTTY = func() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// resolveLogFormat turns the format setting into a concrete "text" or "json". An
// explicit "text"/"json" always wins. Otherwise ("auto", the default) the format
// follows the destination: file and syslog get json (they feed machine
// consumers), while stderr/stdout get text only when a human is watching an
// interactive terminal, and json when redirected to a file, a pipe, or a service
// manager like journald.
func resolveLogFormat(format, target string) string {
	switch format {
	case "text", "json":
		return format
	}
	switch target {
	case "file", "syslog":
		return "json"
	default: // stderr, stdout
		if stderrIsTTY() {
			return "text"
		}
		return "json"
	}
}

// resolveLogTarget maps the "auto"/"-"/"" sentinels to a concrete target. A
// long-running service is normally a daemon, so auto selects file logging --
// keeping the console that launched it clean -- and the caller downgrades to
// stderr when no file path is available.
func resolveLogTarget(t string) string {
	switch t {
	case "", "-", "auto":
		return "file"
	default:
		return t
	}
}

// defaultLogFile is the service's default log file: Name/Name.log under the user
// cache dir, or "" when that cannot be determined (the caller falls back to
// stderr).
func (ks *KettlingarService) defaultLogFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ks.Name, ks.Name+".log")
}

// clientLogFile is the per-client log file: Name-cli.log in the same directory
// as the default log file, or "" when the cache dir cannot be determined.
func (ks *KettlingarService) clientLogFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, ks.Name, ks.Name+"-cli.log")
}
