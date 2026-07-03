# logging

`logging` builds the base `slog.Handler` a kettlingar service writes its logs to,
from configuration. It is the destination half of the logging stack; `logtee` is
the fan-out half. The service installs the handler built here as the base of a
`logtee.FanoutHandler`, so every record goes both to this configured sink (file
/ syslog / stderr) and to any live API subscribers (`status --watch`, discover
progress) -- independent branches, never either/or.

## API

- `BaseHandler(cfg Config) (slog.Handler, io.Closer, error)` -- build the
  handler for a resolved `Config`. The `Closer` releases the sink (file handle or
  syslog connection) when the handler is swapped out; it is a no-op for stderr
  and none.
- `Config{ Target, Format string; Level slog.Level; File FileConfig; Syslog SyslogConfig }`.
  - `Target`: `stderr` (default), `stdout`, `file`, `syslog`, or `none`.
  - `Format`: `text` (default) or `json`.
  - `none` yields a discard handler: nothing is written, but fan-out subscribers
    still receive records, so API log streaming works with logging off.
- `NewBufferHandler(capacity)` -> `*BufferHandler` -- the bootstrap in-memory
  sink used before logging is configured. It captures records (cloned, since a
  record's attrs are only valid during `Handle`) into a bounded ring, dropping
  the oldest past `capacity` (counted by `Dropped`). `Drain()` returns the
  captured records in order and closes the buffer; pass it to
  `logtee.FanoutHandler.SetBaseFlushing` to replay them into the real sink once
  it is known.
- `Tee(hs ...slog.Handler)` -- forward each record to every handler whose own
  `Enabled` admits it. Used for the bootstrap base: a `BufferHandler` capturing
  everything plus a stderr handler gated at `WARN`, so urgent startup problems
  are visible on the console before logging is configured.

## File rotation

The `file` target writes through an internal rotating writer (no third-party
dependency). It rotates when a write would exceed `MaxSizeMB`: the current file
becomes `name.1` (gzipped to `name.1.gz` when `Compress` is set), older backups
shift up to `MaxBackups`, and backups beyond that count or older than
`MaxAgeDays` are removed. All access is serialized by a mutex; `MaxSizeMB == 0`
disables size rotation.

## Syslog

The `syslog` target (`syslog_unix.go`, built on non-Windows/plan9) connects to
the local syslog daemon, or a remote server over UDP when `Syslog.Addr` is set,
using `Syslog.Facility` (default `daemon`) and `Syslog.Tag` (default
`pagekite-go`). Each record is formatted with the configured text/json body and
sent at the syslog severity mapped from its slog level. On platforms without
`log/syslog` (`syslog_other.go`) `BaseHandler` returns an error so the caller can
fall back to a file/stderr target.

## Log schema and conventions

JSON output is an API for log processors, so the shape is pinned and kept
consistent. Every handler (text and json) runs a `ReplaceAttr` (`replaceAttr`)
that:

- labels the sub-debug trace level as `TRACE` (slog would otherwise print
  `DEBUG-4`), via `logtee.LevelString`;
- **redacts secrets** as a safety net: any attribute whose key is on the
  `secretKeys` denylist (`secret`, `password`, `authorization`,
  `dns_update_secret`) has its value replaced with `[redacted]`. `Redact(key)`
  exposes the same check so the API log-streaming path redacts identically.

Each json line carries `time` (RFC3339Nano), `level`, `msg`, the record's attrs,
and a `service` name attached once at bootstrap via `logger.With`. `Config.Source`
(the `log_source` setting) adds a `source` file:line attribute for debugging.

Attribute keys are snake_case to match the config/JSON schema, so logs stay
queryable. Emerging conventions: `remote` (peer addr), `relay` (relay id/addr),
`kite` (proto://name), `err` (error string), `proto`, `port`, `count`/`n`. New
call sites should follow them.

## Files

- `logging.go` -- `Config`, `BaseHandler`, `replaceAttr`/`Redact`, the
  discard/none handler.
- `buffer.go`  -- `BufferHandler` (bootstrap capture) and `Tee`.
- `rotate.go`  -- the size/age/count rotating file writer.
- `syslog_unix.go` / `syslog_other.go` -- the syslog handler and its stub.
