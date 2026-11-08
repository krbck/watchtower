# Multi-arch (x86_64 + arm64) — homelab raspberry pi support.
# Go builds statically, so distroless works on every architecture.
# Buildx: docker buildx build --platform linux/amd64,linux/arm64 --push .
# Targets: manager (default), ui — `docker build --target ui`.
FROM golang:1.23-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /manager ./cmd/manager
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /ui ./cmd/ui

FROM gcr.io/distroless/static:nonroot AS manager
COPY --from=builder /manager /
ENTRYPOINT ["/manager"]

FROM gcr.io/distroless/static:nonroot AS ui
COPY --from=builder /ui /
ENTRYPOINT ["/ui"]
