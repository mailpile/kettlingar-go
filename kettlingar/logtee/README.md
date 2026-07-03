# logtee

`logtee` provides an `slog.Handler` that fans log records out to live in-process
subscribers, so an application can watch its own logs at runtime while still
writing them normally. The service layer uses it to drive streaming API updates
(`status --watch`, the `discover` progress generator): work logs through
`slog.Default()` as usual, and a subscriber forwards those records to the API
client as progress.

## Why slog + a fan-out handler

The call sites already log through `*slog.Logger` (`pagekitelib`'s
`Config.Logger` / `slog.Default()`), so introspection needs no changes there: a
custom `slog.Handler` can intercept every record. Two mechanisms combine:

- **Fan-out**: `FanoutHandler` forwards each record both to its base handler
  (normal output) and to any matching subscribers.
- **Context routing**: a subscription can capture *only* one operation's logs.
  The operation runs with a context carrying its `Subscription` as a sink, and
  records emitted under that context go to that subscription alone. This is how a
  single API call streams just its own progress, not all concurrent activity.

## API

- `New(w, level)` / `NewHandler(base, level)` -- build a logger + handler.
- `FanoutHandler.SetLevel` -- adjust global verbosity at runtime.
- `FanoutHandler.SetBase` -- swap where "normal output" is written at runtime
  (e.g. stderr -> a rotating file or syslog), for the process default and every
  derived logger at once, without disturbing live subscribers. The base is owned
  by the shared hub, so a derived handler re-applies its `WithAttrs`/`WithGroup`
  over the current base on each record.
- `FanoutHandler.SetBaseFlushing(base, drain, min)` -- swap the base and replay
  records captured before it was known (e.g. from a bootstrap buffer), writing
  those at or above `min` into the new base first, all under the hub lock so
  nothing is lost, duplicated, or misordered.
- `Subscribe(level)` -- receive every record at or above `level`.
- `SubscribeContext(parent, level)` -- a context-routed subscription plus the
  child context to run the scoped work under.
- `Subscription.C` / `Close()` / `Dropped()`.
- `Format(record)` / `ParseLevel(s)` / `LevelTrace`.

## Gotchas

- Each subscription's channel is bounded (`subBuffer`); sends beyond it are
  dropped rather than blocking the logging path. `Subscription.Dropped()` reports
  the count, so a slow reader degrades gracefully instead of stalling producers.
- `LevelTrace` sits below `slog.LevelDebug` for the noisiest detail.
