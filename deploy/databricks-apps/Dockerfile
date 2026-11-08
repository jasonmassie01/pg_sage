# Build from the repository root. This image is a build tool only; Apps runs
# the exported native binary, not this container.
FROM node:22-alpine AS frontend
WORKDIR /src/sidecar
COPY sidecar/web/package.json sidecar/web/package-lock.json ./web/
RUN cd web && npm ci
COPY sidecar/web/ ./web/
COPY sidecar/internal/api/dist/ ./internal/api/dist/
RUN cd web && npm run build

FROM golang:1.27-alpine AS builder
RUN apk add --no-cache build-base git
WORKDIR /src/sidecar
COPY VERSION /src/VERSION
COPY sidecar/go.mod sidecar/go.sum ./
RUN go mod download
COPY sidecar/ ./
COPY --from=frontend /src/sidecar/internal/api/dist/ ./internal/api/dist/
ARG VERSION
ARG COMMIT=unknown
ARG BUILD_DATE
RUN PG_SAGE_BUILD_VERSION="${VERSION:-$(cat /src/VERSION)}" \
    && PG_SAGE_BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" \
    && mkdir -p /out && CGO_ENABLED=1 go build -trimpath -tags netgo,osusergo \
    -ldflags="-s -w -linkmode external -extldflags \"-static\" -X main.version=${PG_SAGE_BUILD_VERSION} -X main.commit=${COMMIT} -X main.date=${PG_SAGE_BUILD_DATE}" \
    -o /out/pg_sage ./cmd/pg_sage_sidecar/ \
    && /out/pg_sage --version | grep -q 'sql-ast: libpg_query'

FROM scratch AS artifact
COPY --from=builder /out/pg_sage /pg_sage
