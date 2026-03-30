FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /deploy-bot ./cmd/bot

FROM gcr.io/distroless/static-debian12
COPY --from=builder /deploy-bot /deploy-bot
ENTRYPOINT ["/deploy-bot"]
