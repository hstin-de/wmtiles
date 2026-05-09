# syntax=docker/dockerfile:1.7

# Stage 1: viewer bundle (Bun)
FROM oven/bun:1 AS viewer
WORKDIR /src

COPY package.json bun.lock ./
COPY wmtiles-js/package.json wmtiles-js/package.json
COPY cmd/wmtiles/web/package.json cmd/wmtiles/web/package.json
RUN bun install --frozen-lockfile

COPY wmtiles-js wmtiles-js
COPY cmd/wmtiles/web cmd/wmtiles/web
RUN bun -F wmtiles build \
 && bun -F wmtiles-viewer build

# Stage 2: CLI binary (Go + cgo + eccodes)
# trixie ships libeccodes-dev ≥ 2.36; bookworm only has 2.28, which is missing
# codes_get_float_array (added in eccodes 2.31).
FROM golang:1.26-trixie AS build
RUN apt-get update \
 && apt-get install -y --no-install-recommends libeccodes-dev pkg-config \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=viewer /src/cmd/wmtiles/web/dist cmd/wmtiles/web/dist

ARG VERSION=dev
ARG COMMIT=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build \
        -tags embed \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/wmtiles ./cmd/wmtiles/

# Stage 3: runtime
FROM debian:trixie-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        libeccodes0 \
        ca-certificates \
        curl \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/wmtiles /usr/local/bin/wmtiles

WORKDIR /data

EXPOSE 8080

ENTRYPOINT ["wmtiles"]
CMD ["--help"]
