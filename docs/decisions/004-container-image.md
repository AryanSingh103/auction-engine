# 004. Container image: multi-stage build onto distroless static

## Context
The API ships as a container locally (compose) and on ECS. The image should be small, run as non-root, stop cleanly, and build for amd64 (EC2) from an arm64 Mac.

## Decision
- **Build stage:** `golang:1.27.1-alpine3.24` on `$BUILDPLATFORM`, cross-compiling to the target with `CGO_ENABLED=0`. Module and build caches use BuildKit cache mounts.
- **Runtime:** `gcr.io/distroless/static-debian13:nonroot`, exec-form `ENTRYPOINT`, so the binary is PID 1 and receives SIGTERM.

Measured: 16.8 MB on disk (3.86 MB compressed). `docker stop` exits 0 with the shutdown logs, and the amd64 cross-build works.

## Alternatives considered
- **alpine runtime:** a shell makes debugging easy, but it's attack surface we don't need. When we do need a shell, distroless has `:debug` variants.
- **scratch:** smaller still, but no CA certificates, tzdata or nonroot user, and we'd need CA certificates for TLS to AWS.
- **debian12 distroless:** deprecated upstream.

## Consequences
There's no shell and no curl in the image, so there's no `docker exec` debugging and no curl-based compose healthcheck. If we need a container healthcheck later, the binary must implement it itself.
