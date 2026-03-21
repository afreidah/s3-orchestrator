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
	docker build --pull --build-arg VERSION=$(VERSION) -t $(FULL_TAG) .

scan: docker ## Scan Docker image for vulnerabilities with Trivy
	trivy image --severity CRITICAL,HIGH $(FULL_TAG)

# -------------------------------------------------------------------------
# BUILD AND PUSH (MULTI-ARCH)
# -------------------------------------------------------------------------

push: builder ## Build and push multi-arch images to registry
	@echo "Building and pushing $(FULL_TAG) for $(PLATFORMS)"
	docker buildx build \
	  --pull \
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

generate: ## Generate sqlc query code and interface mocks
	sqlc generate
	go generate ./...

test: ## Run Go tests with coverage
	go test -race -cover ./...

vet: ## Run Go vet static analysis
	go vet ./...

lint: ## Run Go linter
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run ./...

govulncheck: ## Scan Go dependencies for known vulnerabilities
	govulncheck ./...

BENCH_COUNT ?= 5
BENCH_FILE  := benchmarks/$(shell date +%Y-%m-%d)-$(shell git rev-parse --short HEAD).txt

bench: ## Run benchmark tests and save results to benchmarks/
	go test -bench=. -benchmem -count=$(BENCH_COUNT) -run='^$$' -timeout=10m ./... | tee $(BENCH_FILE)
	@echo ""
	@echo "Results saved to $(BENCH_FILE)"

bench-compare: ## Compare two benchmark files (usage: make bench-compare OLD=benchmarks/old.txt NEW=benchmarks/new.txt)
	benchstat $(OLD) $(NEW)

fuzz: ## Run fuzz tests for 30s per target
	go test -fuzz=FuzzParseSigV4Fields -fuzztime=30s ./internal/auth/
	go test -fuzz=FuzzBuildCanonicalRequest -fuzztime=30s ./internal/auth/
	go test -fuzz=FuzzParsePath -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzDeleteObjectsXML -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzCompleteMultipartXML -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzIsValidRequestID -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzLoginThrottle_RemoteAddr -fuzztime=30s ./internal/server/
	go test -fuzz=FuzzExtractClientIP -fuzztime=30s ./internal/server/

run: integration-deps ## Run locally (requires config.yaml)
	go run ./cmd/s3-orchestrator -config config.yaml

docs: ## Serve godoc locally at http://localhost:8080
	go run golang.org/x/pkgsite/cmd/pkgsite@latest -http=localhost:8080

migration: ## Create a new database migration file
	@read -p "Migration name: " name; \
	last=$$(ls internal/store/migrations/*.sql 2>/dev/null | sed 's/.*\///' | sort -n | tail -1 | grep -oE '^[0-9]+'); \
	next=$$(printf '%05d' $$(( $${last:-0} + 1 ))); \
	file="internal/store/migrations/$${next}_$${name}.sql"; \
	printf -- '-- +goose Up\n\n-- +goose Down\n' > "$$file"; \
	echo "Created $$file"

# -------------------------------------------------------------------------
# INTEGRATION TESTS
# -------------------------------------------------------------------------

COMPOSE_FILE := docker-compose.test.yml

integration-deps: ## Start integration test dependencies (MinIO + PostgreSQL + Redis)
	docker compose -f $(COMPOSE_FILE) up -d minio-1 minio-2 minio-3 postgres redis --wait
	docker compose -f $(COMPOSE_FILE) run --rm minio-setup

integration-test: integration-deps ## Run integration tests
	MINIO1_ENDPOINT=http://localhost:19000 \
	MINIO2_ENDPOINT=http://localhost:19002 \
	MINIO3_ENDPOINT=http://localhost:19004 \
	POSTGRES_HOST=localhost \
	POSTGRES_PORT=15432 \
	REDIS_ADDR=localhost:16379 \
	go test -race -v -tags integration -count=1 ./internal/integration/; \
	rc=$$?; $(MAKE) integration-clean; exit $$rc

integration-clean: ## Stop and remove integration test containers
	docker compose -f $(COMPOSE_FILE) down -v

# -------------------------------------------------------------------------
# TOOL INSTALLATION
# -------------------------------------------------------------------------

tools: ## Install build and packaging dependencies
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
	go install golang.org/x/perf/cmd/benchstat@latest
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
# APTLY PUBLISHING
# -------------------------------------------------------------------------

APTLY_URL  ?= https://apt.munchbox.cc
APTLY_REPO ?= munchbox
APTLY_USER ?= admin
DEB_DIR    ?= dist
SNAPSHOT_NAME ?= $(IMAGE)-$(shell date +%Y%m%d-%H%M%S)

publish-deb: ## Publish .deb packages to Aptly repository
	@if [ -z "$(APTLY_PASS)" ]; then echo "Error: APTLY_PASS not set (source munchbox-env.sh)"; exit 1; fi
	@echo "Publishing packages to $(APTLY_URL)..."
	@for deb in $(DEB_DIR)/*.deb; do \
		echo "Uploading $$(basename $$deb)..."; \
		curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
			-X POST -F "file=@$$deb" \
			"$(APTLY_URL)/api/files/$(IMAGE)" || exit 1; \
	done
	@echo "Adding packages to repo $(APTLY_REPO)..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X POST "$(APTLY_URL)/api/repos/$(APTLY_REPO)/file/$(IMAGE)" || exit 1
	@echo "Creating snapshot $(SNAPSHOT_NAME)..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X POST -H 'Content-Type: application/json' \
		-d '{"Name":"$(SNAPSHOT_NAME)"}' \
		"$(APTLY_URL)/api/repos/$(APTLY_REPO)/snapshots" || exit 1
	@echo "Updating published repo..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X PUT -H 'Content-Type: application/json' \
		-d '{"Snapshots":[{"Component":"main","Name":"$(SNAPSHOT_NAME)"}],"ForceOverwrite":true}' \
		'$(APTLY_URL)/api/publish/:./stable' || exit 1
	@echo "Cleaning up uploaded files..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X DELETE "$(APTLY_URL)/api/files/$(IMAGE)" || true
	@echo "Published successfully!"

# -------------------------------------------------------------------------
# CHANGELOG
# -------------------------------------------------------------------------

changelog: ## Generate CHANGELOG.md from git history
	git cliff -o CHANGELOG.md

# -------------------------------------------------------------------------
# RELEASE
# -------------------------------------------------------------------------

release: ## Tag and push to trigger a GitHub Release (reads .version)
	git tag $(VERSION)
	git push origin $(VERSION)

release-local: prep-changelog ## Dry-run GoReleaser locally (no publish)
	goreleaser release --snapshot --clean

# -------------------------------------------------------------------------
# LOAD TESTING
# -------------------------------------------------------------------------

LOADTEST_RATE     ?= 100
LOADTEST_DURATION ?= 30s
LOADTEST_SIZE     ?= 1024
LOADTEST_SEED     ?= 100
LOADTEST_WORKERS  ?= 10
LOADTEST_ENDPOINT ?= http://localhost:9000
LOADTEST_BUCKET   ?= photos

loadtest-build: ## Build the vegeta load test binary
	cd loadtest && go build -o s3-loadtest .

loadtest-put: loadtest-build ## Run PUT-only load test (use LOADTEST_RATE, LOADTEST_DURATION, LOADTEST_SIZE)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op put -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		-size $(LOADTEST_SIZE) -workers $(LOADTEST_WORKERS)

loadtest-get: loadtest-build ## Run GET-only load test (use LOADTEST_SEED for pre-seeded object count)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op get -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		-size $(LOADTEST_SIZE) -seed $(LOADTEST_SEED) -workers $(LOADTEST_WORKERS)

loadtest-mixed: loadtest-build ## Run mixed PUT/GET load test
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op mixed -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		-size $(LOADTEST_SIZE) -seed $(LOADTEST_SEED) -workers $(LOADTEST_WORKERS)

loadtest-burst: ## Run k6 burst/admission-control test (requires k6)
	@command -v k6 >/dev/null 2>&1 || { echo "Error: k6 is not installed. Install it from https://grafana.com/docs/k6/latest/set-up/install-k6/"; exit 1; }
	k6 run loadtest/k6/burst.js \
		--env S3_ENDPOINT=$(LOADTEST_ENDPOINT) --env S3_BUCKET=$(LOADTEST_BUCKET)

loadtest-k6: ## Run k6 mixed CRUD workflow test (requires k6)
	@command -v k6 >/dev/null 2>&1 || { echo "Error: k6 is not installed. Install it from https://grafana.com/docs/k6/latest/set-up/install-k6/"; exit 1; }
	k6 run loadtest/k6/mixed.js \
		--env S3_ENDPOINT=$(LOADTEST_ENDPOINT) --env S3_BUCKET=$(LOADTEST_BUCKET)

# -------------------------------------------------------------------------
# DEPLOYMENT DEMOS
# -------------------------------------------------------------------------

kubernetes-demo: ## Run the s3-orchestrator in k3d (requires docker, k3d, kubectl)
	./deploy/kubernetes/local/demo.sh

nomad-demo: ## Run the s3-orchestrator in Nomad dev mode (requires docker, nomad)
	./deploy/nomad/local/demo.sh

# -------------------------------------------------------------------------
# WEBSITE
# -------------------------------------------------------------------------

WEB_IMAGE  := $(REGISTRY)/s3-orchestrator-web
WEB_TAG    ?= $(VERSION)

GODOC_PKGS := admin audit auth backend breaker config counter encryption httputil lifecycle proxy server store telemetry ui worker

web-tools: ## Install Hugo and gomarkdoc for local website development
	go install github.com/gohugoio/hugo@latest
	go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest

web-godoc: ## Generate Go API reference markdown for the website
	@mkdir -p web/content/godoc
	@for pkg in $(GODOC_PKGS); do \
		echo "  godoc: internal/$$pkg"; \
		printf -- '---\ntitle: "%s"\n---\n\n' "$$pkg" > web/content/godoc/$$pkg.md; \
		gomarkdoc ./internal/$$pkg >> web/content/godoc/$$pkg.md; \
		sed -i '/^# '"$$pkg"'$$/d' web/content/godoc/$$pkg.md; \
	done

web-serve: web-godoc ## Serve the project website locally
	cd web && hugo serve

web-build: web-godoc ## Build the project website
	cd web && hugo --minify

web-docker: ## Build website Docker image for local architecture
	docker build --pull -f web/Dockerfile -t $(WEB_IMAGE):$(WEB_TAG) .

web-push: builder ## Build and push multi-arch website image to registry
	docker buildx build \
	  --pull \
	  --platform $(PLATFORMS) \
	  -f web/Dockerfile \
	  -t $(WEB_IMAGE):$(WEB_TAG) \
	  --output type=image,push=true \
	  .

# -------------------------------------------------------------------------
# CLEANUP
# -------------------------------------------------------------------------

clean: ## Remove build artifacts, demo environments, containers, and volumes
	# --- Stop Nomad job and agent ---
	NOMAD_ADDR=http://127.0.0.1:4646 nomad job stop -purge s3-orchestrator 2>/dev/null || true
	pkill -f '[n]omad agent -dev' 2>/dev/null || true
	rm -f /tmp/nomad-demo.pid
	# --- Delete k3d cluster ---
	k3d cluster delete s3-orchestrator-demo 2>/dev/null || true
	# --- Tear down compose services and volumes ---
	docker compose -f $(COMPOSE_FILE) down -v --remove-orphans 2>/dev/null || true
	# --- Remove orphaned volumes from previous runs ---
	docker volume prune -f 2>/dev/null || true
	# --- Build artifacts ---
	go clean
	rm -f s3-orchestrator loadtest/s3-loadtest
	rm -rf dist/ *.deb packaging/changelog.gz
	docker rmi $(FULL_TAG) 2>/dev/null || true
	docker rmi s3-orchestrator:local 2>/dev/null || true

.PHONY: help builder build docker push generate test vet lint govulncheck bench bench-compare run docs migration integration-deps integration-test integration-clean tools prep-changelog deb deb-lint deb-all publish-deb changelog release release-local loadtest-build loadtest-put loadtest-get loadtest-mixed loadtest-burst loadtest-k6 kubernetes-demo nomad-demo web-tools web-godoc web-serve web-build web-docker web-push clean
.DEFAULT_GOAL := help
