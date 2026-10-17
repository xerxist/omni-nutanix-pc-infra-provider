# syntax=docker/dockerfile:1
# Preparation only: pin BUILD_IMAGE and RUNTIME_IMAGE to reviewed digests in CI.
ARG GO_VERSION=1.26.2
ARG BUILD_IMAGE=docker.io/golang:${GO_VERSION}-alpine
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot
ARG IMAGE_VERSION=dev

FROM --platform=$BUILDPLATFORM ${BUILD_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY api/ ./api/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS
ARG TARGETARCH
ARG GO_LDFLAGS="-s -w"
ARG IMAGE_VERSION
# Generated protobuf sources are checked in; no floating generator is installed.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    case "${IMAGE_VERSION}" in ''|[!A-Za-z0-9]*|*[!A-Za-z0-9._+-]*) echo 'Invalid public IMAGE_VERSION' >&2; exit 1;; esac; \
    test "${#IMAGE_VERSION}" -le 128 || exit 1; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=readonly -trimpath \
    -ldflags "${GO_LDFLAGS} -X github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/buildinfo.Version=${IMAGE_VERSION}" \
    -o /out/provider ./cmd/omni-infra-provider-nutanix

FROM ${RUNTIME_IMAGE} AS image-omni-infra-provider-nutanix
ARG IMAGE_SOURCE=""
ARG IMAGE_VERSION
ARG IMAGE_REVISION=""
LABEL org.opencontainers.image.title="Omni Nutanix Prism Central provider" \
      org.opencontainers.image.source="${IMAGE_SOURCE}" \
      org.opencontainers.image.version="${IMAGE_VERSION}" \
      org.opencontainers.image.revision="${IMAGE_REVISION}" \
      org.opencontainers.image.licenses="MPL-2.0"
COPY --from=build --chmod=0555 /out/provider /usr/local/bin/provider
USER 65532:65532
ENV SSL_CERT_DIR=/etc/ssl/certs:/etc/provider/certs
ENTRYPOINT ["/usr/local/bin/provider"]
