PROJECT := omni-infra-provider-nutanix
GO ?= go
CONTAINER_ENGINE ?= docker
IMAGE ?=
IMAGE_SOURCE ?=
IMAGE_VERSION ?= $(shell cat VERSION)
IMAGE_REVISION ?=
BUILD_ARGS ?=
PROTOC ?= protoc
PROTOC_GEN_GO_VERSION ?= $(shell $(GO) list -m -f '{{.Version}}' google.golang.org/protobuf)

.PHONY: all build image generate
all: build

build:
	CGO_ENABLED=0 $(GO) build -mod=readonly -trimpath \
		-ldflags "-X github.com/xerxist/omni-nutanix-pc-infra-provider/internal/pkg/buildinfo.Version=$(IMAGE_VERSION)" \
		-o _out/$(PROJECT) ./cmd/omni-infra-provider-nutanix

# Explicit manual target only. Registry/image metadata comes from the caller/CI.
image:
	@test -n "$(IMAGE)" || { echo 'Set IMAGE to your registry/repository/tag'; exit 1; }
	$(CONTAINER_ENGINE) build --build-arg IMAGE_SOURCE="$(IMAGE_SOURCE)" \
		--build-arg IMAGE_VERSION="$(IMAGE_VERSION)" --build-arg IMAGE_REVISION="$(IMAGE_REVISION)" \
		$(BUILD_ARGS) -t "$(IMAGE)" .

generate:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	$(PROTOC) -I=api/specs --go_out=paths=source_relative:api/specs api/specs/specs.proto
