# Changelog

All notable changes to this project are documented in this file.


## [0.33.0] - 2026-03-28

### Added
- add missing indexes, replace random() scrubber query, harden lifecycle (#435, #451, #453) (#463)
- add generic AtomicConfig[T] and TTLCache[K,V] to eliminate boilerplate (#457) (#459)
- add presigned URL support (#353) (#418)
- add read burst loadtest, cache Grafana panels, fix loadtest build (#417)
- add optional in-memory object data cache (#403) (#416)
- add decrypt-existing admin API, migrate integration tests to testcontainers (#408) (#409)

### Fixed
- fixup: adding benchmarks
- fix lint exclusion path, add concurrency cancel, harden supply chain (#454, #455) (#469)
- fix min_conns default, document new features, update metrics table (#466, #467) (#468)
- fixup: fix website badge

### Hardened
- security: fix signing key cache DoS, CSRF entropy, error enumeration, timer leak (#419, #431, #432, #445) (#464)
- security: verify vault token file permissions before reading (#428) (#461)

### Improved
- update CHANGELOG.md for v0.20.3 (#407)

### Dependencies
- chore(deps): bump codecov/codecov-action from 5 to 6 (#410)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/credentials (#411)
- chore(deps): bump github.com/jackc/pgx/v5 from 5.8.0 to 5.9.1 (#412)
- chore(deps): bump github.com/aws/aws-sdk-go-v2 from 1.41.4 to 1.41.5 (#413)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#414)
- chore(deps): bump github.com/hashicorp/vault/api from 1.22.0 to 1.23.0 (#415)

### Other
- reject encrypted reads during DB outage, fix vault renewal mutex (#429, #433) (#465)
- metrics shutdown, circuit breaker stale probe, load shedding threshold (#420, #424, #437) (#462)
- extract selectWriteTarget, add typed RoutingStrategy, rename shadowed vars (#452, #458) (#460)

## [0.20.3] - 2026-03-23

### Added
- add Helm chart, replace raw Kubernetes manifests (#382) (#406)
- add compatibility matrix and upgrade checklist (#379) (#397)
- add Getting Started section to README (#374) (#394)
- add rebalance pending gauge and encryption unknown-keyID counter (#316) (#327)
- add goleak goroutine leak detection and fix flaky timing tests (#315) (#324)
- add unit tests for chunk encryption, location cache, aggregator, and cleanup queue (#312) (#323)

### Fixed
- fixup: force new version of logo everywhere with version tag
- fixup: bust cache on logo so cloudflare doesn't serve the old one, update image push tasks in makefile

### Hardened
- security: limit active multipart uploads per bucket (#369) (#402)
- security: add two-phase confirmation for remove-backend --purge (#368) (#401)
- security: add CSRF token protection for UI state-changing operations (#371) (#399)
- security: document nonce derivation safety invariant (#372) (#396)
- security: separate metrics listener and strip instance ID from health (#370) (#395)

### Improved
- Update README.md (#400)
- update documentation for v0.19.x changes (#328)
- update CHANGELOG.md for v0.19.1 (#307)

### Documentation
- document docker-compose volume cleanup and add troubleshooting (#375) (#393)
- document database connection pool sizing for production (#351) (#391)

### Other
- logging tweak and version bump
- object integrity verification with SHA-256 content hashing (#404)
- enhancement: add schema version validation at startup, update logo (#387) (#398)
- report missing part numbers in CompleteMultipartUpload error (#384) (#390)
- recommend trace sample rate for production deployments (#355) (#389)
- warn when replication.factor=1 with multiple backends (#348) (#388)
- added new benchmark test after a number of changes
- perf: pipeline Redis Expire calls with INCRBY operations (#336) (#347)
- perf: use string slicing instead of TrimPrefix in list responses (#338) (#346)
- perf: use map lookup for encrypted object locations in GetObject and HeadObject (#337) (#345)
- perf: replace fmt.Sprintf with string concat for multipart part keys (#335) (#344)
- perf: parse SigV4 auth header once and reduce encoding allocations (#333, #334) (#343)
- perf: combine backend filtering into single pass on write path (#332) (#342)
- perf: fetch quota stats once per replication cycle (#341)
- perf: reuse chunk buffers and nonce in encrypt/decrypt readers (#340)
- cleanup: remove unused InFallbackMode export, cancel pipe on PutObject failure (#318) (#329)
- validate encryption chunk size, master_key_file, and workerpool concurrency at startup (#314) (#326)
- filter multipart uploads by backend in drain, fix UI JSON content-type (#325)
- cancel losing goroutine contexts in parallel broadcast read (#322)
- make Close() idempotent on RedisCounterBackend, RateLimiter, and LoginThrottle (#310) (#321)
- remove implicit stripPort from LoginThrottle, require caller-resolved IPs (#309) (#320)
- close timing side-channels in UI login and admin token auth (#308) (#319)

## [0.19.1] - 2026-03-20

### Improved
- updates to meet style guide
- update CHANGELOG.md for v0.19.0 (#306)

## [0.19.0] - 2026-03-20

### Improved
- update CHANGELOG.md for v0.18.3 (#296)

### Dependencies
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#297)
- chore(deps): bump github.com/aws/aws-sdk-go-v2 from 1.41.3 to 1.41.4 (#298)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/credentials (#299)

### Other
- restructure storage/ into breaker/, backend/, store/, counter/, proxy/, worker/ (#304) (#305)
- invert audit→telemetry dependency and narrow MetadataStore interfaces (#301) (#303)
- split config validation into domain files and extract server middleware (#300) (#302)

## [0.18.3] - 2026-03-20

### Added
- add orphan reconciler and update documentation for v0.18.x changes (#289) (#295)
- add replication internals guide and fix encryption claims in architecture diagram (#284)
- Add container tuning, GOMEMLIMIT, and default max_concurrent_requests (#255) (#282)

### Fixed
- fix multi-instance usage accounting and shared admission control (#289) (#294)
- fix log-trace correlation, add span kinds, rename s3proxy telemetry namespace (#286) (#288)

### Hardened
- security: add Vault token renewal, fix SigV4 timing leak, warn on cert/key expiry (#290) (#292)

### Improved
- update CHANGELOG.md for v0.17.9 (#274)

### Dependencies
- chore(deps): bump google.golang.org/grpc (#287)
- chore(deps): bump golang.org/x/time from 0.14.0 to 0.15.0 (#279)
- chore(deps): bump golangci/golangci-lint-action from 7 to 9 (#275)
- chore(deps): bump dorny/paths-filter from 3 to 4 (#276)
- chore(deps): bump go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc (#277)
- chore(deps): bump go.opentelemetry.io/otel/sdk from 1.40.0 to 1.42.0 (#278)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#280)
- chore(deps): bump golang.org/x/crypto from 0.48.0 to 0.49.0 (#281)

### Other
- resilience: fix timeout cascading, bound connection pools, add jitter, reorder readiness (#291) (#293)
- have sitemap setup (#285)

## [0.17.9] - 2026-03-13

### Added
- Add interactive Mermaid.js architecture and flow diagrams to documentation site (#273)

### Improved
- update CHANGELOG.md for v0.17.8 (#272)

## [0.17.8] - 2026-03-12

### Fixed
- Fix circuit breaker deadlock when all backends trip simultaneously (#271)

### Improved
- update CHANGELOG.md for v0.17.5 (#264)

### Dependencies
- chore(deps): bump golang.org/x/net (#267)

### Other
- Reduce per-request CPU and allocation overhead in hot path (#269)

## [0.17.5] - 2026-03-12

### Added
- Add over-replication detection and cleanup with integration tests (#260) (#263)
- Add burst resilience: Retry-After, early rejection, split admission, load shedding, admission wait (#262)

### Fixed
- Fix: updated nomad/kubernetes demos to protect the nomad and kubectl commands to protect if the user has en environment already pointing at real clusters
- Fix: accidentally committed benchmark test results
- Fix: pre-allocate headerlines slice (#259)

### Improved
- update CHANGELOG.md for v0.17.01 (#256)

### Other
- adding documentation for previous graceful degradation branch
- Tune HTTP transport, DNS resolution, and buffer pooling for backend clients (#254) (#261)

## [0.17.01] - 2026-03-10

### Added
- Add generic worker pool and parallelize sequential hot paths (#253)

### Improved
- update CHANGELOG.md for v0.17.00 (#252)

## [0.17.00] - 2026-03-10

### Added
- Add orphan_bytes tracking to prevent quota drift on failed deletes (#250)

### Improved
- update CHANGELOG.md for v0.16.25 (#248)

## [0.16.25] - 2026-03-10

### Added
- Add Tempo, Loki, and Alloy integration for Nomad and Kubernetes demos (#247)

### Fixed
- Fix - expand free tier page with images and more detailed content

### Improved
- update CHANGELOG.md for v0.16.23 (#245)

## [0.16.23] - 2026-03-09

### Fixed
- Fix: The PAT fix ensures CI triggers on the bot's PR. The CODEOWNERS exemption prevents the review request from cluttering it. Together, the release flow should be: tag push → GoReleaser → changelog PR created → CI runs → checks pass → (#244)

## [0.16.22] - 2026-03-09

### Added
- Add trace-to-log correlation via slog TraceHandler (#242)

## [0.16.21] - 2026-03-09

### Other
- Test: bumping version to test release functionality

## [0.16.20] - 2026-03-09

### Fixed
- Fix: let release pipeline create a PR at the end and auto-merge it to have the changelog update (#238)

## [0.16.19] - 2026-03-09

### Fixed
- Fix: re-order pre-changelog, goreleaser, and git-cliff (#237)

## [0.16.18] - 2026-03-09

### Fixed
- Fix: the changelog step was breaking goreleaser on 'make release' (#236)

## [0.16.17] - 2026-03-09

### Other
- write failover for PutObject across eligible backends (#234)

## [0.16.15] - 2026-03-09

### Added
- add strip_sdk_headers option for GCS S3 compatibility (#229)
- add per-backend disable_checksum option for GCS compatibility (#225)
- Add DB query tracing, background worker spans, audit logging gaps, and Grafana dashboard coverage (#222)
- Add git-cliff changelog generation with commit categorization (#219)

### Fixed
- Fix: when a backend is unhealthy, routing still needs to allow half-open through so probes can heal the backend (#227)

### Hardened
- Harden defaults: increase DB pool size and add location cache TTL jitter (#215)

### Documentation
- documentation update I forgot

### Other
- exclude circuit-broken backends from write routing (#226)

## [0.16.4] - 2026-03-08

### Added
- Add file download to admin web UI dashboard (#195)
- add website to readme (#193)

### Fixed
- Fix cfg data race in SIGHUP handler by wrapping with atomic.Pointer[config.Config] (#210)
- Fix ListObjectsV2 pagination bug — when the store returned exactly maxKeys objects with more data available, the manager never set IsTruncated=true, so clients like aws s3 cp --recursive stopped after the first 1000 keys. Added (#200)
- Fix ListObjectsV2 pagination dropping results after exactly maxKeys entries (#198)
- forgot to add log message to new admin ui download functionality (#196)

### Hardened
- Security hardening and error handling consistency (#209)

### Refactored
- Decompose BackendManager into focused component structs (#208)
- Refactoring release that reduces code duplication without changing behavior. Admin API handlers now use Go 1.22+ method routing instead of manual method checks. A streamCopy helper consolidates the repeated (#204)

### Improved
- Expand fuzz testing with new targets, fix CI flakiness, and add file headers (#213)

### Other
- Addbenchmarksuiteforhot-pathoperationsandbenchmarkingdocs (#211)
- if replication factor is 1 (no replication) it should short-circuit before Replicate() is called since it will just find no available targets and log an error. Closes #191 (#192)

## [0.14.4] - 2026-03-06

### Added
- Add optional server-side Redis shared counters for multi-instance usage tracking (#170) (#186)
- add readme to available documentation on hugo website (#183) (#185)
- add readme to available documentation on hugo website (#183)
- Add health-aware replication: replace copies on circuit-broken backends (#171) (#172)

### Improved
- Update lint in ci to be faster (#190)
- update alpine version (#188)

### Dependencies
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#181)
- chore(deps): bump go.opentelemetry.io/otel from 1.40.0 to 1.41.0 (#180)
- chore(deps): bump go.opentelemetry.io/otel/trace from 1.40.0 to 1.41.0 (#179)
- chore(deps): bump github.com/aws/smithy-go from 1.24.1 to 1.24.2 (#178)
- chore(deps): bump github.com/aws/aws-sdk-go-v2 from 1.41.2 to 1.41.3 (#177)
- chore(deps): bump docker/setup-buildx-action from 3 to 4 (#173)
- chore(deps): bump actions/github-script from 7 to 8 (#174)
- chore(deps): bump docker/build-push-action from 6 to 7 (#175)
- chore(deps): bump docker/login-action from 3 to 4 (#176)

### Other
- use lint image instead of installing it from scratch every time which takes forever (#189)
- always pull latest version of docker iamges (#187)
- Triggers on push to main (same as the app image) (#182)

## [0.13.0] - 2026-03-05

### Added
- Add project website, fix usage tracking gaps, and add Vault Transit TLS support (#168) (#169)
- Add optional server-side encryption with envelope encryption and chun… (#167)
- Add per-backend circuit breakers, drain fixes, and production hardening (#161) (#162)
- Add metadata passthrough, govulncheck CI, and production hardening (#159) (#160)

### Hardened
- Harden auth, add request body limits, and fix dashboard CB panel (#164) (#165)
- Harden dashboard JS against XSS and add log pagination (#155) (#157)
- Harden auth and sanitize API error messages (#156)

### Documentation
- documentation tweaks

### Other
- README.md update on double encryption

## [0.11.2] - 2026-03-03

### Added
- Add GitHub community files and repo configuration for open-source readiness (#153)
- Add folder delete from web UI with batch prefix deletion (#151)
- Add Prometheus and Grafana to local demo scripts (#148) (#149)
- Add backend drain and remove operations (#146) (#147)
- Add in-memory log ring buffer with dashboard UI (#143)
- Add server-level admission control for concurrent request limiting (#141)

### Refactored
- Refactor internal code for clarity and reduced duplication (#145)

### Other
- if go or sqlc code changes and the version hasn't been bumped reject the PR

## [0.8.28] - 2026-03-02

### Added
- Add explicit permissions to CI workflow jobs (#136) (#139)
- Add benchmarks, fuzz tests, and e2e tests (#126) (#134)
- add contributing, DR, security, performance, API, and migration guides (#124) (#132)

### Fixed
- Fix unsafe integer conversion in RecordPart (#135) (#137)

### Other
- support bcrypt-hashed admin_secret and deterministic session keys (#133)

## [0.8.23] - 2026-03-02

### Added
- add admin CLI, runtime log level control, and fix Trivy CI (#123) (#131)
- add --mode flag for api/worker/all instance roles (#121) (#129) (#130)
- add --mode flag for api/worker/all instance roles (#121) (#129)
- add readiness probe, JSON health responses, and pre-stop drain (#120) (#128)
- Add configurable HTTP server timeouts and ReadHeaderTimeout (#127)
- add LIKE ESCAPE clause and quota guard to SQL queries (#109)

### Fixed
- Fix code correctness bugs in SQL, auth, XML responses, and concurrency (#114)
- prevent cleanupBackoff overflow on large attempt values (#112)

### Hardened
- Harden security for error messages, config validation, and map access (#115)
- harden SigV4 and token authentication (#107)

### Improved
- Improve packaging, deployment, and build consolidation (#117)
- Improve CI/CD pipeline with sqlc verification, release gates, and scanning (#116)
- replace destructive down migration with no-op (#113)

### Other
- code cleanup and consistency improvements (#81, #84, #87, #93, #94, #96, #97, #101, #102) (#118)
- use detached context for advisory lock unlock (#111)
- enforce MaxObjectSize on multipart uploads and fix ListObjects pagination (#110)
- correct usage tracking for multipart and failed operations (#108)
- defer read context cancellation until body is consumed (#106)
- validate client-supplied X-Request-Id to prevent log injection (#105)
- Clarify storage backend quota enforcement details

## [0.8.6] - 2026-03-01

### Other
- a few integration test additions

## [0.8.5] - 2026-03-01

### Added
- Add ListObjectsV1 and ListMultipartUploads endpoints (#60)
- add HeadBucket, GetBucketLocation, and ListBuckets stubs (#50) (#59)
- add Nomad and Kubernetes deployment examples with local demo scripts (#58)
- add structured circuit breaker transition logging (#57)
- add table of contents to README.md
- add validate and version subcommands, improve developer quickstart (#56)

### Other
- adopt goose for versioned database migrations (#61)

## [0.8.0] - 2026-02-28

### Added
- add dashboard auth, file management, rebalance and sync to web UI (#49)
- add comprehensive Grafana dashboard covering all emitted metrics (#48)
- add screenshot of dashboard to readme

## [0.7.2] - 2026-02-28

### Improved
- updates to local image pushing to be faster, dockerfile/build tweaks,… (#47)
- update tests for better coverage (#43)
- updated version to be set in .version and docs to use x.x.x

### Dependencies
- chore(deps): bump actions/checkout from 4 to 6 (#32)
- chore(deps): bump goreleaser/goreleaser-action from 6 to 7 (#33)
- chore(deps): bump actions/setup-go from 5 to 6 (#34)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#36)
- chore(deps): bump go.opentelemetry.io/otel/sdk from 1.32.0 to 1.40.0 (#38)
- chore(deps): bump github.com/aws/aws-sdk-go-v2 from 1.32.7 to 1.41.2 (#35)
- chore(deps): bump go.opentelemetry.io/otel/trace from 1.32.0 to 1.40.0 (#39)
- chore(deps): bump go.opentelemetry.io/otel from 1.32.0 to 1.40.0

### Other
- push docker image to ghcr on merge to main (#45)

## [0.7.0] - 2026-02-27

### Other
- make documents compatible with the style guide and all go code godoc compliant

## [0.6.4] - 2026-02-27

### Added
- add token usage for codecov
- add GoReleaser releases, Codecov, Dependabot, and CI badges
- add lifecycle rules for automatic object expiration
- add advisory locks and adaptive usage flushing for multi-instance safety
- add S3 DeleteObjects batch API
- add persistent retry queue for failed backend cleanup deletions
- add TLS/mTLS support with certificate hot-reload
- add Debian packaging with nfpm and systemd service
- add Debian packaging with nfpm and systemd service
- add structured audit logging with request ID tracing
- add service lifecycle manager with panic recovery and auto-restart
- add storage summary section to dashboard, bump to v0.5.1
- add lazy-loaded directory tree, tests, and v0.5.0 docs
- add SIGHUP config hot-reload with tests and documentation
- add spread routing tests, update docs, fix godoc compliance
- add spread write routing strategy and dashboard favicon
- add web UI documentation, fix OTel service name
- add object listing to dashboard and fix table alignment
- add operator/admin guide for deploying and operating the orchestrator
- add per-backend monthly usage limit enforcement
- add per-backend API request and data transfer tracking
- add GitHub Actions workflow and linter config
- add auth, sync pipeline, and store-level integration tests
- add comprehensive unit tests for manager business logic and fix integration tests
- add database circuit breaker with self-healing degraded mode

### Hardened
- harden security, correctness, and observability for v0.5.2

### Refactored
- rename s3-proxy to s3-orchestrator
- rename Go module to github.com/afreidah/s3-proxy

### Improved
- replace NewBackendManager positional params with config struct
- replace goto with structured control flow in ListObjects
- update README and config example to reflect current features

### Documentation
- document usage limits, new metrics, and usage_deltas table

### Other
- optional parallel broadcast reads in degraded mode
- parallel rebalance move execution
- updating docs for .deb packaging info
- extract concerns from BackendManager and harden circuit breaker
- disable unsigned payload over plain HTTP, use Swap middleware
- stream uploads with unsigned payload, skip full-body buffering
- reduce circuit breaker boilerplate with generic helpers, fix hugeParam lint
- interactive collapsible object tree in dashboard
- cache-bust CSS, fix double-v version, simplify table layout
- table alignment with fixed layout and explicit column widths
- check json.Encode error return to satisfy errcheck lint
- built-in web UI dashboard for operational visibility
- production hardening across 6 areas
- multi-bucket support with per-bucket SigV4 credentials
- use golangci-lint v2 via go run to match config format
- validate quota and replication combinations to prevent nonsensical configs
- make quota_bytes optional — 0 or omitted means unlimited
- highlight multi-cloud replication in project description
- remove munchbox-specific references, improve project description
- remove munchbox-specific references, improve project description
- GetObject result struct, fix goroutine leaks, add cache eviction and race detector
- multipart quota reservation, ListObjects delimiter pagination, backend timeouts
- correct 4 error handling bugs in manager and multipart handlers
- circuit breaker returns ErrDBUnavailable on probe failure, fix all errcheck lint
- split manager, add structured S3 errors, unify handler routing
- production-harden s3-proxy with security, correctness, and code quality improvements
- reorganize s3-proxy into cmd + internal package structure
