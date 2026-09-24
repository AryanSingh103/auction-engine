# 001. Configuration from environment variables, hand-rolled, fail-fast

## Context
The brief requires all configuration to come from environment variables from the first commit, with no hardcoded hosts. The same binary will run on a laptop, in docker compose, in CI and on ECS, and each of these injects configuration as env vars.

## Decision
A small stdlib-only loader (`internal/config`):
- Every variable is required. There are no defaults in code.
- Empty counts as missing.
- Values are parsed into typed fields (`slog.Level`, `time.Duration`) at startup.
- Every problem is collected with `errors.Join` and reported at once.
- `Load` takes a lookup function (the signature of `os.LookupEnv`), so tests pass a map instead of mutating the process environment. That also keeps tests parallel-safe.

## Alternatives considered
- **viper:** built for layered file, flag and remote config with implicit precedence rules. We want exactly one source, so its features would only add ways to be surprised.
- **caarlos0/env or envconfig:** less code, but it relies on reflection and struct tags. For a handful of variables, explicit code is easier to read, debug and explain.
- **Defaults in code:** convenient, but a missing variable in production then silently becomes a laptop value. That's the "localhost buried in the code" failure the brief forbids.

## Consequences
Misconfiguration fails at boot with a complete list of problems, never at first use. Adding a variable costs a few explicit lines, and `.env.example` must be kept in sync by hand.
