# ── Build stage: compile static Go binary ──────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache deps — copy go.mod/go.sum first, download, then copy source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static build (CGO_ENABLED=0) for scratch/distroless target
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /aware-gateway ./cmd/gateway/

# ── Runtime stage: minimal image ──────────────────────
FROM alpine:3.21

# ca-certificates for HTTPS upstreams (OpenAI, OpenRouter, etc.)
# tzdata for correct timestamp handling
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /aware-gateway /usr/local/bin/aware-gateway
COPY configs/gateway-openrouter.yaml /etc/aware-gateway/gateway.yaml

ENV GW_CONFIG=/etc/aware-gateway/gateway.yaml

EXPOSE 12026

ENTRYPOINT ["aware-gateway"]
CMD ["-config", "/etc/aware-gateway/gateway.yaml"]
