FROM docker.io/alpine:3 AS certs
RUN apk add --no-cache ca-certificates && mkdir -p /out/etc/deploy-bot

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certs /out/etc /etc
COPY bin/bot /bot
COPY bin/receiver /receiver
