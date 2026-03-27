FROM node:20-alpine AS frontend
WORKDIR /app
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web/ ./web/
COPY internal/api/dist/ ./internal/api/dist/
RUN cd web && npm run build

FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/internal/api/dist/ ./internal/api/dist/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o sage-sidecar ./cmd/pg_sage_sidecar/

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -g 1000 sage \
    && adduser -u 1000 -G sage -s /sbin/nologin -D sage
COPY --from=builder /build/sage-sidecar /usr/local/bin/sage-sidecar
USER sage
EXPOSE 5433 8080 9187
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -sf http://localhost:8080/api/v1/metrics || exit 1
ENTRYPOINT ["sage-sidecar"]
