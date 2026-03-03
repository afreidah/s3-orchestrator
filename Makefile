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
VERSION    ?= $(shell cat .version)

FULL_TAG   := $(REGISTRY)/$(IMAGE):$(VERSION)
CACHE_TAG  := $(REGISTRY)/$(IMAGE):cache
PLATFORMS  := linux/amd64,linux/arm64

# --- Go build flags ---
GO_LDFLAGS := -s -w -X github.com/afreidah/s3-orchestrator/internal/telemetry.Version=$(VERSION)


# -------------------------------------------------------------------------
# DEFAULT TARGET
# -------------------------------------------------------------------------

help: ## Display available Make targets
	@echo ""
	@echo "Available targets:"
	@echo ""
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' Makefile | \
		awk 'BEGIN {FS = ":.*?## "} {printf "  %-20s %s\n", $$1, $$2}'
	@echo ""

# -------------------------------------------------------------------------
# BUILDX SETUP
# -------------------------------------------------------------------------

builder: ## Ensure the Buildx builder exists
	@docker buildx inspect s3-orchestrator-builder >/dev/null 2>&1 || \
		docker buildx create --name s3-orchestrator-builder --driver-opt network=host --use
	@docker buildx inspect --bootstrap

# -------------------------------------------------------------------------
# BUILD
# -------------------------------------------------------------------------

build: ## Build the Go binary for the local platform
	go build -ldflags="$(GO_LDFLAGS)" -o s3-orchestrator ./cmd/s3-orchestrator

# -------------------------------------------------------------------------
# DOCKER
# -------------------------------------------------------------------------

docker: ## Build Docker image for local architecture
	@echo "Building $(FULL_TAG) for local architecture"
	docker build --build-arg VERSION=$(VERSION) -t $(FULL_TAG) .

scan: docker ## Scan Docker image for vulnerabilities with Trivy
	trivy image --severity CRITICAL,HIGH $(FULL_TAG)

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
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run ./...

bench: ## Run benchmark tests
	go test -bench=. -benchmem -run='^$$' ./...

fuzz: ## Run fuzz tests for 30s per target
	go test -fuzz=FuzzParseSigV4Fields -fuzztime=30s ./internal/auth/
	go test -fuzz=FuzzParsePath -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzDeleteObjectsXML -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzCompleteMultipartXML -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzIsValidRequestID -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzLoginThrottle_RemoteAddr -fuzztime=30s ./internal/server/

run: integration-deps ## Run locally (requires config.yaml)
	go run ./cmd/s3-orchestrator -config config.yaml

docs: ## Serve godoc locally at http://localhost:8080
	go run golang.org/x/pkgsite/cmd/pkgsite@latest -http=localhost:8080

migration: ## Create a new database migration file
	@read -p "Migration name: " name; \
	last=$$(ls internal/storage/migrations/*.sql 2>/dev/null | sed 's/.*\///' | sort -n | tail -1 | grep -oE '^[0-9]+'); \
	next=$$(printf '%05d' $$(( $${last:-0} + 1 ))); \
	file="internal/storage/migrations/$${next}_$${name}.sql"; \
	printf -- '-- +goose Up\n\n-- +goose Down\n' > "$$file"; \
	echo "Created $$file"

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
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0
	sudo apt-get update && sudo apt-get install -y lintian
	curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sudo sh -s -- -b /usr/local/bin

# -------------------------------------------------------------------------
# DEBIAN PACKAGING
# -------------------------------------------------------------------------

prep-changelog: ## Compress changelog for Debian packaging
	@gzip -9 -n -c packaging/changelog > packaging/changelog.gz

deb: prep-changelog ## Build .deb packages via GoReleaser snapshot
	goreleaser release --snapshot --clean --skip=publish

deb-lint: deb ## Run lintian on the .deb packages
	@for f in dist/*.deb; do echo "--- $$f ---"; lintian --tag-display-limit 0 "$$f"; done

# -------------------------------------------------------------------------
# RELEASE
# -------------------------------------------------------------------------

release: ## Tag and push to trigger a GitHub Release (reads .version)
	git tag $(VERSION)
	git push origin $(VERSION)

release-local: prep-changelog ## Dry-run GoReleaser locally (no publish)
	goreleaser release --snapshot --clean

# -------------------------------------------------------------------------
# DEPLOYMENT DEMOS
# -------------------------------------------------------------------------

kubernetes-demo: ## Run the s3-orchestrator in k3d (requires docker, k3d, kubectl)
	./deploy/kubernetes/local/demo.sh

nomad-demo: ## Run the s3-orchestrator in Nomad dev mode (requires docker, nomad)
	./deploy/nomad/local/demo.sh

# -------------------------------------------------------------------------
# CLEANUP
# -------------------------------------------------------------------------

clean: integration-clean ## Remove build artifacts, local image, and test containers
	go clean
	rm -f s3-orchestrator
	rm -rf dist/ *.deb packaging/changelog.gz
	docker rmi $(FULL_TAG) 2>/dev/null || true
	docker rmi s3-orchestrator:local 2>/dev/null || true
	k3d cluster delete s3-orchestrator-demo 2>/dev/null || true
	@if [ -f /tmp/nomad-demo.pid ]; then \
		NOMAD_ADDR=http://127.0.0.1:4646 nomad job stop -purge s3-orchestrator 2>/dev/null || true; \
		kill $$(cat /tmp/nomad-demo.pid) 2>/dev/null || true; \
		rm -f /tmp/nomad-demo.pid; \
	fi

.PHONY: help builder build docker push generate test vet lint run docs migration integration-deps integration-test integration-clean tools prep-changelog deb deb-lint deb-all release release-local kubernetes-demo nomad-demo clean
.DEFAULT_GOAL := help
