# Build:  docker build -t arcadia/api-gateway:local .
#
# Two stages. The runtime is `scratch` — a reverse proxy needs a socket, a clock and
# a CA bundle, and nothing else. There is no shell in the final image, which also
# means there is nothing to get a shell with.

FROM golang:1.24-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

# Manifests before source, so a code-only change reuses the module download.
COPY go.mod go.sum ./

# The default module proxy is the public one, which is what CI uses and what the other
# Arcadia services' Dockerfiles rely on.
#
# Locally the public proxy is often unreachable from inside the build VM, so a working
# one can be supplied — as a BuildKit *secret*, not a build argument. A proxy URL
# usually carries credentials, and a build argument would leave them in `docker history`
# for anyone who pulls the image. The secret is not declared `required`, so a plain
# `docker build .` with no secret still works and simply uses the default.
RUN --mount=type=secret,id=goproxy \
    if [ -s /run/secrets/goproxy ]; then export GOPROXY="$(cat /run/secrets/goproxy)"; fi; \
    go mod download

COPY . .

ARG VERSION=dev

# CGO off so the binary is static and can run on scratch. `-s -w` drops the symbol
# table and DWARF, which is about a third of the size and nothing a production
# binary needs.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/api-gateway \
    ./cmd/api-gateway

# The tests run in the build, not just in CI: an image that compiles but whose
# routing table is wrong sends every request to the wrong service, and that is
# cheaper to catch here than in a running stack.
RUN go vet ./... && go test ./...

# ---------------------------------------------------------------------------- #

FROM scratch

# The CA bundle, for the day an upstream is reached over TLS. Nothing in the
# compose network needs it; a managed service behind the gateway would.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/api-gateway /api-gateway

# An unprivileged uid, declared numerically because scratch has no /etc/passwd to
# resolve a name against.
USER 65532:65532

ARG VERSION=dev
ENV SERVICE_VERSION=${VERSION}

EXPOSE 8090

# The binary answers its own health check, because scratch has no curl and no shell.
# `/livez` rather than `/readyz`: liveness asks whether this process is alive, and a
# gateway whose upstreams are down is still working correctly.
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/api-gateway", "-health-check"]

ENTRYPOINT ["/api-gateway"]
