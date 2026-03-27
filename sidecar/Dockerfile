FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o sage-sidecar ./cmd/pg_sage_sidecar/

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -g 1000 sage \
    && adduser -u 1000 -G sage -s /sbin/nologin -D sage
COPY --from=builder /build/sage-sidecar /usr/local/bin/sage-sidecar
USER sage
EXPOSE 5433 8080 9187
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -sf http://localhost:5433/health || exit 1
ENTRYPOINT ["sage-sidecar"]
