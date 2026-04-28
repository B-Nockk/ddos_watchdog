# ── Build stage ───────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Layer cache: download deps before copying source
COPY go.mod go.sum ./
RUN go mod download

# Copy all Go source files
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o ddos-daemon .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.19

# ipset and iptables are required at runtime for blocker.go
RUN apk add --no-cache ipset iptables ip6tables bash

WORKDIR /app

COPY --from=builder /src/ddos-daemon .

# Audit log directory — also created at runtime by notifier, but good to have
RUN mkdir -p /var/log/ddos-daemon

EXPOSE 8080

ENTRYPOINT ["/app/ddos-daemon"]
