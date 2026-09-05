# -------------------------------------------------------------------------------
# S3 Orchestrator - Build, Package, and Push
#
# Author: Alex Freidah
#
# Go S3 orchestrator for unified S3-compatible storage access. Builds multi-arch
# container images and Debian packages.
# -------------------------------------------------------------------------------

REGISTRY   ?= $(or $(DOCKER_REGISTRY),registry.example.com)
IMAGE      := s3-orchestrator
VERSION    ?= $(shell cat .version)

FULL_TAG   := $(REGISTRY)/$(IMAGE):$(VERSION)
PLATFORMS  := linux/amd64,linux/arm64

# Install location for `make install` (override e.g. BINDIR=$(HOME)/.local/bin
# for a no-sudo user install).
PREFIX     ?= /usr/local
BINDIR     ?= $(PREFIX)/bin

# --- Go build flags ---
GO_LDFLAGS := -s -w -X github.com/afreidah/s3-orchestrator/internal/observe/telemetry.Version=$(VERSION)


# -------------------------------------------------------------------------
# DEFAULT TARGET
# -------------------------------------------------------------------------

help: ## Display available Make targets
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage: make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_-]+:.*?## / { \
			gsub(/[A-Z_][A-Z0-9_]*=[a-zA-Z0-9_|-]+/, "\033[33m&\033[0m", $$2); \
			printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2 \
		} \
		/^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)}' $(MAKEFILE_LIST)

##@ Build

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

install: build ## Install the binary to BINDIR (default /usr/local/bin; needs sudo, or set BINDIR=$(HOME)/.local/bin)
	install -d $(BINDIR)
	install -m 0755 s3-orchestrator $(BINDIR)/s3-orchestrator
	@echo "installed s3-orchestrator $(VERSION) -> $(BINDIR)/s3-orchestrator"

uninstall: ## Remove the installed binary from BINDIR
	rm -f $(BINDIR)/s3-orchestrator
	@echo "removed $(BINDIR)/s3-orchestrator"

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
	  --output type=image,push=true \
	  .

##@ Quality

# -------------------------------------------------------------------------
# DEVELOPMENT
# -------------------------------------------------------------------------

generate: ## Generate sqlc query code and interface mocks
	go tool sqlc generate
	go generate ./...
	@$(MAKE) --no-print-directory strip-mock-package-docs

# mockgen writes "Package X is a generated GoMock package." above every package
# clause. In a package whose mocks are not _test.go files that comment is a real
# package comment, and go/doc concatenates it onto the one in doc.go. Strip it
# from the non-test mocks so doc.go stays the only package comment.
strip-mock-package-docs:
	@for f in $$(git ls-files '*.go' | grep -v '_test\.go$$' | xargs grep -l 'is a generated GoMock package\.'); do \
		perl -0pi -e 's{\n// Package \w+ is a generated GoMock package\.\n(package )}{\n$$1}' $$f; \
		echo "  stripped mockgen package comment: $$f"; \
	done

test: ## Run Go tests with coverage
	go test -race -cover ./...

test-fast: ## Run Go tests without race or coverage for quick iteration
	go test ./...

vet: ## Run Go vet static analysis
	go vet ./...

check: ## Run fast local checks for contributor iteration
	$(MAKE) test-fast
	$(MAKE) vet
	$(MAKE) doc-stub-check

# Keep in sync with the version pinned in .github/workflows/ci.yml.
GOLANGCI_VERSION ?= v2.13.0
GOLANGCI := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

# Which modernizers exist is decided by this pin; which ones apply is decided
# by the go directive in go.mod, so only this has to be bumped by hand.
GOPLS_VERSION ?= v0.22.0

# loadtest is a separate Go module, so the root ./... never reaches it and it
# went unlinted entirely. Every module has to be named explicitly.
GO_MODULE_DIRS := . loadtest

lint: ## Run Go linter
	@for dir in $(GO_MODULE_DIRS); do \
		echo "==> lint $$dir"; \
		(cd $$dir && $(GOLANGCI) run ./...) || exit 1; \
	done

fmt: ## Apply the formatting lint enforces (gofmt + goimports)
	@for dir in $(GO_MODULE_DIRS); do \
		echo "==> fmt $$dir"; \
		(cd $$dir && $(GOLANGCI) fmt ./...) || exit 1; \
	done

modernize: ## Report gopls modernization/hygiene hints (skips generated files)
	GOFLAGS=-tags=integration go run golang.org/x/tools/gopls@$(GOPLS_VERSION) check -severity=hint \
		$$(git ls-files '*.go' | xargs grep -L 'DO NOT EDIT')

openapi: ## Regenerate docs/openapi.yaml from the admin route table
	go test ./internal/transport/admin/ -run TestOpenAPISpec_MatchesRouteTable -update

# The oasdiff pin lives here alone; CI invokes this target rather than
# repeating the version, so the two cannot drift apart.
OASDIFF_VERSION ?= v1.26.1
OASDIFF_BASE    ?= origin/main
OASDIFF_FORMAT  ?= text

openapi-breaking: ## Report admin API contract breaks against OASDIFF_BASE
	@git show $(OASDIFF_BASE):docs/openapi.yaml > /tmp/openapi-base.yaml
	@go run github.com/oasdiff/oasdiff@$(OASDIFF_VERSION) breaking \
		/tmp/openapi-base.yaml docs/openapi.yaml -f $(OASDIFF_FORMAT)

doc-stub-check: ## Fail if tautological '// Foo foo.' doc-comment stubs reappear
	bash scripts/check-doc-stubs.sh

govulncheck: ## Scan Go dependencies for known vulnerabilities
	go tool govulncheck ./...

# -------------------------------------------------------------------------
# COVERAGE (used by SonarCloud and local sonar-scan target)
# -------------------------------------------------------------------------

# Flags mirror .github/workflows/ci.yml so the local profile matches what
# the CI sonarqube job consumes. -coverpkg=./... measures cross-package
# coverage; -covermode=atomic is required for race-enabled runs.
COVER_FLAGS := -race -coverprofile=coverage.out -covermode=atomic -coverpkg=./...
INTEGRATION_COVER_FLAGS := -race -v -tags integration -count=1 \
	-coverprofile=integration-coverage.out -covermode=atomic -coverpkg=./...

coverage: ## Generate coverage.out from unit tests (mirrors CI test job)
	go test $(COVER_FLAGS) ./...

integration-coverage: ## Generate integration-coverage.out (requires Docker for testcontainers)
	go test $(INTEGRATION_COVER_FLAGS) ./internal/integration/ ./internal/store/postgres/

# -------------------------------------------------------------------------
# SONARCLOUD
# -------------------------------------------------------------------------

# SONAR_TOKEN must be set in the environment. Generate one at
# https://sonarcloud.io/account/security and export it from your shell rc.
# The scanner reads sonar-project.properties for project key, organisation,
# and coverage report paths.
sonar-scan: ## Run SonarCloud analysis on the current branch (requires SONAR_TOKEN, coverage.out, integration-coverage.out)
	@test -n "$$SONAR_TOKEN" || { echo "Error: SONAR_TOKEN is not set. Generate one at https://sonarcloud.io/account/security."; exit 1; }
	@test -f coverage.out || { echo "Error: coverage.out missing. Run 'make coverage' first (or 'make sonar-pr' to do everything)."; exit 1; }
	@test -f integration-coverage.out || { echo "Error: integration-coverage.out missing. Run 'make integration-coverage' first (or 'make sonar-pr')."; exit 1; }
	@branch="$$(git branch --show-current)"; \
	if [ -z "$$branch" ]; then echo "Error: detached HEAD; check out a branch first."; exit 1; fi; \
	echo "Scanning branch: $$branch"; \
	docker run --rm \
		-e SONAR_TOKEN \
		-e SONAR_HOST_URL=https://sonarcloud.io \
		-v "$$PWD:/usr/src" \
		sonarsource/sonar-scanner-cli:latest \
		-Dsonar.branch.name="$$branch"

sonar-pr: ## End-to-end pre-PR check: lint + unit coverage + integration coverage + SonarCloud scan
	@test -n "$$SONAR_TOKEN" || { echo "Error: SONAR_TOKEN is not set. Generate one at https://sonarcloud.io/account/security."; exit 1; }
	$(MAKE) lint
	$(MAKE) coverage
	$(MAKE) integration-coverage
	$(MAKE) sonar-scan

preflight: ## Run the full release preflight locally (mirrors CI release workflow)
	go tool sqlc diff
	$(MAKE) lint
	go test -race ./...
	go test -race -v -tags integration -count=1 ./internal/integration/

##@ Benchmarks and Fuzzing

BENCH_COUNT    ?= 10
BENCH_TIME     ?= 2s
FUZZ_TIME      ?= 30s
BENCH_RAW_FILE := benchmarks/raw/$(shell date +%Y-%m-%d)-$(shell git rev-parse --short HEAD).txt
BENCH_FILE     := benchmarks/$(shell date +%Y-%m-%d)-$(shell git rev-parse --short HEAD).txt

# Capture the full unfiltered go-test output to benchmarks/raw/ so future
# tooling and post-hoc debugging keep every field go-test emits, then
# pipe through the benchstat-friendly filter to benchmarks/ for the
# trend-tracking summary.
bench: ## Run all benchmarks (override: BENCH_COUNT=10 BENCH_TIME=3s make bench)
	@mkdir -p benchmarks/raw
	go test -bench=. -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=30m ./... \
		| tee $(BENCH_RAW_FILE) \
		| grep -E '^(Benchmark|pkg:|goos:|goarch:|cpu:)' \
		| tee $(BENCH_FILE)
	@echo ""
	@echo "Raw output:        $(BENCH_RAW_FILE)"
	@echo "Filtered summary:  $(BENCH_FILE)"

bench-auth: ## Run auth hot-path benchmarks (SigV4, signing key cache, token auth)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=10m ./internal/transport/auth/

bench-crypto: ## Run encryption throughput benchmarks (encrypt, decrypt, round-trip)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=10m ./internal/encryption/

bench-compression: ## Run compression throughput benchmarks (encode, decode, incompressible vs compressible)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=10m ./internal/compression/

bench-cache: ## Run cache and buffer pool benchmarks (LocationCache, TTLCache, bufpool)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=10m ./internal/proxy/object/ ./internal/util/syncutil/ ./internal/util/bufpool/

bench-usage: ## Run usage tracking benchmarks (WithinLimits, Record)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=10m ./internal/counter/

bench-quota: ## Run striped quota benchmarks (requires Docker — stripe fan-out, claim admit/decline)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=30m -tags integration ./internal/store/postgres/

bench-integration: ## Run integration benchmarks (requires Docker — PutObject, ListObjects, Rebalance)
	go test -bench=Benchmark -benchmem -count=$(BENCH_COUNT) -benchtime=$(BENCH_TIME) -run='^$$' -timeout=30m -tags integration ./internal/integration/

bench-compare: ## Compare two benchmark runs (OLD=file NEW=file, defaults to two most recent)
	@OLD="$(OLD)"; NEW="$(NEW)"; \
	if [ -z "$$OLD" ] || [ -z "$$NEW" ]; then \
		files=$$(ls -1t benchmarks/*.txt 2>/dev/null | head -2); \
		if [ "$$(echo "$$files" | wc -l)" -lt 2 ]; then \
			echo "bench-compare: need at least 2 files in benchmarks/"; exit 1; \
		fi; \
		NEW=$$(echo "$$files" | sed -n '1p'); \
		OLD=$$(echo "$$files" | sed -n '2p'); \
		echo "Comparing OLD=$$OLD NEW=$$NEW"; \
	fi; \
	benchstat "$$OLD" "$$NEW"

fuzz: ## Run fuzz tests (override: FUZZ_TIME=5m make fuzz)
	go test -fuzz=FuzzParseSigV4Fields -fuzztime=$(FUZZ_TIME) ./internal/transport/auth/
	go test -fuzz=FuzzBuildCanonicalRequest -fuzztime=$(FUZZ_TIME) ./internal/transport/auth/
	go test -fuzz=FuzzBuildPresignedCanonicalRequest -fuzztime=$(FUZZ_TIME) ./internal/transport/auth/
	go test -fuzz=FuzzParsePath -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzDeleteObjectsXML -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzCompleteMultipartXML -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzIsValidRequestID -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzExtractClientIP -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzValidMetadataToken -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzLoginThrottle_RemoteAddr -fuzztime=$(FUZZ_TIME) ./internal/transport/httputil/
	go test -fuzz=FuzzPatternMatches -fuzztime=$(FUZZ_TIME) ./internal/transport/cors/
	go test -fuzz=FuzzParseHeaderList -fuzztime=$(FUZZ_TIME) ./internal/transport/cors/
	go test -fuzz=FuzzParsePlaintextRange -fuzztime=$(FUZZ_TIME) ./internal/proxy/object/
	go test -fuzz=FuzzParseQueryInt -fuzztime=$(FUZZ_TIME) ./internal/transport/s3api/
	go test -fuzz=FuzzParseHeader -fuzztime=$(FUZZ_TIME) ./internal/encryption/
	go test -fuzz=FuzzCiphertextRange -fuzztime=$(FUZZ_TIME) ./internal/encryption/
	go test -fuzz=FuzzUnpackKeyData -fuzztime=$(FUZZ_TIME) ./internal/encryption/

fuzz-import: ## Import crashing inputs from the latest nightly fuzz CI run
	@echo "Downloading fuzz corpus artifacts from latest fuzz workflow run..."
	@run_id=$$(gh run list -w fuzz.yml --status failure --limit 1 --json databaseId --jq '.[0].databaseId'); \
	if [ -z "$$run_id" ]; then echo "No failed fuzz runs found."; exit 0; fi; \
	echo "Run ID: $$run_id"; \
	tmpdir=$$(mktemp -d); \
	gh run download "$$run_id" -D "$$tmpdir" -p 'fuzz-corpus-*' 2>/dev/null || true; \
	count=0; \
	for f in $$(find "$$tmpdir" -path '*/testdata/fuzz/*/*' -type f 2>/dev/null); do \
		rel=$${f#$$tmpdir/}; \
		dest=$$rel; \
		mkdir -p $$(dirname "$$dest"); \
		if [ ! -f "$$dest" ]; then \
			cp "$$f" "$$dest"; \
			echo "  Added $$dest"; \
			count=$$((count + 1)); \
		fi; \
	done; \
	rm -rf "$$tmpdir"; \
	echo "Imported $$count new corpus file(s)."

##@ Development

run: ## Run locally (starts MinIO backends via Docker, uses SQLite by default)
	docker compose -f $(COMPOSE_FILE) up -d --wait minio-1 minio-2 minio-3
	docker compose -f $(COMPOSE_FILE) run --rm minio-setup
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

##@ Testing

# -------------------------------------------------------------------------
# INTEGRATION TESTS
# -------------------------------------------------------------------------

COMPOSE_FILE := docker-compose.test.yml

integration-test: ## Run integration tests (testcontainers — no docker-compose needed)
	go test -race -v -tags integration -count=1 ./internal/integration/ ./internal/store/postgres/

dev-deps: ## Start dev environment services (MinIO + PostgreSQL + Redis + observability)
	docker compose -f $(COMPOSE_FILE) up -d --wait

dev-clean: ## Stop and remove dev environment containers
	docker compose -f $(COMPOSE_FILE) down -v

##@ Tools

# -------------------------------------------------------------------------
# TOOL INSTALLATION
# -------------------------------------------------------------------------

tools: ## Install build and packaging dependencies
	go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
	go install golang.org/x/perf/cmd/benchstat@latest
	sudo apt-get update && sudo apt-get install -y lintian
	curl -sfL https://raw.githubusercontent.com/aquasecurity/trivy/main/contrib/install.sh | sudo sh -s -- -b /usr/local/bin
	curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sudo sh -s -- -b /usr/local/bin

##@ Packaging and Release

# -------------------------------------------------------------------------
# DEBIAN PACKAGING
# -------------------------------------------------------------------------

prep-changelog: ## Compress changelog for Debian packaging
	@gzip -9 -n -c packaging/changelog > packaging/changelog.gz

deb: prep-changelog ## Build .deb packages via GoReleaser snapshot
	goreleaser release --snapshot --clean --skip=publish,sign

deb-lint: deb ## Run lintian on the .deb packages
	@for f in dist/*.deb; do echo "--- $$f ---"; lintian --tag-display-limit 0 "$$f"; done

# -------------------------------------------------------------------------
# APTLY PUBLISHING
# -------------------------------------------------------------------------

APTLY_URL             ?= $(or $(APTLY_ENDPOINT),https://apt.munchbox.cc)
APTLY_REPO            ?= $(or $(APTLY_REPOSITORY),munchbox)
APTLY_USER            ?= admin
APTLY_PUBLISH_PREFIX  ?= $(or $(APTLY_PREFIX),s3:munchbox:)
APTLY_DISTRIBUTION    ?= stable
APTLY_ARCHITECTURES   ?= amd64,arm64
DEB_DIR               ?= dist
SNAPSHOT_NAME         ?= $(IMAGE)-$(shell date +%Y%m%d-%H%M%S)

publish-deb: ## Publish .deb packages to Aptly repository
	@if [ -z "$(APTLY_PASS)" ]; then echo "Error: APTLY_PASS not set"; exit 1; fi
	@echo "Publishing packages to $(APTLY_URL)..."
	@for deb in $(DEB_DIR)/*.deb; do \
		echo "Uploading $$(basename $$deb)..."; \
		curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
			-X POST -F "file=@$$deb" \
			"$(APTLY_URL)/api/files/$(IMAGE)" || exit 1; \
	done
	@echo "Adding packages to repo $(APTLY_REPO)..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X POST "$(APTLY_URL)/api/repos/$(APTLY_REPO)/file/$(IMAGE)?forceReplace=1" || exit 1
	@echo "Creating snapshot $(SNAPSHOT_NAME)..."
	@curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-X POST -H 'Content-Type: application/json' \
		-d '{"Name":"$(SNAPSHOT_NAME)"}' \
		"$(APTLY_URL)/api/repos/$(APTLY_REPO)/snapshots" || exit 1
	@echo "Updating published repo at $(APTLY_PUBLISH_PREFIX) ($(APTLY_DISTRIBUTION))..."
	@body=$$(mktemp); \
	status=$$(curl -sS -u "$(APTLY_USER):$(APTLY_PASS)" \
		-o "$$body" -w '%{http_code}' \
		-X PUT -H 'Content-Type: application/json' \
		-d '{"Snapshots":[{"Component":"main","Name":"$(SNAPSHOT_NAME)"}],"ForceOverwrite":true}' \
		'$(APTLY_URL)/api/publish/$(APTLY_PUBLISH_PREFIX)/$(APTLY_DISTRIBUTION)'); \
	if [ "$$status" = "200" ]; then \
		echo "Updated existing publication."; \
		rm -f "$$body"; \
	elif [ "$$status" = "404" ]; then \
		echo "No publication at $(APTLY_PUBLISH_PREFIX)/$(APTLY_DISTRIBUTION); bootstrapping..."; \
		rm -f "$$body"; \
		archs=$$(echo '$(APTLY_ARCHITECTURES)' | sed 's/,/","/g'); \
		curl -fsS -u "$(APTLY_USER):$(APTLY_PASS)" \
			-X POST -H 'Content-Type: application/json' \
			-d "{\"SourceKind\":\"snapshot\",\"Sources\":[{\"Component\":\"main\",\"Name\":\"$(SNAPSHOT_NAME)\"}],\"Architectures\":[\"$$archs\"],\"Distribution\":\"$(APTLY_DISTRIBUTION)\"}" \
			'$(APTLY_URL)/api/publish/$(APTLY_PUBLISH_PREFIX)' || exit 1; \
		echo "Bootstrapped publication."; \
	else \
		echo "Publish update failed: HTTP $$status"; \
		echo "Server response:"; cat "$$body"; echo; \
		rm -f "$$body"; \
		exit 1; \
	fi
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
	goreleaser release --snapshot --clean --skip=sign

##@ Load Testing

# -------------------------------------------------------------------------
# LOAD TESTING
# -------------------------------------------------------------------------

LOADTEST_RATE        ?= 100
LOADTEST_DURATION    ?= 30s
LOADTEST_SIZE        ?= 1024
LOADTEST_SIZES       ?=
LOADTEST_SEED        ?= 100
LOADTEST_WORKERS     ?= 10
LOADTEST_ENDPOINT    ?= http://localhost:9000
LOADTEST_BUCKET      ?= photos
LOADTEST_OUTPUT_JSON ?=

# Sweep mode flag: -sizes wins when LOADTEST_SIZES is non-empty so a single
# `LOADTEST_SIZES=...` invocation switches into the per-size matrix run.
LOADTEST_SIZE_FLAG    = $(if $(LOADTEST_SIZES),-sizes $(LOADTEST_SIZES),-size $(LOADTEST_SIZE))
LOADTEST_OUTPUT_FLAG  = $(if $(LOADTEST_OUTPUT_JSON),-output-json $(LOADTEST_OUTPUT_JSON),)

loadtest-build: ## Build the vegeta load test binary
	cd loadtest && go build -buildvcs=false -o s3-loadtest .

loadtest-put: loadtest-build ## Run PUT-only load test (use LOADTEST_RATE, LOADTEST_DURATION, LOADTEST_SIZE or LOADTEST_SIZES)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op put -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		$(LOADTEST_SIZE_FLAG) -workers $(LOADTEST_WORKERS) $(LOADTEST_OUTPUT_FLAG)

loadtest-get: loadtest-build ## Run GET-only load test (use LOADTEST_SEED for pre-seeded object count)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op get -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		$(LOADTEST_SIZE_FLAG) -seed $(LOADTEST_SEED) -workers $(LOADTEST_WORKERS) $(LOADTEST_OUTPUT_FLAG)

loadtest-mixed: loadtest-build ## Run mixed PUT/GET load test
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op mixed -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		$(LOADTEST_SIZE_FLAG) -seed $(LOADTEST_SEED) -workers $(LOADTEST_WORKERS) $(LOADTEST_OUTPUT_FLAG)

LOADTEST_LIST_PREFIX   ?= loadtest/
LOADTEST_LIST_MAX_KEYS ?= 1000

loadtest-listobjects: loadtest-build ## Run ListObjectsV2 load test against a pre-seeded prefix (use LOADTEST_SEED for object count)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op listobjects -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		$(LOADTEST_SIZE_FLAG) -seed $(LOADTEST_SEED) -workers $(LOADTEST_WORKERS) \
		-list-prefix $(LOADTEST_LIST_PREFIX) -list-max-keys $(LOADTEST_LIST_MAX_KEYS) \
		$(LOADTEST_OUTPUT_FLAG)

loadtest-tagging: loadtest-build ## Run PutObjectTagging/GetObjectTagging/DeleteObjectTagging load test against a pre-seeded set (use LOADTEST_SEED)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op tagging -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		$(LOADTEST_SIZE_FLAG) -seed $(LOADTEST_SEED) -workers $(LOADTEST_WORKERS) $(LOADTEST_OUTPUT_FLAG)

loadtest-put-tagged: loadtest-build ## Run PUT with x-amz-tagging; diff against loadtest-put at the same rate and size for the inline-tagging cost
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op puttagged -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		$(LOADTEST_SIZE_FLAG) -workers $(LOADTEST_WORKERS) $(LOADTEST_OUTPUT_FLAG)

loadtest-cache: loadtest-build ## Run cache stress test (seeds more data than cache capacity to exercise eviction)
	./loadtest/s3-loadtest \
		-endpoint $(LOADTEST_ENDPOINT) -bucket $(LOADTEST_BUCKET) \
		-op mixed -rate $(LOADTEST_RATE) -duration $(LOADTEST_DURATION) \
		-size 262144 -seed 2000 -workers $(LOADTEST_WORKERS)

loadtest-burst: ## Run k6 burst/admission-control test (requires k6)
	@command -v k6 >/dev/null 2>&1 || { echo "Error: k6 is not installed. Install it from https://grafana.com/docs/k6/latest/set-up/install-k6/"; exit 1; }
	k6 run loadtest/k6/burst.js \
		--env S3_ENDPOINT=$(LOADTEST_ENDPOINT) --env S3_BUCKET=$(LOADTEST_BUCKET)

loadtest-burst-read: ## Run k6 read burst test (requires k6, use PEAK_VUS, SEED_COUNT, HOLD_DURATION)
	@command -v k6 >/dev/null 2>&1 || { echo "Error: k6 is not installed. Install it from https://grafana.com/docs/k6/latest/set-up/install-k6/"; exit 1; }
	k6 run loadtest/k6/burst-read.js \
		--env S3_ENDPOINT=$(LOADTEST_ENDPOINT) --env S3_BUCKET=$(LOADTEST_BUCKET)

PERF_PROFILE ?= smoke
# Admin token for the cache-flush (cold-read) scenario. Defaults to the local
# demo's token; set PERF_ADMIN_TOKEN=... when targeting another deployment.
# Passed inline so it wins over any stale S3O_ADMIN_TOKEN in the shell.
PERF_ADMIN_TOKEN ?= admin

perf: loadtest-build ## Run the full perf-envelope suite (PROFILE=smoke|baseline|saturation; PERF_ADMIN_TOKEN for cold-read flush)
	@S3O_ADMIN_TOKEN=$(PERF_ADMIN_TOKEN) ./loadtest/run-suite.sh $(PERF_PROFILE)

LOADTEST_MPU_CONCURRENCY ?= 10
LOADTEST_MPU_PART_COUNT  ?= 5
LOADTEST_MPU_PART_SIZE   ?= 5242880

loadtest-multipart: ## Run k6 concurrent multipart upload test (requires k6, use LOADTEST_MPU_*)
	@command -v k6 >/dev/null 2>&1 || { echo "Error: k6 is not installed. Install it from https://grafana.com/docs/k6/latest/set-up/install-k6/"; exit 1; }
	k6 run loadtest/k6/multipart.js \
		--env S3_ENDPOINT=$(LOADTEST_ENDPOINT) --env S3_BUCKET=$(LOADTEST_BUCKET) \
		--env CONCURRENCY=$(LOADTEST_MPU_CONCURRENCY) \
		--env PART_COUNT=$(LOADTEST_MPU_PART_COUNT) \
		--env PART_SIZE=$(LOADTEST_MPU_PART_SIZE)

loadtest-k6: ## Run k6 mixed CRUD workflow test (requires k6)
	@command -v k6 >/dev/null 2>&1 || { echo "Error: k6 is not installed. Install it from https://grafana.com/docs/k6/latest/set-up/install-k6/"; exit 1; }
	k6 run loadtest/k6/mixed.js \
		--env S3_ENDPOINT=$(LOADTEST_ENDPOINT) --env S3_BUCKET=$(LOADTEST_BUCKET)

##@ Deployment Demos

# -------------------------------------------------------------------------
# DEPLOYMENT DEMOS
# -------------------------------------------------------------------------

kubernetes-demo: ## Run the s3-orchestrator in k3d (requires docker, k3d, kubectl)
	./deploy/kubernetes/local/demo.sh

nomad-demo: ## Run the s3-orchestrator in Nomad dev mode (requires docker, nomad)
	./deploy/nomad/local/demo.sh

##@ Website

# -------------------------------------------------------------------------
# WEBSITE
# -------------------------------------------------------------------------

WEB_IMAGE  := $(REGISTRY)/s3-orchestrator-web
WEB_TAG    ?= $(VERSION)

# GODOC_EXCLUDES filters auto-discovered internal packages out of the godoc
# build. Anchored regex: each alternative matches a complete sub-path under
# internal/. Adjust here when adding a new test-only or generated package.
GODOC_EXCLUDES := ^(integration|testutil|testutil/.*|backend/backendtest|store/postgres/sqlc|store/postgres/migrations|.*/testdata|.*/testdata/.*)$$

web-tools: ## Install Hugo and gomarkdoc for local website development
	go install github.com/gohugoio/hugo@latest
	go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest

# web-godoc auto-discovers every internal/ Go package and generates a
# matching Hugo godoc page, avoiding the maintenance hazard of a hand-kept
# package list. The page filename is the package's basename; nested
# packages produce non-colliding bases (e.g. transport/admin -> admin.md,
# cli/adminctl -> adminctl.md).
web-godoc: ## Generate Go API reference markdown for the website
	@mkdir -p web/content/godoc
	@go list -f '{{.ImportPath}}' ./internal/... \
		| sed 's|^github.com/afreidah/s3-orchestrator/internal/||' \
		| grep -vE '$(GODOC_EXCLUDES)' \
		| sort \
		| while read pkg; do \
			name=$$(basename $$pkg); \
			echo "  godoc: internal/$$pkg -> $$name.md"; \
			printf -- '---\ntitle: "%s"\n---\n\n' "$$name" > web/content/godoc/$$name.md; \
			gomarkdoc ./internal/$$pkg >> web/content/godoc/$$name.md; \
			sed -i '/^# '"$$name"'$$/d' web/content/godoc/$$name.md; \
		done

web-submodules: ## Ensure Hugo theme submodule is checked out
	@git submodule update --init --recursive web/themes/hugo-theme-relearn

web-serve: web-submodules web-godoc ## Serve the project website locally
	cd web && hugo serve

web-build: web-submodules web-godoc ## Build the project website
	cd web && hugo --minify

web-docker: web-submodules ## Build website Docker image for local architecture
	docker build --pull -f web/Dockerfile -t $(WEB_IMAGE):$(WEB_TAG) .

web-push: web-submodules builder ## Build and push multi-arch website image to registry
	docker buildx build \
	  --pull \
	  --platform $(PLATFORMS) \
	  -f web/Dockerfile \
	  -t $(WEB_IMAGE):$(WEB_TAG) \
	  --output type=image,push=true \
	  .

##@ Cleanup

# -------------------------------------------------------------------------
# CLEANUP
# -------------------------------------------------------------------------

clean: ## Remove build artifacts, demo environments, containers, and volumes
	# --- Stop Nomad job and agent (localhost dev agent only; clear any prod
	#     NOMAD_*/CONSUL_* from the shell so this can never hit a real cluster) ---
	env -u NOMAD_TOKEN -u NOMAD_CACERT -u NOMAD_CLIENT_CERT -u NOMAD_CLIENT_KEY -u NOMAD_TLS_SERVER_NAME -u NOMAD_NAMESPACE -u NOMAD_REGION \
		-u CONSUL_HTTP_TOKEN -u CONSUL_CACERT -u CONSUL_CLIENT_CERT -u CONSUL_CLIENT_KEY -u CONSUL_TLS_SERVER_NAME \
		NOMAD_ADDR=http://127.0.0.1:4646 CONSUL_HTTP_ADDR=http://127.0.0.1:8500 \
		nomad job stop -purge s3-orchestrator 2>/dev/null || true
	pkill -f '[n]omad agent -dev' 2>/dev/null || true
	rm -f /tmp/nomad-demo.pid
	# --- Delete k3d cluster ---
	k3d cluster delete s3-orchestrator-demo 2>/dev/null || true
	# --- Tear down compose services and volumes ---
	docker compose -f $(COMPOSE_FILE) down -v --remove-orphans 2>/dev/null || true
	# --- Remove orphaned volumes from previous runs ---
	docker volume prune -f 2>/dev/null || true
	# --- SQLite dev database ---
	rm -f dev-data.db dev-data.db-shm dev-data.db-wal
	# --- Build artifacts ---
	go clean
	rm -f s3-orchestrator loadtest/s3-loadtest
	rm -f coverage.out integration-coverage.out
	rm -rf dist/ *.deb packaging/changelog.gz
	docker rmi $(FULL_TAG) 2>/dev/null || true
	docker rmi s3-orchestrator:local 2>/dev/null || true

.PHONY: openapi openapi-breaking help builder build install uninstall docker push generate test vet lint govulncheck coverage integration-coverage sonar-scan sonar-pr bench bench-compare run docs migration integration-test dev-deps dev-clean tools prep-changelog deb deb-lint publish-deb changelog release release-local loadtest-build loadtest-put loadtest-get loadtest-mixed loadtest-listobjects loadtest-multipart loadtest-burst loadtest-burst-read loadtest-k6 perf kubernetes-demo nomad-demo web-tools web-godoc web-submodules web-serve web-build web-docker web-push clean
.DEFAULT_GOAL := help
