# syntax=docker/dockerfile:1

# Build stage: compiles a static Go binary and caches dependencies separately.
FROM golang:1.25.7-alpine AS builder

WORKDIR /src

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go mod tidy \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/evaluation-service .

# Runtime stage: lightweight base with certs and wget for container healthchecks.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates wget \
    && addgroup -S app \
    && adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/evaluation-service /app/evaluation-service

USER app

ENV PORT=8084
EXPOSE 8084

CMD ["/app/evaluation-service"]
