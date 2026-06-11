# Changelog

All notable changes to this project are documented in this file.


## [0.62.1] - 2026-06-11

### Fixed
- fix(ci,deps): patch webpage base image CVEs + bump pgx to v5.10.0 (#1027)

### Other
- GH_ISSUE_1025: human-readable CLI output + per-item progress streaming (#1026)
- GH_ISSUE_1023: bound + rate-limit backfill-checksums; add make install (#1024)

## [0.61.1] - 2026-06-08

### Other
- GH_ISSUE_1021: target a remote instance via admin flags/env, no full config (#1022)

## [0.61.0] - 2026-06-08

### Improved
- update CHANGELOG.md for v0.60.40 (#1001)

### Dependencies
- chore(deps): bump the aws-sdk group with 4 updates (#1016)

### Other
- docs(web): reflect usage reconciliation in godoc + background-services diagram (#1020)
- GH_ISSUE_1018: add usage reconciliation to correct bytes_used drift (#1019)
- GH_ISSUE_1011: ride out transient MinIO 502s in concurrent put/delete test (#1015)
- GH_ISSUE_1002: route over-replication cleaner through the TxAdapter lock seam (#1010)

## [0.60.40] - 2026-06-02

### Added
- add s3o_list_pages_capped_total counter (#764)

### Fixed
- fixup: web godoc updates
- fixup: added updated web godoc
- fixup: added web godocs that were missing
- fixup: adjust nomad-demo resource allocations
- fix(proxy): materialize CopyObject source to a seekable body (#816)
- fix(s3api): include ETag and StorageClass in ListObjects responses (#814)
- fix(sqlite): route tx.Commit failure through breaker PostCheck (#775) (#786)

### Refactored
- refactor(proxy): extract read-failover orchestration into internal/proxy/readpath (#849) (#855)
- refactor(proxy): decompose internal/proxy into focused subpackages (#841) (#845)
- refactor(proxy): consolidate proxy package files (22 → 10) (#844)
- refactor(di): centralize worker provider dependency resolution (#839)
- refactor(proxy): drop worker fields from BackendManager and validate constructor inputs (#838)
- refactor(worker): extract replicator planner and outcome struct (#811)
- refactor(logging): handler-enforced error rendering + component scoping (#809)
- refactor(di): explicit Optional[T] resolution distinguishing missing vs failed providers (#807)
- refactor(reload): generation-aware coordinator with structured outcomes (#790) (#802)
- refactor(backend): typed CopyError for stream-copy phase classification (#793) (#801)
- refactor(worker): drop dead quotaStats from replication path (#792) (#800)
- refactor(cli/serve): decompose god object into runtime/httpserver/reload (#788) (#789)
- refactor(store): use mapping helpers for nullable fields (#774) (#787)
- refactor(ioutilx): extract read-closer helpers from proxy (#776) (#785)
- refactor(observe): unify span error recording across managers (#773) (#784)
- refactor(transport): extract shared HTTP JSON helpers (#772) (#783)
- refactor(ui): split handler.go into focused transport modules (#771) (#782)
- refactor(di): explicit manager wiring via di.WireManager (#777) (#781)
- refactor(proxy): split multipart.go into focused files (#770) (#780)
- refactor(proxy): remove parent back-pointers from ObjectManager and MultipartManager (#769) (#779)
- refactor(di): split di.go into focused provider files (#768) (#778)

### Improved
- update CHANGELOG.md for v0.46.20 (#762)

### Dependencies
- chore(deps): bump the aws-sdk group across 1 directory with 4 updates (#954)
- chore(deps): bump the actions group with 2 updates (#953)
- chore(deps): bump the otel group across 1 directory with 4 updates (#955)
- chore(deps): bump the minor-and-patch group with 3 updates (#956)
- chore(deps): bump the actions group with 4 updates (#932)
- chore(deps): bump the minor-and-patch group with 2 updates (#931)
- chore(deps): bump the minor-and-patch group with 2 updates (#837)

### Other
- GH_ISSUE_974: split NewInjector into grouped helpers + typed config.Mode (#1000)
- Revise README for clarity and project comparisons (#999)
- GH_ISSUE_973: drop issue refs and lint-threshold rationale from source comments (#998)
- GH_ISSUE_982: split monolithic README + admin-guide into front door + per-topic docs (#996)
- GH_ISSUE_989: group BackendManagerConfig into capability sub-structs (#995)
- GH_ISSUE_987: encapsulate BackendManager sub-manager fields behind accessors (#994)
- GH_ISSUE_985: split ObjectCore into ObjectWriteCore / ObjectReadCore; drop transitive BackendOrder (#993)
- GH_ISSUE_986: split ObjectCoordinator into WriteRouter / PendingWriter / CleanupWriter (#992)
- GH_ISSUE_984: narrow consumer store interfaces; MetadataStore is now composition-root-only (#991)
- GH_ISSUE_964: fix fuzz workflow filing false-positive crash issues (#967)
- GH_ISSUE_950: wire runtime/trace.FlightRecorder into admin snapshot endpoint (#966)
- GH_ISSUE_949: migrate eligible tests to testing/synctest (Go 1.25) (#965)
- GH_ISSUE_962: bump golangci-lint to v2.12.2 + fix surfaced findings (#963)
- GH_ISSUE_944: adopt errors.AsType[T] (Go 1.26) (#961)
- GH_ISSUE_946: adopt sync.WaitGroup.Go (Go 1.25) (#960)
- GH_ISSUE_945: use net.JoinHostPort instead of fmt.Sprintf("%s:%d") (#959)
- GH_ISSUE_952: go fix ./... modernizer sweep + govulncheck bump (#958)
- GH_ISSUE_948: log shutdown cause from signal.NotifyContext (Go 1.26) (#957)
- GH_ISSUE_834: preserve raw benchmark output alongside filtered summary (#943)
- GH_ISSUE_836: per-backend credential_source for AWS default chain (#942)
- GH_ISSUE_803: advisory-lock + replicator + reaper invariant tests (#941)
- GH_ISSUE_803: cleanup queue concurrent-worker integration test (#940)
- GH_ISSUE_804 (PR-B): DB-recovery cache invalidation, mixed-outcome counter, Range test (#939)
- GH_ISSUE_804: degraded-mode observability + operator opt-out (#938)
- GH_ISSUE_927: hoist per-cycle telemetry out of business logic (#937)
- GH_ISSUE_919: drop narrative comments and linter-shaped helper DTOs (#936)
- GH_ISSUE_918: collapse single-impl, single-consumer interface seams (#935)
- GH_ISSUE_920: standardize constructor dependency validation (#933)
- GH_ISSUE_925: relocate worker lifecycle services beside workers (#930)
- GH_ISSUE_924: extract shared object-move primitive (drain + rebalancer) (#929)
- Three-issue bundle: read-path split (#923) + rebalancer copy-map + accounting (#928)
- GH_ISSUE_916: populate content_hash on multipart completion (#926)
- GH_ISSUE_405: dashboard integrity polish + brand palette (#915)
- GH_ISSUE_883: guard DrainActive.Dec() with sync.Once (#914)
- GH_ISSUE_831: drop context.Background() from DI startup logs (#913)
- GH_ISSUE_906: split admin/handler.go by domain (#912)
- GH_ISSUE_902: split objects_write.go by operation type (#911)
- GH_ISSUE_903: extract finalize helpers for materialized COPY + DELETE (#910)
- GH_ISSUE_905: name batch-delete concurrency constant (#909)
- GH_ISSUE_901: collapse cache invalidation into one helper (#908)
- GH_ISSUE_904: clone caller's slice before sorting in CopyToReplica (#907)
- GH_ISSUE_858: cap parallel degraded-broadcast fanout (#900)
- GH_ISSUE_881: stop double-counting backend DELETE API calls (#899)
- GH_ISSUE_884: HEAD-probe ambiguous native-copy failures before falling back (#898)
- GH_ISSUE_835: document admissionSemFor concurrency model + pin wiring with tests (#897)
- GH_ISSUE_880: skip cleanup enqueue when RecoverFromRecordFailure DELETE 404s (#896)
- GH_ISSUE_798: HTTP panic-recovery middleware with structured logging, audit, and metric (#895)
- GH_ISSUE_830: make PendingReaper registration explicit, drop nil,nil semantics (#894)
- GH_ISSUE_882: apply backend_timeout consistently to COPY + multipart complete (#893)
- GH_ISSUE_833: align loadtest go.mod with root + CI drift check (#892)
- GH_ISSUE_861: replace semaphore-per-item workerpool with fixed worker model (#891)
- FIX: add new benchmark file
- GH_ISSUE_889: Helm chart support for dedicated metrics listener + opt-in pprof (#890)
- GH_ISSUE_885: pool chunkBuffers + seal/open into pre-allocated dst (#888)
- GH_ISSUE_886: mount pprof only on dedicated metrics listener (#887)
- GH_ISSUE_878: docs audit sync — bring every doc in line with current code (#879)
- GH_ISSUE_843: treat backend DELETE 404 as idempotent success (#877)
- GH_ISSUE_653: close drain-race + fix DrainActive metric + bound purge loop (#876)
- GH_ISSUE_871: make notify mockOutboxStore thread-safe + parallelize 23 tests (#875)
- GH_ISSUE_860: swap UsageTracker RWMutex pair for atomic.Pointer snapshots (#874)
- GH_ISSUE_848: trim repetitive comments in consumer-interface + observability files (#873)
- GH_ISSUE_850: decompose infra.Core into capability-oriented services (#872)
- GH_ISSUE_854: replace http.DefaultClient with ts.Client() in s3api tests + parallelize adminctl (#870)
- GH_ISSUE_856: optimize PutObject buffering and integrity pipeline (#869)
- GH_ISSUE_859: add same-backend server-side copy fast path (#868)
- GH_ISSUE_857: cancel losing degraded-read probes after a winner is declared (#867)
- GH_ISSUE_853: centralize per-operation completion observability (#866)
- GH_ISSUE_864: guard web targets against missing Hugo theme submodule (#865)
- FIXUP: add web godoc updates
- centralize per-backend usage + operation accounting via internal/proxy/accounting (#851) (#863)
- consumer-declared interfaces for proxy subpackages + lifecycle runners + reload hooks (#846) (#847)
- docs(logging): make error-attribute rule explicit (#840)
- surface orphan-enqueue failures during DB outages (#824)
- test(proxy): drop BenchmarkCopyMaterializeSink cognitive complexity (#822)
- ci(bench): nightly benchmark trend tracking via github-action-benchmark (#821)
- test(proxy): benchmark CopyObject materialize sink at both branches (#819)
- ci(fuzz): gate auto-filed bug issues on actual crashing input (#818)
- surface background worker health via admin API and Prometheus (#817)
- test(di): pin advisory-lock, interval, and mode-matrix invariants for periodic services (#812)
- docs(store): mechanical-checks contributing page + depguard boundary rule (#810)
- remove multipart DEK backfill worker (#766) (#767)
- perf envelope tooling + cache-flush admin API (#367) (#765)

## [0.46.20] - 2026-05-10

### Fixed
- fix(proxy,release): cache streaming admission + cosign bundle filename (#761)

## [0.46.19] - 2026-05-10

### Added
- add package-level doc comments to every Go package (#697)
- add FailableBackend, sentinel config errors, and edge-case integration scenarios (#591)

### Fixed
- fix(release): switch cosign signing to --bundle (#756) (#757)
- fix(postgres): keep backend_quotas.bytes_used in step with encrypt/decrypt rewrites (#742) (#743)
- fix(breaker): clean Open->Closed recovery via new Recover() method (#739) (#741)
- fix(auth): SigV4 verifier honours wire-form path encoding (#737) (#738)
- fix(s3api): scope multipart endpoints to URL bucket to close cross-bucket IDOR (#735) (#736)
- fix(cleanup): per-row claim pattern eliminates double-processing race (#733) (#734)
- fix(ui/logs): stringify error attrs in ring buffer + click-to-expand rows (#720)
- fix(proxy/multipart): per-uploadID advisory lock + cleanup on failure (#715)
- fix(store): apply backend_quotas deltas in stable order to prevent deadlock (#687) (#688)
- fix(replicator): consistent size between row and quota; pass actual source size (#652) (#686)
- fix(proxy): single-tx batch DeleteObjects (#677)
- fix(rebalancer): batch backend lookup per source instead of per object (#675)
- fix(proxy): advance ListObjects continuation token past emitted CommonPrefix (#672)
- fix(proxy): release per-call timeout on broadcast-read winner (#671)
- fix(test): TestCircuitBreaker_DegradedDurationIsPositive flake (#670)
- fix(test): TestCircuitBreaker_DegradedDurationIsPositive flake
- fix(store/sqlite): clear S2077 hotspots via json_each IN expansion (#644)
- fix(proxy): paginate ReconcileBackend with bounded-memory sorted-merge (closes #614) (#642)
- fix(ui): use String.replaceAll() to trim slashes in upload path (#638)
- fix(ui): make cookie Secure flag follow trusted-proxy X-Forwarded-Proto (#635)
- fix(docs): use <br> for Mermaid line breaks in diagrams (#625)
- fix(test): serialize TransitionLogs_HalfOpenToClosed to prevent captureLogs race (#603)

### Hardened
- security: validate streaming SigV4 chunk signatures end-to-end (#730)

### Refactored
- refactor(proxy): cleanup-DELETE accounting + read-path location plumbing (#758) (#759)
- refactor(di): drop redundant adapters, bag structs, side-effect registration (#753)
- refactor(store): collapse narrow store-role interfaces (#747) (#751)
- refactor(store): move CB into driver-level DBTX wrapper, delete decorator layer (#750)
- refactor(test): consolidate three handwritten mockStore implementations onto mockgen (#749)
- refactor(observability): standardize structured logging conventions (#718)
- refactor(lifecycle): rename Service/Stoppable to Runner/Stopper (#710)
- refactor(integration): drop S3776 cognitive complexity in test fixtures (#699)
- refactor(proxy): lift workers out of BackendManager (#676 B) (#685)
- refactor(proxy): slim backendCore (#676 C) (#684)
- refactor(proxy): extract metrics, drain, dashboard subpackages (676D partial) (#682)
- refactor(store): drop alias layer + split AdminStore into narrow roles (#681)
- refactor(store): drop postgres re-exports (676A) (#680)
- refactor(store): extract engine-agnostic core, thin per-engine adapters (#674)
- refactor(breaker, telemetry): decouple breaker from observability; split metrics (#640)
- refactor(store): collapse 11-case toObjectLocation switch via accessors (#639)
- refactor(transport): handlers depend on narrow Deps, not *BackendManager (closes #613) (#636)
- refactor(s3api): extract enforceContentLength helper (closes #632) (#633)
- refactor(cmd): thin cmd/ via internal/cli + breaker.Registry; atomic SIGHUP (#630)
- refactor(observe): collapse span+metrics+status boilerplate (#621)
- refactor(store): retire MetadataStore union, narrow roles everywhere (#617)
- decompose MetadataStore into narrow per-worker store interfaces (#566) (#579)

### Improved
- replace do.MustInvoke with explicit error handling in DI resolution (#564) (#589)
- update CHANGELOG.md for v0.40.1 (#563)

### Dependencies
- chore(deps): bump the actions group with 2 updates (#726)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#727)
- chore(deps): bump github.com/redis/go-redis/v9 (#668)
- chore(deps): bump the aws-sdk group with 3 updates (#667)
- chore(deps): bump SonarSource/sonarqube-scan-action in the actions group (#666)
- chore(deps): bump the minor-and-patch group across 1 directory with 3 updates (#624)
- chore(deps): bump the aws-sdk group with 3 updates (#619)
- chore(deps): bump the actions group with 4 updates (#618)
- chore(deps): bump github.com/jackc/pgx/v5 from 5.9.1 to 5.9.2 (#596)
- chore(deps): bump the minor-and-patch group across 1 directory with 2 updates (#576)
- chore(deps): bump go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc (#573)
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#572)

### Other
- FIX: new benchmark and tweaks to nomad resources to use less cpu since performance improved greatly on cpu
- cross-cutting cleanup (#754) (#755)
- delete verified-dead helpers and methods (#748)
- bug: encryption stream readers no longer translate IO errors to EOF (#732)
- docs(errors): include path/host/byte-position in error messages (#728)
- tactical helpers across postgres, config, worker (#725)
- tactical helpers across postgres, config, worker (#724)
- If-None-Match: * conditional writes; document last-writer-wins (#723)
- reaper: skip backends with open circuit; drop vault token perms warning (#722)
- share upload-level DEK + legacy backfill worker (#716)
- extract two duplicated blocks flagged by SonarCloud (#712)
- run SonarQube on non-Go PRs and replace remaining rgba bgs (#706)
- style(ui): raise UI text contrast to WCAG AA (#705)
- docs(ui): suppress go:S2092 false positives on Secure cookie flag (#703)
- pin sqlc and govulncheck via go.mod tool directive (#701)
- drop S3776 cognitive complexity violations across the repo (#692)
- graduate exhausted retries to cleanup_dlq for operator visibility (#689)
- added new benchmark
- tidy proxy test helpers and split worker ops contracts (#676 E+G+H) (#683)
- SigV4 timing equalization + reconciler stale-row sweep (#673)
- Revert "fix(test): TestCircuitBreaker_DegradedDurationIsPositive flake"
- PUT-before-COMMIT pending-row pattern with timestamp-aware reaper (#665)
- write-path cleanup timeouts, accounting symmetry, batch error (#656)
- clarity and code-reduction sweep from architecture review (#648)
- replication-aware dashboard, multi-backend file rows, admin actions (#646)
- test(di): cover audit callback, sqlite concrete store, postgres branch, watchdog backend loop (#629)
- dedupe row-mapping, encrypt-result assembly, sigv4, admin CLI (#623)
- latest benchmark
- log+observe silent errors in counter/notify; normalize log casing (#595)
- perf(test): make lifecycle backoff injectable; expand admin/ui handler coverage (#594)
- parallelize top-level tests in ui, store, breaker (#592)
- centralize magic timeouts and quiet test flakiness (#569, refs #522) (#590)
- address SonarQube findings #582-586 (#588)
- extract string constants and encryption helpers, add SonarQube (#580, #581) (#587)
- rebalancer skips moves where target already has a copy (#577) (#578)
- split store.go (1625 lines) into domain-focused files (#565) (#575)

## [0.40.1] - 2026-04-16

### Added
- add on-demand reconciliation admin endpoint (#557) (#562)

### Improved
- update CHANGELOG.md for v0.39.21 (#555)

### Other
- per-backend max_object_size to skip oversized writes (#560) (#561)
- pending gauges decrement per task for live progress (#558) (#559)

## [0.39.21] - 2026-04-15

### Improved
- update CHANGELOG.md for v0.39.19 (#552)

### Other
- exclude failed targets from replication target selection (#553) (#554)

## [0.39.19] - 2026-04-13

### Improved
- update CHANGELOG.md for v0.39.18 (#550)

### Dependencies
- chore(deps): bump the minor-and-patch group with 7 updates (#544)

### Other
- adapt Port API for moby/moby v1.54 and update otel dependencies (#551)

## [0.39.18] - 2026-04-12

### Dependencies
- chore(deps): bump github.com/aws/aws-sdk-go-v2/service/s3 (#542)
- chore(deps): bump the otel group with 4 updates (#543)

### Other
- fail startup when encryption is enabled but encryptor init fails (#548) (#549)

## [0.39.16] - 2026-04-11

### Added
- add g3 backend to free-tier guide, enlarge admin UI logo (#537) (#538)
- add new benchmark test results

### Improved
- update CHANGELOG.md for v0.38.2 (#504)

### Dependencies
- chore(deps): bump actions/github-script from 8 to 9 in the actions group (#541)
- chore(deps): bump the aws-sdk group with 2 updates (#528)
- chore(deps): bump actions/upload-artifact in the actions group (#527)
- chore(deps): bump github.com/aws/smithy-go in the minor-and-patch group (#529)
- chore(deps): bump github.com/go-jose/go-jose/v4 from 4.1.3 to 4.1.4 (#530)

### Other
- run rebalance and cleanup async to prevent client-side cancellation (#546) (#547)
- pin mermaid CDN to 11.8.0 to restore diagram tooltips (#545)
- Replicator cleans up stale metadata on source 404 (#539)
- 404 responses should not trip backend circuit breakers (#535) (#536)
- Replication target selection respects configured routing strategy (#534)
- fuzz-found false positive in presigned canonical request assertion (#523) (#526)
- testing: add 13 integration tests for edge cases and missing scenarios (#522) (#525)
- testing: add t.Parallel() to proxy, breaker, notify, audit, lifecycle (#522) (#524)
- Redis counter recovery lost-update race (#507) (#521)
- enhancement: enable gosec/errcheck/bodyclose/noctx linters, add t.Parallel() (#513) (#520)
- robustness improvements — overflow, starvation, stale probes, blocking (#514) (#519)
- extract testable run(), fix stale paths, add benchmarks and fuzz tests (#515)
- extract testable run() from monolithic runServe(), update dev environment (#515) (#518)
- panic recovery in pipe goroutines, worker ordering fixes (#508, #509) (#517)
- concurrency and robustness fixes (#506, #510, #511, #512) (#516)
- embedded SQLite backend, init CLI, zero-dependency deployments (#505)

## [0.38.2] - 2026-03-30

### Hardened
- security hardening — Redis counter race, tree API auth, SigV4 edge cases (#488) (#491)

### Improved
- update CHANGELOG.md for v0.37.2 (#486)

### Other
- strip whitespace from SigV4 header names, add fuzz-import tooling (#498) (#503)
- close onboarding gaps for replication with encryption (#501) (#502)
- fuzz-found bugs in SigV4 canonical request and encryption header parsing (#495, #496) (#497)
- cosign signing, Vault DEK caching on failover, CI improvements (#381, #425) (#494)
- shutdown correctness, worker observability, and operational robustness (#490) (#493)
- config validation gaps that defer errors to runtime (#489) (#492)
- deduplicate Grafana dashboard panels on redeploy (#485) (#487)

## [0.37.2] - 2026-03-29

### Improved
- update CHANGELOG.md for v0.37.1 (#484)

## [0.37.1] - 2026-03-29

### Fixed
- fixup: bump version

### Improved
- update CHANGELOG.md for v0.37.0 (#483)

## [0.37.0] - 2026-03-29

### Added
- add timeouts, release preflight integration tests, Dependabot grouping, linters, SBOM (#456) (#478)
- add edge case coverage for lifecycle, config, auth cache, admission (#450) (#477)

### Fixed
- fix!: require session_secret when UI is enabled, remove admin_secret fallback (#442) (#476)
- fixup: add new benchmark file

### Improved
- update CHANGELOG.md for v0.33.0 (#473)

### Dependencies
- chore(deps): bump aquasecurity/trivy-action (#470)

### Other
- web Dockerfile build context blocked by root .dockerignore
- event notifications, DI refactor, package restructuring (#360) (#481)
- configurable multipart timeout, capacity warning, production docs (#480)
- added latest bench test
- cache read error, download Content-Type, unknown keyID, range clamp (#439, #440, #441, #444) (#475)
- testing: improve fuzz targets and benchmark coverage (#472) (#474)

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
