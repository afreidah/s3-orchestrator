window.BENCHMARK_DATA = {
  "lastUpdate": 1778829281211,
  "repoUrl": "https://github.com/afreidah/s3-orchestrator",
  "entries": {
    "Go benchmarks": [
      {
        "commit": {
          "author": {
            "name": "dependabot[bot]",
            "username": "dependabot[bot]",
            "email": "49699333+dependabot[bot]@users.noreply.github.com"
          },
          "committer": {
            "name": "GitHub",
            "username": "web-flow",
            "email": "noreply@github.com"
          },
          "id": "0688dc30aa6bef497d479d29e68c8397eaa69724",
          "message": "chore(deps): bump the minor-and-patch group with 2 updates (#837)\n\nBumps the minor-and-patch group with 2 updates: [golang.org/x/crypto](https://github.com/golang/crypto) and [modernc.org/sqlite](https://gitlab.com/cznic/sqlite).\n\n\nUpdates `golang.org/x/crypto` from 0.50.0 to 0.51.0\n- [Commits](https://github.com/golang/crypto/compare/v0.50.0...v0.51.0)\n\nUpdates `modernc.org/sqlite` from 1.50.0 to 1.50.1\n- [Changelog](https://gitlab.com/cznic/sqlite/blob/master/CHANGELOG.md)\n- [Commits](https://gitlab.com/cznic/sqlite/compare/v1.50.0...v1.50.1)\n\n---\nupdated-dependencies:\n- dependency-name: golang.org/x/crypto\n  dependency-version: 0.51.0\n  dependency-type: direct:production\n  update-type: version-update:semver-minor\n  dependency-group: minor-and-patch\n- dependency-name: modernc.org/sqlite\n  dependency-version: 1.50.1\n  dependency-type: direct:production\n  update-type: version-update:semver-patch\n  dependency-group: minor-and-patch\n...\n\nSigned-off-by: dependabot[bot] <support@github.com>\nCo-authored-by: dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>\nCo-authored-by: Alex Freidah <alex.freidah@gmail.com>",
          "timestamp": "2026-05-15T07:12:19Z",
          "url": "https://github.com/afreidah/s3-orchestrator/commit/0688dc30aa6bef497d479d29e68c8397eaa69724"
        },
        "date": 1778829280796,
        "tool": "go",
        "benches": [
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 11.23,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 11.23,
            "unit": "ns/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 74.25,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "16145755 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 74.25,
            "unit": "ns/op",
            "extra": "16145755 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "16145755 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "16145755 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 115.6,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "10356466 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 115.6,
            "unit": "ns/op",
            "extra": "10356466 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "10356466 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "10356466 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 31.67,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "37385194 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 31.67,
            "unit": "ns/op",
            "extra": "37385194 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "37385194 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "37385194 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 75.99,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "15816265 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 75.99,
            "unit": "ns/op",
            "extra": "15816265 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "15816265 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "15816265 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 9.522,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "126038635 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 9.522,
            "unit": "ns/op",
            "extra": "126038635 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "126038635 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "126038635 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 244,
            "unit": "ns/op\t     176 B/op\t       3 allocs/op",
            "extra": "4918858 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 244,
            "unit": "ns/op",
            "extra": "4918858 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 176,
            "unit": "B/op",
            "extra": "4918858 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4918858 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 282.1,
            "unit": "ns/op\t     176 B/op\t       3 allocs/op",
            "extra": "4334484 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 282.1,
            "unit": "ns/op",
            "extra": "4334484 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 176,
            "unit": "B/op",
            "extra": "4334484 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4334484 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 145.9,
            "unit": "ns/op\t      17 B/op\t       0 allocs/op",
            "extra": "8088988 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 145.9,
            "unit": "ns/op",
            "extra": "8088988 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 17,
            "unit": "B/op",
            "extra": "8088988 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "8088988 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 36.13,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "33170370 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 36.13,
            "unit": "ns/op",
            "extra": "33170370 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "33170370 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "33170370 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 95.8,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12176841 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 95.8,
            "unit": "ns/op",
            "extra": "12176841 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12176841 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12176841 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 14.81,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "80755558 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 14.81,
            "unit": "ns/op",
            "extra": "80755558 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "80755558 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "80755558 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 708045,
            "unit": "ns/op\t1480.95 MB/s\t 2426996 B/op\t      41 allocs/op",
            "extra": "1506 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 708045,
            "unit": "ns/op",
            "extra": "1506 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 1480.95,
            "unit": "MB/s",
            "extra": "1506 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 2426996,
            "unit": "B/op",
            "extra": "1506 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 41,
            "unit": "allocs/op",
            "extra": "1506 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 523299,
            "unit": "ns/op\t2003.78 MB/s\t 1124201 B/op\t      24 allocs/op",
            "extra": "2275 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 523299,
            "unit": "ns/op",
            "extra": "2275 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 2003.78,
            "unit": "MB/s",
            "extra": "2275 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 1124201,
            "unit": "B/op",
            "extra": "2275 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 24,
            "unit": "allocs/op",
            "extra": "2275 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 2429720,
            "unit": "ns/op\t 431.56 MB/s\t 5794183 B/op\t      92 allocs/op",
            "extra": "508 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 2429720,
            "unit": "ns/op",
            "extra": "508 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 431.56,
            "unit": "MB/s",
            "extra": "508 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 5794183,
            "unit": "B/op",
            "extra": "508 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 92,
            "unit": "allocs/op",
            "extra": "508 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 11.26,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 11.26,
            "unit": "ns/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 9620,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "123849 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 9620,
            "unit": "ns/op",
            "extra": "123849 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "123849 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "123849 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 6.751,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "176357298 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 6.751,
            "unit": "ns/op",
            "extra": "176357298 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "176357298 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "176357298 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 185,
            "unit": "ns/op\t     480 B/op\t       2 allocs/op",
            "extra": "6369886 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 185,
            "unit": "ns/op",
            "extra": "6369886 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 480,
            "unit": "B/op",
            "extra": "6369886 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "6369886 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 88.92,
            "unit": "ns/op\t     160 B/op\t       1 allocs/op",
            "extra": "13548748 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 88.92,
            "unit": "ns/op",
            "extra": "13548748 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 160,
            "unit": "B/op",
            "extra": "13548748 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "13548748 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 55.74,
            "unit": "ns/op\t     160 B/op\t       1 allocs/op",
            "extra": "21325422 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 55.74,
            "unit": "ns/op",
            "extra": "21325422 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 160,
            "unit": "B/op",
            "extra": "21325422 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "21325422 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 1443,
            "unit": "ns/op\t2838.52 MB/s\t    4256 B/op\t       5 allocs/op",
            "extra": "862530 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 1443,
            "unit": "ns/op",
            "extra": "862530 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 2838.52,
            "unit": "MB/s",
            "extra": "862530 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 4256,
            "unit": "B/op",
            "extra": "862530 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "862530 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 240012,
            "unit": "ns/op\t4368.84 MB/s\t 1048743 B/op\t       5 allocs/op",
            "extra": "6427 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 240012,
            "unit": "ns/op",
            "extra": "6427 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 4368.84,
            "unit": "MB/s",
            "extra": "6427 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 1048743,
            "unit": "B/op",
            "extra": "6427 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "6427 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 2200734,
            "unit": "ns/op\t15246.93 MB/s\t33554593 B/op\t       5 allocs/op",
            "extra": "546 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 2200734,
            "unit": "ns/op",
            "extra": "546 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 15246.93,
            "unit": "MB/s",
            "extra": "546 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 33554593,
            "unit": "B/op",
            "extra": "546 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "546 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 27848487,
            "unit": "ns/op\t2409.78 MB/s\t     896 B/op\t       9 allocs/op",
            "extra": "42 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 27848487,
            "unit": "ns/op",
            "extra": "42 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 2409.78,
            "unit": "MB/s",
            "extra": "42 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 896,
            "unit": "B/op",
            "extra": "42 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 9,
            "unit": "allocs/op",
            "extra": "42 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 57.18,
            "unit": "ns/op\t      64 B/op\t       2 allocs/op",
            "extra": "20253505 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 57.18,
            "unit": "ns/op",
            "extra": "20253505 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 64,
            "unit": "B/op",
            "extra": "20253505 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "20253505 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 31515,
            "unit": "ns/op\t     236 B/op\t       8 allocs/op",
            "extra": "38341 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 31515,
            "unit": "ns/op",
            "extra": "38341 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 236,
            "unit": "B/op",
            "extra": "38341 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 8,
            "unit": "allocs/op",
            "extra": "38341 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 83.55,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "14321953 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 83.55,
            "unit": "ns/op",
            "extra": "14321953 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "14321953 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "14321953 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 19.84,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "60217641 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 19.84,
            "unit": "ns/op",
            "extra": "60217641 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "60217641 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "60217641 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 105,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "11246895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 105,
            "unit": "ns/op",
            "extra": "11246895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "11246895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "11246895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 123.4,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "9717982 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 123.4,
            "unit": "ns/op",
            "extra": "9717982 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "9717982 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "9717982 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 88.02,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "13696596 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 88.02,
            "unit": "ns/op",
            "extra": "13696596 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "13696596 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "13696596 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 104.1,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12239118 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 104.1,
            "unit": "ns/op",
            "extra": "12239118 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12239118 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12239118 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 38.84,
            "unit": "ns/op\t105452.35 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "30092827 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 38.84,
            "unit": "ns/op",
            "extra": "30092827 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 105452.35,
            "unit": "MB/s",
            "extra": "30092827 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "30092827 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "30092827 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 39.32,
            "unit": "ns/op\t1666666.43 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "29799565 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 39.32,
            "unit": "ns/op",
            "extra": "29799565 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1666666.43,
            "unit": "MB/s",
            "extra": "29799565 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "29799565 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "29799565 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 41.55,
            "unit": "ns/op\t25239400.28 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "27999751 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 41.55,
            "unit": "ns/op",
            "extra": "27999751 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 25239400.28,
            "unit": "MB/s",
            "extra": "27999751 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "27999751 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "27999751 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 32.33,
            "unit": "ns/op\t518990909.94 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "37189743 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 32.33,
            "unit": "ns/op",
            "extra": "37189743 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 518990909.94,
            "unit": "MB/s",
            "extra": "37189743 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "37189743 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "37189743 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 52205,
            "unit": "ns/op\t1255.36 MB/s\t  204447 B/op\t      30 allocs/op",
            "extra": "22911 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 52205,
            "unit": "ns/op",
            "extra": "22911 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1255.36,
            "unit": "MB/s",
            "extra": "22911 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 204447,
            "unit": "B/op",
            "extra": "22911 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 30,
            "unit": "allocs/op",
            "extra": "22911 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 701841,
            "unit": "ns/op\t1494.04 MB/s\t 3277651 B/op\t      39 allocs/op",
            "extra": "1744 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 701841,
            "unit": "ns/op",
            "extra": "1744 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1494.04,
            "unit": "MB/s",
            "extra": "1744 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 3277651,
            "unit": "B/op",
            "extra": "1744 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 39,
            "unit": "allocs/op",
            "extra": "1744 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 4010277,
            "unit": "ns/op\t4183.56 MB/s\t53003300 B/op\t      47 allocs/op",
            "extra": "291 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 4010277,
            "unit": "ns/op",
            "extra": "291 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 4183.56,
            "unit": "MB/s",
            "extra": "291 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 53003300,
            "unit": "B/op",
            "extra": "291 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 47,
            "unit": "allocs/op",
            "extra": "291 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 5986,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "199652 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 5986,
            "unit": "ns/op",
            "extra": "199652 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "199652 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "199652 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 2481,
            "unit": "ns/op\t    2144 B/op\t      29 allocs/op",
            "extra": "439609 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 2481,
            "unit": "ns/op",
            "extra": "439609 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 2144,
            "unit": "B/op",
            "extra": "439609 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 29,
            "unit": "allocs/op",
            "extra": "439609 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 188.9,
            "unit": "ns/op\t     336 B/op\t       2 allocs/op",
            "extra": "6263713 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 188.9,
            "unit": "ns/op",
            "extra": "6263713 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 336,
            "unit": "B/op",
            "extra": "6263713 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "6263713 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 41.32,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "28810771 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 41.32,
            "unit": "ns/op",
            "extra": "28810771 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "28810771 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "28810771 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6015,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "194337 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6015,
            "unit": "ns/op",
            "extra": "194337 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "194337 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "194337 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 5969,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "189648 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 5969,
            "unit": "ns/op",
            "extra": "189648 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "189648 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "189648 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 7381,
            "unit": "ns/op\t    4680 B/op\t      68 allocs/op",
            "extra": "160641 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 7381,
            "unit": "ns/op",
            "extra": "160641 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4680,
            "unit": "B/op",
            "extra": "160641 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 68,
            "unit": "allocs/op",
            "extra": "160641 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 12741,
            "unit": "ns/op\t    8352 B/op\t     104 allocs/op",
            "extra": "93799 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 12741,
            "unit": "ns/op",
            "extra": "93799 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 8352,
            "unit": "B/op",
            "extra": "93799 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 104,
            "unit": "allocs/op",
            "extra": "93799 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 2052,
            "unit": "ns/op\t     896 B/op\t      17 allocs/op",
            "extra": "581242 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 2052,
            "unit": "ns/op",
            "extra": "581242 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 896,
            "unit": "B/op",
            "extra": "581242 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 17,
            "unit": "allocs/op",
            "extra": "581242 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 9645,
            "unit": "ns/op\t    6088 B/op\t      82 allocs/op",
            "extra": "124011 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 9645,
            "unit": "ns/op",
            "extra": "124011 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 6088,
            "unit": "B/op",
            "extra": "124011 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 82,
            "unit": "allocs/op",
            "extra": "124011 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 191.8,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "6192542 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 191.8,
            "unit": "ns/op",
            "extra": "6192542 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "6192542 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "6192542 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 280,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "4269252 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 280,
            "unit": "ns/op",
            "extra": "4269252 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "4269252 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "4269252 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 687.4,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "1743354 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 687.4,
            "unit": "ns/op",
            "extra": "1743354 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "1743354 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "1743354 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 28.26,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "42186489 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 28.26,
            "unit": "ns/op",
            "extra": "42186489 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "42186489 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "42186489 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 62.02,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "20092466 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 62.02,
            "unit": "ns/op",
            "extra": "20092466 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "20092466 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "20092466 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 28.63,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "41874492 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 28.63,
            "unit": "ns/op",
            "extra": "41874492 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "41874492 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "41874492 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 28.79,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "42278528 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 28.79,
            "unit": "ns/op",
            "extra": "42278528 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "42278528 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "42278528 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 39.29,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "32263525 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 39.29,
            "unit": "ns/op",
            "extra": "32263525 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "32263525 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "32263525 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 42.92,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "27349471 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 42.92,
            "unit": "ns/op",
            "extra": "27349471 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "27349471 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "27349471 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 40.58,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "29572017 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 40.58,
            "unit": "ns/op",
            "extra": "29572017 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "29572017 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "29572017 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 28.04,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "43869297 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 28.04,
            "unit": "ns/op",
            "extra": "43869297 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "43869297 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "43869297 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 52.97,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "22807075 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 52.97,
            "unit": "ns/op",
            "extra": "22807075 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "22807075 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "22807075 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 12.63,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "96017329 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 12.63,
            "unit": "ns/op",
            "extra": "96017329 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "96017329 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "96017329 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1.064,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "1000000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1.064,
            "unit": "ns/op",
            "extra": "1000000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "1000000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "1000000000 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 228.6,
            "unit": "ns/op\t      48 B/op\t       3 allocs/op",
            "extra": "5214879 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 228.6,
            "unit": "ns/op",
            "extra": "5214879 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "5214879 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "5214879 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 640.3,
            "unit": "ns/op\t     432 B/op\t       8 allocs/op",
            "extra": "1874438 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 640.3,
            "unit": "ns/op",
            "extra": "1874438 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 432,
            "unit": "B/op",
            "extra": "1874438 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 8,
            "unit": "allocs/op",
            "extra": "1874438 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1730,
            "unit": "ns/op\t    1160 B/op\t      18 allocs/op",
            "extra": "710637 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1730,
            "unit": "ns/op",
            "extra": "710637 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 1160,
            "unit": "B/op",
            "extra": "710637 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 18,
            "unit": "allocs/op",
            "extra": "710637 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 7961,
            "unit": "ns/op\t    5320 B/op\t      62 allocs/op",
            "extra": "148384 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 7961,
            "unit": "ns/op",
            "extra": "148384 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 5320,
            "unit": "B/op",
            "extra": "148384 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 62,
            "unit": "allocs/op",
            "extra": "148384 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 70.12,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "17144421 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 70.12,
            "unit": "ns/op",
            "extra": "17144421 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "17144421 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "17144421 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 521.2,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "2301526 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 521.2,
            "unit": "ns/op",
            "extra": "2301526 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "2301526 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "2301526 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1150,
            "unit": "ns/op\t    1299 B/op\t      14 allocs/op",
            "extra": "938865 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1150,
            "unit": "ns/op",
            "extra": "938865 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 1299,
            "unit": "B/op",
            "extra": "938865 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 14,
            "unit": "allocs/op",
            "extra": "938865 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 17188,
            "unit": "ns/op\t    7946 B/op\t      30 allocs/op",
            "extra": "69901 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 17188,
            "unit": "ns/op",
            "extra": "69901 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 7946,
            "unit": "B/op",
            "extra": "69901 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 30,
            "unit": "allocs/op",
            "extra": "69901 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 137021,
            "unit": "ns/op\t   24716 B/op\t     122 allocs/op",
            "extra": "8636 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 137021,
            "unit": "ns/op",
            "extra": "8636 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 24716,
            "unit": "B/op",
            "extra": "8636 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 122,
            "unit": "allocs/op",
            "extra": "8636 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1332375,
            "unit": "ns/op\t  190857 B/op\t    1022 allocs/op",
            "extra": "890 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1332375,
            "unit": "ns/op",
            "extra": "890 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 190857,
            "unit": "B/op",
            "extra": "890 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1022,
            "unit": "allocs/op",
            "extra": "890 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 82453,
            "unit": "ns/op\t   97776 B/op\t    1002 allocs/op",
            "extra": "14553 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 82453,
            "unit": "ns/op",
            "extra": "14553 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 97776,
            "unit": "B/op",
            "extra": "14553 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1002,
            "unit": "allocs/op",
            "extra": "14553 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 195,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6021055 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 195,
            "unit": "ns/op",
            "extra": "6021055 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6021055 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6021055 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 194.4,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6168583 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 194.4,
            "unit": "ns/op",
            "extra": "6168583 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6168583 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6168583 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 198.5,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6385842 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 198.5,
            "unit": "ns/op",
            "extra": "6385842 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6385842 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6385842 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui)",
            "value": 73155468,
            "unit": "ns/op\t   17074 B/op\t      83 allocs/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - ns/op",
            "value": 73155468,
            "unit": "ns/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - B/op",
            "value": 17074,
            "unit": "B/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - allocs/op",
            "value": 83,
            "unit": "allocs/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui)",
            "value": 73250993,
            "unit": "ns/op\t   16967 B/op\t      82 allocs/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - ns/op",
            "value": 73250993,
            "unit": "ns/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - B/op",
            "value": 16967,
            "unit": "B/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - allocs/op",
            "value": 82,
            "unit": "allocs/op",
            "extra": "15 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 51.85,
            "unit": "ns/op\t78994.53 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "22896457 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 51.85,
            "unit": "ns/op",
            "extra": "22896457 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 78994.53,
            "unit": "MB/s",
            "extra": "22896457 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "22896457 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "22896457 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 52.13,
            "unit": "ns/op\t1257225.23 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "23025278 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 52.13,
            "unit": "ns/op",
            "extra": "23025278 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 1257225.23,
            "unit": "MB/s",
            "extra": "23025278 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "23025278 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "23025278 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 53.39,
            "unit": "ns/op\t19639207.03 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "21980287 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 53.39,
            "unit": "ns/op",
            "extra": "21980287 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 19639207.03,
            "unit": "MB/s",
            "extra": "21980287 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "21980287 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "21980287 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 49.23,
            "unit": "ns/op\t340814391.89 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "24033030 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 49.23,
            "unit": "ns/op",
            "extra": "24033030 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 340814391.89,
            "unit": "MB/s",
            "extra": "24033030 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "24033030 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "24033030 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 5345,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "254667 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 5345,
            "unit": "ns/op",
            "extra": "254667 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "254667 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "254667 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 48836,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "24558 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 48836,
            "unit": "ns/op",
            "extra": "24558 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "24558 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "24558 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 470605,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "2532 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 470605,
            "unit": "ns/op",
            "extra": "2532 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "2532 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "2532 times\n4 procs"
          }
        ]
      }
    ]
  }
}