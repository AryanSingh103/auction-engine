# 001. Config from env vars: hand-rolled, fail-fast

## Context
The brief requires all config to come from env vars, with no hardcoded hosts. The same binary runs on a laptop, in compose, in CI and on ECS, and all of them inject env vars.

## Decision
`internal/config` is stdlib only:
- every variable is required, with no defaults
- empty counts as missing
- values are parsed into typed fields at startup
- all errors are reported together via `errors.Join`

`Load` takes an `os.LookupEnv`-shaped function, so tests pass a map instead of mutating the process env.

## Alternatives considered
- **viper:** layered file, flag and remote sources with implicit precedence. We want exactly one source.
- **caarlos0/env:** less code, but reflection and struct tags. For a few variables, explicit code is easier to debug and explain.
- **Defaults in code:** then a missing variable in production silently becomes a laptop value, which is exactly the failure the brief forbids.

## Consequences
Misconfiguration fails at boot with every problem listed, never at first use. Each new variable costs a few explicit lines, and `.env.example` must be kept in sync by hand.
