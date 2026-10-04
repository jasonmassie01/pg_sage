FROM node:20-alpine AS frontend
WORKDIR /app
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web/ ./web/
COPY internal/api/dist/ ./internal/api/dist/
RUN cd web && npm run build

FROM golang:1.25-alpine AS builder
# build-base: cgo compiles libpg_query for parse-tree SQL validation.
RUN apk add --no-cache git build-base
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/internal/api/dist/ ./internal/api/dist/
# Build metadata surfaces in --version and pg_sage_info (G10-B03).
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=1 go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o sage-sidecar ./cmd/pg_sage_sidecar/ \
    && ./sage-sidecar --version | grep -q "sql-ast: libpg_query"

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -g 1000 sage \
    && adduser -u 1000 -G sage -s /sbin/nologin -D sage
COPY --from=builder /build/sage-sidecar /usr/local/bin/sage-sidecar
# Roadmap 1.1: this commit's signed PGIncidentBench reports (CI puts them
# in release-bench/; a local build ships only the README). The sidecar
# verifies and ingests them at startup.
COPY release-bench/ /usr/share/pg_sage/bench/
USER sage
EXPOSE 8080 9187
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -sf http://localhost:8080/health || exit 1
ENTRYPOINT ["sage-sidecar"]
