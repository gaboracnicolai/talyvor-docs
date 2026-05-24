# syntax=docker/dockerfile:1.7
# Stage 1 — build the Go binary on Alpine. We use the same major-
# minor as go.mod so the produced binary matches local dev.
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads in a layer of their own so dependency
# changes are the only thing that invalidates the bigger source
# layer below.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath strips $PWD from binary paths; -ldflags shrinks the
# binary by dropping symbol + DWARF tables. Both are cheap and
# improve the production posture.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-w -s" \
    -o /out/docs ./cmd/docs

# Stage 2 — minimal runtime. alpine:3.19 + wget gets us a working
# docker healthcheck without dragging in curl or busybox extras.
FROM alpine:3.19

RUN apk add --no-cache wget tzdata ca-certificates && \
    adduser -D -u 10001 docs

WORKDIR /app
COPY --from=builder /out/docs /usr/local/bin/docs
COPY --from=builder /src/migrations /app/migrations

USER docs
EXPOSE 4000

# Docker healthcheck — wget over loopback is enough for liveness.
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD wget -qO- http://localhost:4000/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/docs"]
