FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/bot     ./cmd/bot && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/receiver ./cmd/receiver

# Seed the config directory so the mount point exists in the image.
RUN mkdir -p /out/etc/deploy-bot

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/etc /etc
COPY --from=builder /out/bot     /bot
COPY --from=builder /out/receiver /receiver
