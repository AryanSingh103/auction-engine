# 003. HTTP server timeouts and graceful shutdown

## Context
The zero-value `http.Server` has no timeouts, so one slow or idle client can hold a connection forever. Processes will also be stopped constantly: deploys and fault injection.

## Decision
All four timeouts are required config:
- `ReadHeaderTimeout` (5s): defeats Slowloris.
- `ReadTimeout` (10s): bounds reading headers plus body.
- `WriteTimeout` (10s): starts when the **headers** are read, so reading the body eats into it (net/http `server.go`). When it expires, the connection dies silently: the handler keeps running and the log still says 200. Handler deadlines come in M1 (R11).
- `IdleTimeout` (75s): bounds keep-alive connections. It sits above the ALB's 60s idle timeout to avoid 502s.

On SIGINT or SIGTERM, `Shutdown` stops accepting connections and waits for in-flight requests. `SHUTDOWN_TIMEOUT` bounds the whole shutdown, metrics and live handlers included. After that, we cancel every request context (`BaseContext`), `Close` the remaining connections, and exit 1.

## Alternatives considered
- **Zero-value server:** unbounded, and cannot be shut down.
- **Exiting immediately on signal:** in-flight requests get connection resets.

## Consequences
`Shutdown` does not drain hijacked (WebSocket) connections, and `WriteTimeout` kills WebSocket streams. Both are M3 work (R12).
