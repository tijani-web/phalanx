# ─────────────────────────────────────────────────────────────────────
# Phalanx — Multi-Stage "Machine Vibe" Dockerfile
#
# Stage 1: Compile the Go binaries with stripped symbols.
# Stage 2: Minimal Alpine runtime with BadgerDB volume mount point.
#
# Build: docker build -t phalanx .
# Run:   docker run -v phalanx_data:/data -p 9000:9000 -p 8080:8080 phalanx
# ─────────────────────────────────────────────────────────────────────

# ───── Stage 1: Builder ─────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build server binary — stripped and statically linked.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /bin/phalanx-server ./cmd/server

# Build CLI binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /bin/phalanx ./cmd/phalanx

# ───── Stage 2: Runtime ─────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates bash

# BadgerDB data directory — mount a volume here.
RUN mkdir -p /data
VOLUME ["/data"]

# Copy binaries from builder.
COPY --from=builder /bin/phalanx-server /usr/local/bin/phalanx-server
COPY --from=builder /bin/phalanx /usr/local/bin/phalanx

# Copy entrypoint script.
COPY start.sh /usr/local/bin/start.sh
RUN chmod +x /usr/local/bin/start.sh

# Expose gRPC (9000) and debug HTTP (8080).
EXPOSE 9000 8080

# Default entrypoint uses start.sh for Fly.io auto-configuration.
# Override with CMD for manual configuration.
ENTRYPOINT ["/usr/local/bin/start.sh"]
