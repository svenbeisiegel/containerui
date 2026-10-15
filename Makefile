BINARY := containerui
MAIN   := ./cmd/containerui
DIST   := dist

# VERSION is the released version and lives in ./VERSION so it is bumped in one
# place. Override it on the command line for a one off build, e.g. from CI:
#   make packages VERSION=1.0.0
VERSION     ?= $(strip $(shell cat $(CURDIR)/VERSION))
STAMP       := $(shell date +%Y%m%d-%H%M%S)
DEV_VERSION := $(VERSION)-dev-$(STAMP)

# CGO_ENABLED=0: fully static binaries; PAM is loaded at runtime.
GOFLAGS  := -trimpath
BUILDENV := CGO_ENABLED=0

PLATFORMS := linux/amd64 linux/arm64 windows/amd64 windows/arm64

# The packages ship binaries stamped with the package that owns them, and a
# configuration written by the binary's own init-config, so it always matches
# the build. The binaries and configs are built into PKG; the packages land in
# DIST.
PKG  := build/pkg
NFPM := go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0

.PHONY: all build test race vet fmt lint clean packages rpm deb msi-inputs checksums version

all: vet test build

# build cross compiles every platform with a dev version, for trying a build
# by hand; releases are the packages.
build:
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=$(DIST)/$(BINARY)_$(DEV_VERSION)_$${os}_$${arch}; \
		if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch $(BUILDENV) go build $(GOFLAGS) \
			-ldflags "-s -w -X main.version=$(DEV_VERSION)" -o $$out $(MAIN) || exit 1; \
	done

test:
	go test ./... -count=1

race:
	go test ./... -race -count=1

vet:
	go vet ./...
	GOOS=linux go vet ./...
	GOOS=windows go vet ./...

fmt:
	gofmt -l -w .

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt found unformatted files:"; gofmt -l .; exit 1)

version:
	@echo $(VERSION)

clean:
	rm -rf $(DIST) build

# packages builds the rpm and deb packages into DIST, and the binaries and
# configuration the msi is made of, which only the WiX Toolset on Windows can
# build, into PKG.
packages: rpm deb msi-inputs
	@$(MAKE) --no-print-directory checksums

# nfpm takes its files from fixed paths, see packaging/linux/nfpm.yaml, so the
# architectures are packaged one after the other. rpm and deb share the recipe:
# the target names the packager and the package stamped into the binary.
rpm deb:
	@mkdir -p $(PKG)/linux $(DIST)
	@rm -f $(PKG)/linux/config.yaml
	go run $(MAIN) init-config -compose-dir /var/lib/containerui/compose $(PKG)/linux/config.yaml
	@for arch in amd64 arm64; do \
		echo "building $(PKG)/linux/$(BINARY) for $$arch"; \
		GOOS=linux GOARCH=$$arch $(BUILDENV) go build $(GOFLAGS) \
			-ldflags "-s -w -X main.version=$(VERSION) -X main.packaging=$@" \
			-o $(PKG)/linux/$(BINARY) $(MAIN) || exit 1; \
		VERSION=$(VERSION) ARCH=$$arch $(NFPM) package --config packaging/linux/nfpm.yaml \
			--packager $@ --target $(DIST)/ || exit 1; \
	done

msi-inputs:
	@mkdir -p $(PKG)
	@rm -f $(PKG)/config.windows.yaml
	go run $(MAIN) init-config $(PKG)/config.windows.yaml
	@for arch in amd64 arm64; do \
		bin=$(PKG)/$(BINARY)_windows_$$arch.exe; \
		echo "building $$bin"; \
		GOOS=windows GOARCH=$$arch $(BUILDENV) go build $(GOFLAGS) \
			-ldflags "-s -w -X main.version=$(VERSION) -X main.packaging=msi" -o $$bin $(MAIN) || exit 1; \
	done

checksums:
	@cd $(DIST) && (sha256sum * > SHA256SUMS 2>/dev/null || shasum -a 256 * > SHA256SUMS)
	@echo "wrote $(DIST)/SHA256SUMS"
