FROM golang:1.26-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev make ca-certificates

WORKDIR /app

COPY go.mod go.sum ./

COPY . .

ARG COMMIT_HASH
RUN CGO_ENABLED=1 go build -ldflags="-X github.com/lnliz/faucet.coinbin.org/service.CommitHash=${COMMIT_HASH} -linkmode external -extldflags '-static'" -o app .

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/app       /app/app
COPY --from=builder /app/templates /app/templates
COPY --from=builder /app/static   /app/static

WORKDIR /app

ENTRYPOINT ["/app/app"]
