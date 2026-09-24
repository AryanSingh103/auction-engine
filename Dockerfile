# syntax=docker/dockerfile:1

# ---- build stage ----------------------------------------------------------
# Runs on the build machine's native platform ($BUILDPLATFORM) and
# cross-compiles for the target, so building an amd64 image for AWS on an
# arm64 Mac needs no emulation. Pure-Go cross-compilation is trivial with
# CGO disabled.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24 AS build
WORKDIR /src

# Dependencies first: this layer is only rebuilt when go.mod or go.sum
# change, not on every source edit. The cache mount keeps the module cache
# across builds even when the layer is rebuilt.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG TARGETOS TARGETARCH
# CGO_ENABLED=0: fully static binary, no libc needed at runtime.
# -trimpath: strip local filesystem paths from the binary (reproducibility).
# -ldflags="-s -w": drop the symbol table and DWARF debug info (smaller).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate ./cmd/loadgen

# ---- runtime stage --------------------------------------------------------
# distroless/static: CA certificates, tzdata and a nonroot user, but no shell
# or package manager. See docs/decisions/004.
FROM gcr.io/distroless/static-debian13:nonroot
# One image, three entrypoints: the API (default), the one-shot migrate
# command and the load generator (compose overrides the entrypoint).
COPY --from=build /out/api /api
COPY --from=build /out/migrate /migrate
COPY --from=build /out/loadgen /loadgen
USER nonroot:nonroot
EXPOSE 8080
# Exec form: the binary is PID 1 and receives SIGTERM from `docker stop`
# directly. Shell form would wrap it in /bin/sh, which does not forward
# signals (and does not exist in this image anyway).
ENTRYPOINT ["/api"]
