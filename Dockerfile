# syntax=docker/dockerfile:1

# One image for both roles: "serve" and "worker" are subcommands of the same
# binary, so the command decides the role, for example
#   docker run durable-mcp worker
#   docker run durable-mcp serve -transport http ...
# See "Run in a container" in the README.

# The Go release line go.mod requires; CI checks that the two agree. The tag
# tracks the line's latest patch release, so standard library fixes arrive
# with a rebuild.
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
# Use the toolchain in the image rather than downloading the one go.mod
# names.
ENV GOTOOLCHAIN=local CGO_ENABLED=0

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG REVISION=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${REVISION}" \
      -o /out/durable-mcp ./cmd/durable-mcp

# Static and distroless: no shell or package manager, but CA certificates
# (for the GitHub API) and time zone data.
FROM gcr.io/distroless/static-debian13:nonroot

ARG VERSION=dev
ARG REVISION=
LABEL org.opencontainers.image.title="durable-mcp" \
      org.opencontainers.image.description="MCP server that runs long agent jobs as durable Temporal workflows" \
      org.opencontainers.image.source="https://github.com/nicogranuja/durable-mcp" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/durable-mcp /usr/local/bin/durable-mcp

# distroless's nonroot user, by number, so Kubernetes' runAsNonRoot can
# check it without reading /etc/passwd.
USER 65532:65532

# Ports the README's examples use: the MCP HTTP transport and -metrics-addr.
# Neither listens by default, and neither listens beyond loopback unless told
# to; the HTTP transport refuses to without auth tokens.
EXPOSE 8080 9464

# SIGTERM starts a graceful stop. The worker takes up to a minute, so stop
# the container with a grace period of at least 70 seconds
# (docker stop -t 70, or terminationGracePeriodSeconds: 70).
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/durable-mcp"]
