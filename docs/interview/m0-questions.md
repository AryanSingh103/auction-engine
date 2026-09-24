# Milestone 0 interview questions

Generated from the M0 diff by the interview-questions subagent. Answer each one in your own words (below each question, or in chat), then have Claude grade the answers and point out where they are wrong or vague.

## 1. Drain timeline
*Files: `cmd/api/main.go` (shutdown sequence), `docs/decisions/003-http-server-timeouts-and-shutdown.md`*

SIGTERM arrives 8s into a 20s handler, with `SHUTDOWN_TIMEOUT=15s` and `HTTP_WRITE_TIMEOUT=10s`.
- Walk through the timeline: what happens to the listener, to idle keep-alive connections, and to this request's connection, and when does each timeout take effect?
- After `srv.Shutdown` times out and `srv.Close()` returns, is the handler goroutine stopped? What does it notice (`r.Context()`, a failed `Write`)?
- In M1 this handler could be halfway through writing a bid to Postgres. What guarantee do you have about that bid, and what would you change?

## 2. WriteTimeout and the ALB
*Files: ADR 003, `.env.example`, `internal/httpapi/middleware.go`*

- A handler takes 12s with `WriteTimeout=10s`, then writes a 200 with a body. What does the client see? Is the request context cancelled at 10s? What status and byte count does `requestLogger` record, and does that match what happened on the wire?
- Explain the race behind "IdleTimeout (75s) must be above the ALB's 60s idle timeout". Who closes the connection first, what is in flight at that moment, and why do 502s happen only when the order is the other way round?

## 3. Signals and PID 1
*Files: `Dockerfile`, `cmd/api/main.go`, `compose.yaml` (`stop_grace_period`)*

- With exec-form `ENTRYPOINT ["/api"]`, the binary is PID 1. How does the Linux kernel treat signals to PID 1 differently from signals to other processes?
- If you deleted the `signal.NotifyContext` line, what would `docker stop` do, how long would it take, and what exit status would you see?
- During the drain, `stop()` is called "so a second Ctrl-C kills immediately". Does that still hold when the process is PID 1 in this container? Why or why not?

## 4. Config design and testing `run()`
*Files: `internal/config/config.go`, `config_test.go`, `cmd/api/main.go`, `compose.yaml`*

- `config.Load` takes a `LookupFunc` instead of reading `os.Getenv`. What concrete testing problem does that solve compared with `os.Setenv` or `t.Setenv`?
- Several constraints are not enforced anywhere: IdleTimeout above the ALB's 60s, ReadHeaderTimeout ≤ ReadTimeout, and SHUTDOWN_TIMEOUT below the compose `stop_grace_period`. Which belong in `Load`, which belong elsewhere, and why?
- `run()` has no tests. How would you restructure it so the "drain times out, force Close, return error" path can be tested deterministically, without real signals or sleeps?

## 5. Middleware order
*Files: `internal/httpapi/router.go`, `internal/httpapi/middleware.go`*

The chain is RequestID, then requestLogger, then recoverer.
- (a) A handler writes a 200 header and half its body, then panics. What does the client receive? What does `recoverer`'s `w.WriteHeader(500)` actually do? What status ends up in the request log, and is it true?
- (b) A handler panics with `http.ErrAbortHandler`. Is a request log line written at all? Why?
- (c) What changes in each case if you swap `r.Use(requestLogger)` and `r.Use(recoverer)`?
