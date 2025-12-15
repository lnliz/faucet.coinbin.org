FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev sqlite-dev make

WORKDIR /app

COPY go.mod go.sum ./

COPY . .

ARG COMMIT_HASH
RUN CGO_ENABLED=1 go build -ldflags="-X github.com/lnliz/faucet.coinbin.org/service.CommitHash=${COMMIT_HASH}" -o app .

FROM alpine:3.23

RUN apk add --no-cache sqlite-libs

WORKDIR /app

COPY --from=builder /app/app       .
COPY --from=builder /app/templates ./templates

ENTRYPOINT ["/app/app"]
