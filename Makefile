# -------------------------------------------------------------------------------
# S3 Orchestrator - Build, Package, and Push
#
# Author: Alex Freidah
#
# Go S3 orchestrator for unified S3-compatible storage access. Builds multi-arch
# container images and Debian packages.
# -------------------------------------------------------------------------------

REGISTRY   ?= registry.munchbox.cc
IMAGE      := s3-orchestrator
VERSION    ?= latest

FULL_TAG   := $(REGISTRY)/$(IMAGE):$(VERSION)
CACHE_TAG  := $(REGISTRY)/$(IMAGE):cache
PLATFORMS  := linux/amd64,linux/arm64

# --- Go build flags ---
GO_LDFLAGS := -s -w -X github.com/afreidah/s3-orchestrator/internal/telemetry.Version=$(VERSION)

# --- Debian packaging ---
DEB_VERSION ?= $(shell v="$(VERSION)"; if echo "$$v" | grep -qE '^[0-9]'; then echo "$$v"; else echo "0.0.0+$$v"; fi)
DEB_ARCH    ?= $(shell dpkg --print-architecture 2>/dev/null || echo amd64)
DEB_OUT      = s3-orchestrator_$(DEB_VERSION)_$(DEB_ARCH).deb

# -------------------------------------------------------------------------
# DEFAULT TARGET
# -------------------------------------------------------------------------

help: ## Display available Make targets
	@echo ""
	@echo "Available targets:"
	@echo ""
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' Makefile | \
		awk 'BEGIN {FS = ":.*?## "} {printf "  %-15s %s\n", $$1, $$2}'
	@echo ""

# -------------------------------------------------------------------------
# BUILDX SETUP
# -------------------------------------------------------------------------

builder: ## Ensure the Buildx builder exists
	@docker buildx inspect s3-orchestrator-builder >/dev/null 2>&1 || \
		docker buildx create --name s3-orchestrator-builder --driver-opt network=host --use
	@docker buildx inspect --bootstrap

# -------------------------------------------------------------------------
# BUILD (LOCAL)
# -------------------------------------------------------------------------

build: ## Build for local architecture
	@echo "Building $(FULL_TAG) for local architecture"
	docker build --build-arg VERSION=$(VERSION) -t $(FULL_TAG) .

# -------------------------------------------------------------------------
# BUILD AND PUSH (MULTI-ARCH)
# -------------------------------------------------------------------------

push: builder ## Build and push multi-arch images to registry
	@echo "Building and pushing $(FULL_TAG) for $(PLATFORMS)"
	docker buildx build \
	  --platform $(PLATFORMS) \
	  --build-arg VERSION=$(VERSION) \
	  -t $(FULL_TAG) \
	  --cache-from type=registry,ref=$(CACHE_TAG) \
	  --cache-to type=registry,ref=$(CACHE_TAG),mode=max \
	  --output type=image,push=true \
	  .

# -------------------------------------------------------------------------
# DEVELOPMENT
# -------------------------------------------------------------------------

generate: ## Generate sqlc query code
	sqlc generate

test: ## Run Go tests with coverage
	go test -race -cover ./...

vet: ## Run Go vet static analysis
	go vet ./...

lint: ## Run Go linter
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...

run: ## Run locally (requires config.yaml)
	go run ./cmd/s3-orchestrator -config config.yaml

# -------------------------------------------------------------------------
# INTEGRATION TESTS
# -------------------------------------------------------------------------

COMPOSE_FILE := docker-compose.test.yml

integration-deps: ## Start integration test dependencies (MinIO + PostgreSQL)
	docker compose -f $(COMPOSE_FILE) up -d minio-1 minio-2 postgres --wait
	docker compose -f $(COMPOSE_FILE) run --rm minio-setup

integration-test: integration-deps ## Run integration tests
	MINIO1_ENDPOINT=http://localhost:19000 \
	MINIO2_ENDPOINT=http://localhost:19002 \
	POSTGRES_HOST=localhost \
	POSTGRES_PORT=15432 \
	go test -race -v -tags integration -count=1 ./internal/integration/; \
	rc=$$?; $(MAKE) integration-clean; exit $$rc

integration-clean: ## Stop and remove integration test containers
	docker compose -f $(COMPOSE_FILE) down -v

# -------------------------------------------------------------------------
# TOOL INSTALLATION
# -------------------------------------------------------------------------

tools: ## Install build and packaging dependencies
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
	sudo apt-get update && sudo apt-get install -y lintian

# -------------------------------------------------------------------------
# DEBIAN PACKAGING
# -------------------------------------------------------------------------

# --- Map dpkg architecture names to GOARCH ---
GOARCH_amd64 := amd64
GOARCH_arm64 := arm64
GOARCH       := $(GOARCH_$(DEB_ARCH))

prep-changelog: ## Compress changelog for Debian packaging
	@gzip -9 -n -c packaging/changelog > packaging/changelog.gz

deb: prep-changelog ## Build a .deb package for DEB_ARCH (default: host arch)
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build \
	  -ldflags="$(GO_LDFLAGS)" \
	  -o dist/s3-orchestrator ./cmd/s3-orchestrator
	VERSION=$(DEB_VERSION) GOARCH=$(GOARCH) nfpm package --packager deb --target $(DEB_OUT)

deb-lint: deb ## Run lintian on the .deb package
	lintian --tag-display-limit 0 $(DEB_OUT)

deb-all: ## Build .deb packages for amd64 and arm64
	$(MAKE) deb DEB_ARCH=amd64
	$(MAKE) deb DEB_ARCH=arm64

# -------------------------------------------------------------------------
# RELEASE
# -------------------------------------------------------------------------

release-local: prep-changelog ## Dry-run GoReleaser locally (no publish)
	goreleaser release --snapshot --clean

# -------------------------------------------------------------------------
# CLEANUP
# -------------------------------------------------------------------------

clean: ## Remove build artifacts and local image
	go clean
	rm -rf dist/ *.deb packaging/changelog.gz
	docker rmi $(FULL_TAG) || true

.PHONY: help builder build push generate test vet lint run integration-deps integration-test integration-clean tools prep-changelog deb deb-lint deb-all release-local clean
.DEFAULT_GOAL := help
