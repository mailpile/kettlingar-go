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


# Kettlingar? Huh?

Kettlingar means "kittens" in Icelandic.
This is a spin-off project from
[moggie](https://github.com/mailpile/moggie/) (a moggie is a cat)
and the author is Icelandic.


# License and Credits

[MIT](https://choosealicense.com/licenses/mit/), have fun!

Created by [Bjarni R. Einarsson](https://github.com/BjarniRunar).
