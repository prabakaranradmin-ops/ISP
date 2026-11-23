# AAA core daemon — RADIUS server, FUP scanner, Asynq workers, dead-letter monitor.
# Referenced by docker-compose.yml service aaa_core_daemon.

# ── Build ────────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies are copied first so the module download layer is reused whenever
# only application source has changed.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is off so the result is a static binary that runs on a scratch-like base.
# -trimpath keeps build-host paths out of the binary and makes builds reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/radiusd \
    ./cmd/radiusd

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.19

# ca-certificates is needed for outbound TLS to the WhatsApp and SMS APIs;
# tzdata so IST-scheduled jobs resolve Asia/Kolkata rather than falling back to UTC.
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 -h /app bss

WORKDIR /app
COPY --from=build /out/radiusd /app/radiusd

# Runs unprivileged: RADIUS uses 1812/1813, which are above 1024 and need no
# elevated capability to bind.
USER bss

EXPOSE 1812/udp 1813/udp 9100

ENTRYPOINT ["/app/radiusd"]
