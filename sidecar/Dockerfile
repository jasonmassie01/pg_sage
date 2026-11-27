FROM node:22-alpine AS frontend
WORKDIR /app
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci
COPY web/ ./web/
COPY internal/api/dist/ ./internal/api/dist/
RUN cd web && npm run build

# The Go release is the toolchain line in go.mod (TestToolchainPinsAgree).
FROM golang:1.27.4-alpine3.24 AS builder
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

# Alpine 3.24 pinned by digest; Dependabot (docker) proposes digest bumps.
FROM alpine:3.24@sha256:3f4d6b32e26703c6082839234f43769d5046b3ac7c910033911d1e71d8cd630a
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 1000 sage \
    && adduser -u 1000 -G sage -s /sbin/nologin -D sage \
    && install -d -m 0700 -o sage -g sage /var/lib/pg_sage
COPY --from=builder /build/sage-sidecar /usr/local/bin/sage-sidecar
# Roadmap 1.1: this commit's signed PGIncidentBench reports (CI puts them
# in release-bench/; a local build ships only the README). The sidecar
# verifies and ingests them at startup.
COPY release-bench/ /usr/share/pg_sage/bench/
USER sage
# security-08: without encryption_key the sidecar generates
# /var/lib/pg_sage/encryption.key (mode 600) and seals stored secrets with
# it. Mount a persistent volume here and back the file up: losing it loses
# the sealed secrets.
ENV SAGE_DATA_DIR=/var/lib/pg_sage
VOLUME /var/lib/pg_sage
EXPOSE 8080 8187
# ci-06: the sidecar probes its own /health, following api.listen_addr
# (SAGE_CONFIG_PATH or ./config.yaml) and HTTPS when SAGE_TLS_CERT and
# SAGE_TLS_KEY are set. A config passed as --config in the container
# args is invisible here: use SAGE_CONFIG_PATH.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["sage-sidecar", "healthcheck"]
ENTRYPOINT ["sage-sidecar"]
