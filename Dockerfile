# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

WORKDIR /src
COPY go.mod .
COPY main.go .

RUN go build -ldflags="-s -w" -o /sfh .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates is needed if you ever proxy HTTPS; tzdata for correct times
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /sfh /usr/local/bin/sfh

# Default data directory inside the container
RUN mkdir -p /data
VOLUME /data

EXPOSE 4040

# SFH_AUTH  → --auth user:pass  (required to enable write/manage endpoints)
# SFH_PORT  → --port            (default 4040)
# SFH_DIR   → --dir             (default /data)
ENV PORT=4040 \
    DIR=/data \
    AUTH=""

CMD sh -c 'exec sfh \
  --port "${PORT}" \
  --dir  "${DIR}"  \
  $([ -n "${AUTH}" ] && echo "--auth ${AUTH}")'
