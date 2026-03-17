# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.21-alpine AS builder

WORKDIR /src
COPY go.mod .
COPY main.go .

RUN go build -ldflags="-s -w" -o /sfh .

# ── cloudflared download stage ────────────────────────────────────────────────
# Fetch the correct architecture binary directly from the official release.
FROM alpine:3.20 AS cf-downloader

RUN apk add --no-cache curl

# TARGETARCH is injected by BuildKit: "amd64" or "arm64"
ARG TARGETARCH

RUN set -eux; \
    BASE="https://github.com/cloudflare/cloudflared/releases/latest/download"; \
    case "${TARGETARCH}" in \
      amd64) FILE="cloudflared-linux-amd64"  ;; \
      arm64) FILE="cloudflared-linux-arm64"  ;; \
      *)     echo "Unsupported arch: ${TARGETARCH}" && exit 1 ;; \
    esac; \
    curl -fsSL "${BASE}/${FILE}" -o /cloudflared; \
    chmod +x /cloudflared

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates: needed by both sfh (HTTPS) and cloudflared (TLS to Cloudflare)
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder     /sfh          /usr/local/bin/sfh
COPY --from=cf-downloader /cloudflared /usr/local/bin/cloudflared

# Default data directory inside the container
RUN mkdir -p /data
VOLUME /data

EXPOSE 4040

# ── Environment variables ─────────────────────────────────────────────────────
# AUTH   → --auth user:pass  (required to enable write/manage endpoints)
# PORT   → --port            (default 4040)
# DIR    → --dir             (default /data)
# CF         → set to "true" to enable the Cloudflare Tunnel
# TOKEN   → Cloudflare Tunnel token (required when CF=true)
ENV PORT=4040 \
    DIR=/data  \
    AUTH=""    \
    CF="false"     \
    TOKEN=""

# ── Entrypoint script ─────────────────────────────────────────────────────────
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
