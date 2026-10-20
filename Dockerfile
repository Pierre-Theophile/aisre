# SPDX-License-Identifier: Apache-2.0
#
# The aisre image: the one static binary (constitution X) on a distroless base, as a non-root
# user. Everything is a subcommand, so the same image serves the API and runs any feeder:
#
#   docker run ghcr.io/pierre-theophile/aisre serve --db "$PG_DSN" --migrate --auth oidc ...
#   docker run ghcr.io/pierre-theophile/aisre feed k8s --server https://... --cluster-name prod
#
# `serve` listens on $PORT when it is set (Cloud Run, Knative), else on :8080.

# The build runs on the builder's own platform and cross-compiles: Go needs no emulation for a
# cgo-free binary, so a multi-arch image costs one compile per architecture and nothing more.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/aisre ./cmd/aisre

# distroless/static carries CA certificates and tzdata and nothing else: no shell, no package
# manager. nonroot is uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/aisre /usr/local/bin/aisre
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/aisre"]
CMD ["--help"]
