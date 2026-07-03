# kettlingar-go

This is a re-implementation of the
[Python Kettlingar RPC microframework](https://github.com/mailpile/kettlingar/),
in Go.

Kettingar is a micro-framework for building Go microservices that expose an HTTP/1.1 interface.
The motivation was to solve the folowing two use cases:

- Control, inspect and manage simple Go background services
- Gather all the "must have" common functionality for running a service, into one place

Some features:

- Expose HTTP RPC servers from Golang functions
- Msgpack by default, JSON supported for RPC interactions
- Other formats can be rendered in response to the Accept: header
- Incremental (HTTP chunked or SSE) responses using Go channels
- Built in client for calling a running service from other go code
- Built in CLI for starting, stopping and interacting service
- Extendable OpenMetrics, errors and latency measured by default


# Status / TODO:

Status: Experimental, almost useful.

TODO:

- Document how configuration works (viper and cobra)
- Think about error handling a bit more, especially in generators
- Serve normal web requests as well, serve as HTMX backend?
- Websocket support?

Maybe?

- Unix domain socket support
- Passing file descriptors to/from the service
- Python kettlingar RPC compatibility


# Installation

...


# Usage

See [main.go](main.go) for a working example.

... demo the CLI


# Writing API endpoints

- Overall service struct
- Argument struct (defaults annotations)
- Response struct
- Function - generator or no?
- Implementing the Render API for the Response struct

...


# Acces controls

A `kettlingar` microservice offers two levels of access control:

- "Public", or unauthenticated methods
- Private methods

Access to private methods is granted by checking for a special token's presence in the HTTP Authorization header,
or in the URL path.

In the case where the client is running on the same machine,
and is running using the same user-id as the microservice,
credentials are automatically found at a well defined location in the user's home directory.
Access to them is restricted using Unix file system permissions.


# Logging

kettlingar owns the logging machinery so a service does not have to reimplement
it. Two subpackages provide the mechanism, and `KettlingarService` provides a
small high-level API on top:

- `kettlingar/logging` -- the destination handlers built from configuration: a
  rotating file writer (no third-party dependency), syslog, stderr/stdout, a
  discard sink, the bootstrap `BufferHandler` (captures records before the sink
  is known) and `Tee`. Secret-valued attributes are redacted via a denylist that
  applications extend with `logging.RegisterSecretKey`.
- `kettlingar/logtee` -- the `FanoutHandler`: one base sink plus any number of
  by-level `Subscribe`rs, so a service can both write to a file and stream the
  same records to a log-streaming API method. Also defines the sub-debug `TRACE`
  level and `ParseLevel`/`LevelString`/`Format`.

The service-level API (see `logging.go`):

- `MakeService` calls `InstallLogging`, which installs the fan-out as the default
  `slog` logger with a bootstrap base (in-memory buffer + stderr gated at WARN),
  so the service logs immediately, tagged with its name, before any destination
  is configured.
- `ConfigureLogging(LogSettings)` resolves the settings (auto target/format, a
  default `Name/Name.log` file under the user cache dir) and installs the sink as
  the fan-out base, flushing the buffer on first call and change-detecting later
  calls so an unrelated reconfigure does not reopen the file.
- `SetupClientLogging()` routes a short-lived client process's own logs to a
  `Name-cli.log` file at TRACE (WARN+ echoed to stderr); `DefaultMain` calls it
  for client commands and leaves the foreground daemon's logging alone.
- `LogFanout()` exposes the fan-out for a service's log-streaming API methods.

Beyond that, kettlingar does not decide where logs go: it emits through
`log/slog` and leaves the routing policy to the host. It never writes diagnostics
to stderr directly. The service logger is `KettlingarService.Logger`; a caller
may override it.

Log messages are prefixed with the configured service name (`KettlingarService.Name`,
or the `name` passed to `MakeClient`), e.g. `myservice: listening` -- the library
does not put its own name in the messages. Levels follow the usual policy --
verbose per-operation detail at TRACE/DEBUG, handled problems at WARN, failures
that stop something at ERROR:

- **Client** traces at a sub-debug TRACE level (`slog.LevelDebug - 4`), from both
  RPC client paths: the embeddable `MakeClient` (how the client was wired -- one
  line per method with its endpoint -- and every call it makes: request method/
  endpoint/streaming/size, response status, failures, and a streaming call's frame
  count when its stream closes) and the `DefaultMain` CLI's own `api <cmd>` caller
  (per-call request/response, keyed by method; the URL is deliberately omitted
  because it embeds the auth secret). So, from the client side, which connections
  were made and which API calls were sent.
- **Server** traces each received RPC at TRACE (method, remote, authed, generator)
  and logs its completion at DEBUG (method, code, elapsed_us); a handler panic and
  encode/convert failures are ERROR, a malformed request body is DEBUG. The
  request is never dumped wholesale (it carries the auth secret); only safe fields
  are logged.
- **Lifecycle** (the `DefaultMain` CLI driver) logs setup/startup/listen/daemonize
  failures and the async HTTP-server and shutdown-hook errors through the logger.
  Key lifecycle transitions (listening, started in background, stopping, shutting
  down) are logged at INFO *in addition to* the human-facing stdout banner -- the
  banner is for the interactive user, the log line is the durable record. Only
  genuine program output (RPC results, those status banners) goes to stdout; fatal
  failures rely on the host's level policy (e.g. WARN+ mirrored to the console) for
  visibility rather than writing stderr themselves.


# Kettlingar? Huh?

Kettlingar means "kittens" in Icelandic.
This is a spin-off project from
[moggie](https://github.com/mailpile/moggie/) (a moggie is a cat)
and the author is Icelandic.


# License and Credits

[MIT](https://choosealicense.com/licenses/mit/), have fun!

Created by [Bjarni R. Einarsson](https://github.com/BjarniRunar).
