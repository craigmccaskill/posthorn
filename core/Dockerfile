# syntax=docker/dockerfile:1.7
#
# Build:  cd core && docker build -t posthorn:local .
# Run:    docker run -p 8080:8080 \
#           -v $PWD/posthorn.toml:/etc/posthorn/config.toml:ro \
#           -e POSTMARK_API_KEY=$POSTMARK_API_KEY \
#           posthorn:local

# --- Stage 1: build the static binary ----------------------------------
FROM golang:1.25-alpine AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# GOWORK=off makes the build ignore the repo-root go.work file. The build
# context here is just core/, which is a self-contained module — no need
# (or means) to resolve the workspace.
ENV CGO_ENABLED=0 \
    GOWORK=off \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

WORKDIR /src

# Cache module downloads in a separate layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/posthorn \
        ./cmd/posthorn

# --- Stage 2: minimal runtime ------------------------------------------
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.title="Posthorn" \
      org.opencontainers.image.description="The unified outbound mail layer for self-hosted projects" \
      org.opencontainers.image.source="https://github.com/craigmccaskill/posthorn" \
      org.opencontainers.image.url="https://posthorn.dev" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/posthorn /usr/local/bin/posthorn

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/posthorn", "serve", "--config", "/etc/posthorn/config.toml"]
