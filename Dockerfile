# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27

# ---- build ------------------------------------------------------------------
# Runs on the builder's native platform and cross-compiles for the target, so
# multi-arch images build at native speed with no QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

# Dependencies are downloaded in their own layer, keyed only on go.mod/go.sum,
# so source edits don't invalidate the module cache.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X url_shortener/internal/buildinfo.Version=${VERSION}" \
      -o /out/ \
      ./cmd/...

# ---- runtime ----------------------------------------------------------------
# Distroless static: no shell or package manager, just CA certificates (needed
# for TLS to Neon), tzdata and a non-root user.
FROM gcr.io/distroless/static-debian13:nonroot AS runtime

COPY --from=build /out/api /out/migrate /usr/local/bin/

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/api"]
