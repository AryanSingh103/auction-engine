# 003. HTTP server timeouts and graceful shutdown

## Context
Go's `http.Server` zero value has **no timeouts**, so one slow or idle client can hold a connection forever. Processes will also be stopped constantly: deploys, and the brief's own fault injection.

## Decision
All four timeouts are set, and each is required config:
- `ReadHeaderTimeout` (5s) defeats Slowloris, where a client trickles headers slowly.
- `ReadTimeout` (10s) bounds reading the whole request, body included.
- `WriteTimeout` (10s) bounds the time from the end of the request read to the end of the response write.
- `IdleTimeout` (75s) bounds keep-alive connections. It's kept above the ALB's 60s idle timeout to avoid 502s.

On SIGINT or SIGTERM, `Shutdown` stops accepting connections, closes idle ones, and waits up to `SHUTDOWN_TIMEOUT` for in-flight requests. After that, `Close` force-closes what remains and the process exits 1. Both paths were verified by hand.

## Alternatives considered
- **Zero-value server or `http.ListenAndServe`:** unbounded, and cannot be shut down.
- **Exiting immediately on signal:** in-flight requests would get connection resets.

## Consequences
`Shutdown` does **not** drain hijacked (WebSocket) connections, and `WriteTimeout` would kill long-lived WebSocket streams. Both need rework in M3.
