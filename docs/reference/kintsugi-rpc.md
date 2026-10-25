# kintsugi-rpc

JSON-RPC loopback API.

## Synopsis

```bash
kintsugi rpc [--mode stdio|tcp|http] [--addr host:port] [--auth secret]
```

## Description

Serve the harness over JSON-RPC 2.0 so an external client — an editor, a web UI,
another agent — can drive it.

The server binds loopback only. `stdio` speaks newline-delimited JSON on standard
input and output. `tcp` speaks one JSON-RPC message per line, with an optional
shared secret sent as a leading `AUTH <secret>` line. `http` accepts a POST per
request, with the secret sent as `Authorization: Bearer <secret>`.

A non-loopback address is refused rather than bound: the API carries the
operator's tool authority, so it is never reachable from the network.

## Methods

- **initialize** - Report the server's protocol version and capabilities
- **session/new** - Open a session; returns its id
- **session/prompt** - Run one turn in a session
- **session/cancel** - Abandon the turn in flight
- **session/list** - List the sessions this server knows

## Options

- **--mode** - Transport: stdio (default), tcp, or http
- **--addr** - Listen address for tcp/http. Must be loopback. Default `127.0.0.1:9876`
- **--auth** - Shared secret required by tcp/http clients

## Examples

Speak JSON-RPC on stdio:

```bash
$ kintsugi rpc
```

Serve on loopback with a secret:

```bash
$ kintsugi rpc --mode tcp --addr 127.0.0.1:9876 --auth "$KINTSUGI_RPC_SECRET"
```

## See Also

- [kintsugi](kintsugi.md)
- [kintsugi-run](kintsugi-run.md)
