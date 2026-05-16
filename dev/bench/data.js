window.BENCHMARK_DATA = {
  "lastUpdate": 1778911148115,
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
      },
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
        "date": 1778830524812,
        "tool": "go",
        "benches": [
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 31.68,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "37662056 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 31.68,
            "unit": "ns/op",
            "extra": "37662056 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "37662056 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "37662056 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 80.69,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "14844931 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 80.69,
            "unit": "ns/op",
            "extra": "14844931 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "14844931 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "14844931 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 132,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "9144753 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 132,
            "unit": "ns/op",
            "extra": "9144753 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "9144753 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "9144753 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 73.82,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "16400317 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 73.82,
            "unit": "ns/op",
            "extra": "16400317 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "16400317 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "16400317 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 64.43,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "18131844 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 64.43,
            "unit": "ns/op",
            "extra": "18131844 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "18131844 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "18131844 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 24.24,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "49362100 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 24.24,
            "unit": "ns/op",
            "extra": "49362100 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "49362100 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "49362100 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 247,
            "unit": "ns/op\t     176 B/op\t       3 allocs/op",
            "extra": "4795520 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 247,
            "unit": "ns/op",
            "extra": "4795520 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 176,
            "unit": "B/op",
            "extra": "4795520 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4795520 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 256.4,
            "unit": "ns/op\t     176 B/op\t       3 allocs/op",
            "extra": "4686207 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 256.4,
            "unit": "ns/op",
            "extra": "4686207 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 176,
            "unit": "B/op",
            "extra": "4686207 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4686207 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 137.1,
            "unit": "ns/op\t      17 B/op\t       0 allocs/op",
            "extra": "8933656 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 137.1,
            "unit": "ns/op",
            "extra": "8933656 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 17,
            "unit": "B/op",
            "extra": "8933656 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "8933656 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 58.44,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "20397217 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 58.44,
            "unit": "ns/op",
            "extra": "20397217 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "20397217 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "20397217 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 145.4,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "8432869 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 145.4,
            "unit": "ns/op",
            "extra": "8432869 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "8432869 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "8432869 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 31.9,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "37580742 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 31.9,
            "unit": "ns/op",
            "extra": "37580742 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "37580742 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "37580742 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 710922,
            "unit": "ns/op\t1474.95 MB/s\t 2426939 B/op\t      41 allocs/op",
            "extra": "1758 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 710922,
            "unit": "ns/op",
            "extra": "1758 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 1474.95,
            "unit": "MB/s",
            "extra": "1758 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 2426939,
            "unit": "B/op",
            "extra": "1758 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 41,
            "unit": "allocs/op",
            "extra": "1758 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 582550,
            "unit": "ns/op\t1799.98 MB/s\t 1124232 B/op\t      25 allocs/op",
            "extra": "2092 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 582550,
            "unit": "ns/op",
            "extra": "2092 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 1799.98,
            "unit": "MB/s",
            "extra": "2092 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 1124232,
            "unit": "B/op",
            "extra": "2092 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 25,
            "unit": "allocs/op",
            "extra": "2092 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 1966855,
            "unit": "ns/op\t 533.12 MB/s\t 5793642 B/op\t      92 allocs/op",
            "extra": "614 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 1966855,
            "unit": "ns/op",
            "extra": "614 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 533.12,
            "unit": "MB/s",
            "extra": "614 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 5793642,
            "unit": "B/op",
            "extra": "614 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 92,
            "unit": "allocs/op",
            "extra": "614 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 6.997,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "171453578 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 6.997,
            "unit": "ns/op",
            "extra": "171453578 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "171453578 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "171453578 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 8473,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "143058 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 8473,
            "unit": "ns/op",
            "extra": "143058 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "143058 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "143058 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 4.929,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "243418060 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 4.929,
            "unit": "ns/op",
            "extra": "243418060 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "243418060 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "243418060 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 189.6,
            "unit": "ns/op\t     480 B/op\t       2 allocs/op",
            "extra": "6251371 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 189.6,
            "unit": "ns/op",
            "extra": "6251371 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 480,
            "unit": "B/op",
            "extra": "6251371 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "6251371 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 87.96,
            "unit": "ns/op\t     160 B/op\t       1 allocs/op",
            "extra": "14066602 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 87.96,
            "unit": "ns/op",
            "extra": "14066602 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 160,
            "unit": "B/op",
            "extra": "14066602 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "14066602 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 61,
            "unit": "ns/op\t     160 B/op\t       1 allocs/op",
            "extra": "19093676 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 61,
            "unit": "ns/op",
            "extra": "19093676 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 160,
            "unit": "B/op",
            "extra": "19093676 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "19093676 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 1476,
            "unit": "ns/op\t2775.12 MB/s\t    4256 B/op\t       5 allocs/op",
            "extra": "802410 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 1476,
            "unit": "ns/op",
            "extra": "802410 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 2775.12,
            "unit": "MB/s",
            "extra": "802410 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 4256,
            "unit": "B/op",
            "extra": "802410 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "802410 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 199168,
            "unit": "ns/op\t5264.77 MB/s\t 1048736 B/op\t       5 allocs/op",
            "extra": "5565 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 199168,
            "unit": "ns/op",
            "extra": "5565 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 5264.77,
            "unit": "MB/s",
            "extra": "5565 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 1048736,
            "unit": "B/op",
            "extra": "5565 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "5565 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 6471701,
            "unit": "ns/op\t5184.79 MB/s\t33554593 B/op\t       5 allocs/op",
            "extra": "178 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 6471701,
            "unit": "ns/op",
            "extra": "178 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 5184.79,
            "unit": "MB/s",
            "extra": "178 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 33554593,
            "unit": "B/op",
            "extra": "178 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "178 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 32198339,
            "unit": "ns/op\t2084.23 MB/s\t     525 B/op\t       9 allocs/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 32198339,
            "unit": "ns/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 2084.23,
            "unit": "MB/s",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 525,
            "unit": "B/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 9,
            "unit": "allocs/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 55.3,
            "unit": "ns/op\t      64 B/op\t       2 allocs/op",
            "extra": "20420289 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 55.3,
            "unit": "ns/op",
            "extra": "20420289 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 64,
            "unit": "B/op",
            "extra": "20420289 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "20420289 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 13689,
            "unit": "ns/op\t     236 B/op\t       8 allocs/op",
            "extra": "88360 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 13689,
            "unit": "ns/op",
            "extra": "88360 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 236,
            "unit": "B/op",
            "extra": "88360 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 8,
            "unit": "allocs/op",
            "extra": "88360 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 62.87,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "19055502 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 62.87,
            "unit": "ns/op",
            "extra": "19055502 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "19055502 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "19055502 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 24.54,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "48021994 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 24.54,
            "unit": "ns/op",
            "extra": "48021994 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "48021994 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "48021994 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 100.6,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12083895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 100.6,
            "unit": "ns/op",
            "extra": "12083895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12083895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12083895 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 107.1,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "11245410 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 107.1,
            "unit": "ns/op",
            "extra": "11245410 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "11245410 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "11245410 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 94.87,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12275893 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 94.87,
            "unit": "ns/op",
            "extra": "12275893 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12275893 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12275893 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 103.9,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "11560200 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 103.9,
            "unit": "ns/op",
            "extra": "11560200 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "11560200 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "11560200 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 38.04,
            "unit": "ns/op\t107683.43 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "31613539 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 38.04,
            "unit": "ns/op",
            "extra": "31613539 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 107683.43,
            "unit": "MB/s",
            "extra": "31613539 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "31613539 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "31613539 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 38.17,
            "unit": "ns/op\t1717064.29 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "30893139 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 38.17,
            "unit": "ns/op",
            "extra": "30893139 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1717064.29,
            "unit": "MB/s",
            "extra": "30893139 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "30893139 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "30893139 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 41.01,
            "unit": "ns/op\t25568675.25 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "29104813 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 41.01,
            "unit": "ns/op",
            "extra": "29104813 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 25568675.25,
            "unit": "MB/s",
            "extra": "29104813 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "29104813 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "29104813 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 32.64,
            "unit": "ns/op\t513932236.75 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "36294915 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 32.64,
            "unit": "ns/op",
            "extra": "36294915 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 513932236.75,
            "unit": "MB/s",
            "extra": "36294915 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "36294915 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "36294915 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 52330,
            "unit": "ns/op\t1252.36 MB/s\t  204446 B/op\t      30 allocs/op",
            "extra": "22856 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 52330,
            "unit": "ns/op",
            "extra": "22856 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1252.36,
            "unit": "MB/s",
            "extra": "22856 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 204446,
            "unit": "B/op",
            "extra": "22856 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 30,
            "unit": "allocs/op",
            "extra": "22856 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 648224,
            "unit": "ns/op\t1617.61 MB/s\t 3277670 B/op\t      40 allocs/op",
            "extra": "1806 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 648224,
            "unit": "ns/op",
            "extra": "1806 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1617.61,
            "unit": "MB/s",
            "extra": "1806 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 3277670,
            "unit": "B/op",
            "extra": "1806 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 40,
            "unit": "allocs/op",
            "extra": "1806 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 10532259,
            "unit": "ns/op\t1592.94 MB/s\t53003275 B/op\t      47 allocs/op",
            "extra": "100 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 10532259,
            "unit": "ns/op",
            "extra": "100 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1592.94,
            "unit": "MB/s",
            "extra": "100 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 53003275,
            "unit": "B/op",
            "extra": "100 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 47,
            "unit": "allocs/op",
            "extra": "100 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6115,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "191156 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6115,
            "unit": "ns/op",
            "extra": "191156 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "191156 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "191156 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 2575,
            "unit": "ns/op\t    2144 B/op\t      29 allocs/op",
            "extra": "468249 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 2575,
            "unit": "ns/op",
            "extra": "468249 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 2144,
            "unit": "B/op",
            "extra": "468249 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 29,
            "unit": "allocs/op",
            "extra": "468249 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 211.1,
            "unit": "ns/op\t     336 B/op\t       2 allocs/op",
            "extra": "5532180 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 211.1,
            "unit": "ns/op",
            "extra": "5532180 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 336,
            "unit": "B/op",
            "extra": "5532180 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "5532180 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 40.9,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "29180025 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 40.9,
            "unit": "ns/op",
            "extra": "29180025 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "29180025 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "29180025 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6283,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "190485 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6283,
            "unit": "ns/op",
            "extra": "190485 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "190485 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "190485 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6093,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "185517 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6093,
            "unit": "ns/op",
            "extra": "185517 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "185517 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "185517 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 7429,
            "unit": "ns/op\t    4680 B/op\t      68 allocs/op",
            "extra": "162447 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 7429,
            "unit": "ns/op",
            "extra": "162447 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4680,
            "unit": "B/op",
            "extra": "162447 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 68,
            "unit": "allocs/op",
            "extra": "162447 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 12855,
            "unit": "ns/op\t    8352 B/op\t     104 allocs/op",
            "extra": "94225 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 12855,
            "unit": "ns/op",
            "extra": "94225 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 8352,
            "unit": "B/op",
            "extra": "94225 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 104,
            "unit": "allocs/op",
            "extra": "94225 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 2091,
            "unit": "ns/op\t     896 B/op\t      17 allocs/op",
            "extra": "560505 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 2091,
            "unit": "ns/op",
            "extra": "560505 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 896,
            "unit": "B/op",
            "extra": "560505 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 17,
            "unit": "allocs/op",
            "extra": "560505 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 9897,
            "unit": "ns/op\t    6088 B/op\t      82 allocs/op",
            "extra": "119418 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 9897,
            "unit": "ns/op",
            "extra": "119418 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 6088,
            "unit": "B/op",
            "extra": "119418 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 82,
            "unit": "allocs/op",
            "extra": "119418 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 196.4,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "6059559 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 196.4,
            "unit": "ns/op",
            "extra": "6059559 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "6059559 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "6059559 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 310.9,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "3857562 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 310.9,
            "unit": "ns/op",
            "extra": "3857562 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "3857562 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "3857562 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 789,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "1519938 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 789,
            "unit": "ns/op",
            "extra": "1519938 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "1519938 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "1519938 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 40.67,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "29403316 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 40.67,
            "unit": "ns/op",
            "extra": "29403316 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "29403316 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "29403316 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 101.2,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12198357 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 101.2,
            "unit": "ns/op",
            "extra": "12198357 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12198357 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12198357 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 40.93,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "29137460 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 40.93,
            "unit": "ns/op",
            "extra": "29137460 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "29137460 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "29137460 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 40.88,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "29342827 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 40.88,
            "unit": "ns/op",
            "extra": "29342827 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "29342827 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "29342827 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 37.18,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "32173323 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 37.18,
            "unit": "ns/op",
            "extra": "32173323 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "32173323 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "32173323 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 38.92,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "30554946 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 38.92,
            "unit": "ns/op",
            "extra": "30554946 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "30554946 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "30554946 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 39.18,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "31518236 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 39.18,
            "unit": "ns/op",
            "extra": "31518236 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "31518236 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "31518236 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 26.91,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "43968427 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 26.91,
            "unit": "ns/op",
            "extra": "43968427 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "43968427 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "43968427 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 51.27,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "23629596 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 51.27,
            "unit": "ns/op",
            "extra": "23629596 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "23629596 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "23629596 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 12.15,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "98156040 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 12.15,
            "unit": "ns/op",
            "extra": "98156040 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "98156040 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "98156040 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1.243,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "958690443 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1.243,
            "unit": "ns/op",
            "extra": "958690443 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "958690443 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "958690443 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 221.2,
            "unit": "ns/op\t      48 B/op\t       3 allocs/op",
            "extra": "5422581 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 221.2,
            "unit": "ns/op",
            "extra": "5422581 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "5422581 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "5422581 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 635.3,
            "unit": "ns/op\t     432 B/op\t       8 allocs/op",
            "extra": "1889408 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 635.3,
            "unit": "ns/op",
            "extra": "1889408 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 432,
            "unit": "B/op",
            "extra": "1889408 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 8,
            "unit": "allocs/op",
            "extra": "1889408 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1729,
            "unit": "ns/op\t    1160 B/op\t      18 allocs/op",
            "extra": "686924 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1729,
            "unit": "ns/op",
            "extra": "686924 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 1160,
            "unit": "B/op",
            "extra": "686924 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 18,
            "unit": "allocs/op",
            "extra": "686924 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 7690,
            "unit": "ns/op\t    5320 B/op\t      62 allocs/op",
            "extra": "154690 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 7690,
            "unit": "ns/op",
            "extra": "154690 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 5320,
            "unit": "B/op",
            "extra": "154690 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 62,
            "unit": "allocs/op",
            "extra": "154690 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 67.54,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "17716448 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 67.54,
            "unit": "ns/op",
            "extra": "17716448 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "17716448 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "17716448 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 450,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "2665089 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 450,
            "unit": "ns/op",
            "extra": "2665089 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "2665089 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "2665089 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1232,
            "unit": "ns/op\t    1299 B/op\t      14 allocs/op",
            "extra": "921592 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1232,
            "unit": "ns/op",
            "extra": "921592 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 1299,
            "unit": "B/op",
            "extra": "921592 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 14,
            "unit": "allocs/op",
            "extra": "921592 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 17096,
            "unit": "ns/op\t    7945 B/op\t      30 allocs/op",
            "extra": "70508 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 17096,
            "unit": "ns/op",
            "extra": "70508 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 7945,
            "unit": "B/op",
            "extra": "70508 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 30,
            "unit": "allocs/op",
            "extra": "70508 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 132395,
            "unit": "ns/op\t   24704 B/op\t     122 allocs/op",
            "extra": "8872 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 132395,
            "unit": "ns/op",
            "extra": "8872 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 24704,
            "unit": "B/op",
            "extra": "8872 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 122,
            "unit": "allocs/op",
            "extra": "8872 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1283752,
            "unit": "ns/op\t  191487 B/op\t    1022 allocs/op",
            "extra": "930 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1283752,
            "unit": "ns/op",
            "extra": "930 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 191487,
            "unit": "B/op",
            "extra": "930 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1022,
            "unit": "allocs/op",
            "extra": "930 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 81826,
            "unit": "ns/op\t   97776 B/op\t    1002 allocs/op",
            "extra": "14673 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 81826,
            "unit": "ns/op",
            "extra": "14673 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 97776,
            "unit": "B/op",
            "extra": "14673 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1002,
            "unit": "allocs/op",
            "extra": "14673 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 139.5,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "8604729 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 139.5,
            "unit": "ns/op",
            "extra": "8604729 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "8604729 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "8604729 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 143.6,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "8348035 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 143.6,
            "unit": "ns/op",
            "extra": "8348035 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "8348035 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "8348035 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 180.3,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6658194 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 180.3,
            "unit": "ns/op",
            "extra": "6658194 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6658194 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6658194 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui)",
            "value": 63195145,
            "unit": "ns/op\t   16941 B/op\t      82 allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - ns/op",
            "value": 63195145,
            "unit": "ns/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - B/op",
            "value": 16941,
            "unit": "B/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - allocs/op",
            "value": 82,
            "unit": "allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui)",
            "value": 63224466,
            "unit": "ns/op\t   16852 B/op\t      81 allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - ns/op",
            "value": 63224466,
            "unit": "ns/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - B/op",
            "value": 16852,
            "unit": "B/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - allocs/op",
            "value": 81,
            "unit": "allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 48.73,
            "unit": "ns/op\t84046.93 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "23990329 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 48.73,
            "unit": "ns/op",
            "extra": "23990329 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 84046.93,
            "unit": "MB/s",
            "extra": "23990329 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "23990329 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "23990329 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 48.71,
            "unit": "ns/op\t1345488.15 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "24966320 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 48.71,
            "unit": "ns/op",
            "extra": "24966320 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 1345488.15,
            "unit": "MB/s",
            "extra": "24966320 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "24966320 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "24966320 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 49.93,
            "unit": "ns/op\t20999529.13 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "23872564 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 49.93,
            "unit": "ns/op",
            "extra": "23872564 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 20999529.13,
            "unit": "MB/s",
            "extra": "23872564 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "23872564 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "23872564 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 47.25,
            "unit": "ns/op\t355037609.02 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "24322605 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 47.25,
            "unit": "ns/op",
            "extra": "24322605 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 355037609.02,
            "unit": "MB/s",
            "extra": "24322605 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "24322605 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "24322605 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 5662,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "214707 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 5662,
            "unit": "ns/op",
            "extra": "214707 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "214707 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "214707 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 44661,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "27061 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 44661,
            "unit": "ns/op",
            "extra": "27061 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "27061 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "27061 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 423108,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "2808 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 423108,
            "unit": "ns/op",
            "extra": "2808 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "2808 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "2808 times\n4 procs"
          }
        ]
      },
      {
        "commit": {
          "author": {
            "name": "Alex Freidah",
            "username": "afreidah",
            "email": "alex.freidah@gmail.com"
          },
          "committer": {
            "name": "GitHub",
            "username": "web-flow",
            "email": "noreply@github.com"
          },
          "id": "05fd04be977fd6cc8c09802508373994b346cc08",
          "message": "docs(logging): make error-attribute rule explicit (#840)\n\nReplaces the \"all three forms acceptable\" wording with a concrete rule:\ndefault to bare \"error\", err in key-value calls, use logfmt.Err only\nwhere the typed-attr API (slog.LogAttrs, audit.Log) requires a\nslog.Attr or where err may legitimately be nil. slog.Any(\"error\", err)\nis documented as accepted-but-discouraged so the runtime handler\ncontract stays unchanged. Matches existing usage (118 bare pairs vs 3\nlogfmt.Err sites, all in slog.LogAttrs / audit.Log calls) without\nforcing structural changes to those typed-attr call sites.\n\nCloses #832",
          "timestamp": "2026-05-15T09:26:07Z",
          "url": "https://github.com/afreidah/s3-orchestrator/commit/05fd04be977fd6cc8c09802508373994b346cc08"
        },
        "date": 1778911147775,
        "tool": "go",
        "benches": [
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 10.9,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "112620242 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 10.9,
            "unit": "ns/op",
            "extra": "112620242 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "112620242 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Closed (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "112620242 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 70.33,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "16899740 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 70.33,
            "unit": "ns/op",
            "extra": "16899740 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "16899740 times\n4 procs"
          },
          {
            "name": "BenchmarkPostCheck_Success (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "16899740 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 108.7,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "11048781 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 108.7,
            "unit": "ns/op",
            "extra": "11048781 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "11048781 times\n4 procs"
          },
          {
            "name": "BenchmarkPrePostCheck_RoundTrip (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "11048781 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker)",
            "value": 30.43,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "38745013 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - ns/op",
            "value": 30.43,
            "unit": "ns/op",
            "extra": "38745013 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "38745013 times\n4 procs"
          },
          {
            "name": "BenchmarkPreCheck_Concurrent (github.com/afreidah/s3-orchestrator/internal/breaker) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "38745013 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 70.63,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "17051864 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 70.63,
            "unit": "ns/op",
            "extra": "17051864 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "17051864 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "17051864 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 9.067,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "132280518 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 9.067,
            "unit": "ns/op",
            "extra": "132280518 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "132280518 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "132280518 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 270.7,
            "unit": "ns/op\t     176 B/op\t       3 allocs/op",
            "extra": "4785093 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 270.7,
            "unit": "ns/op",
            "extra": "4785093 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 176,
            "unit": "B/op",
            "extra": "4785093 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4785093 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 276.4,
            "unit": "ns/op\t     176 B/op\t       3 allocs/op",
            "extra": "4327172 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 276.4,
            "unit": "ns/op",
            "extra": "4327172 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 176,
            "unit": "B/op",
            "extra": "4327172 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Put_Eviction (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4327172 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache)",
            "value": 132.5,
            "unit": "ns/op\t      17 B/op\t       0 allocs/op",
            "extra": "8838416 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - ns/op",
            "value": 132.5,
            "unit": "ns/op",
            "extra": "8838416 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - B/op",
            "value": 17,
            "unit": "B/op",
            "extra": "8838416 times\n4 procs"
          },
          {
            "name": "BenchmarkMemoryCache_Concurrent_ReadWrite (github.com/afreidah/s3-orchestrator/internal/cache) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "8838416 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 35.38,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "33516362 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 35.38,
            "unit": "ns/op",
            "extra": "33516362 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "33516362 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "33516362 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 86.81,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "13864616 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 86.81,
            "unit": "ns/op",
            "extra": "13864616 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "13864616 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_WithinLimits_Parallel (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "13864616 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter)",
            "value": 14.39,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "82813299 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - ns/op",
            "value": 14.39,
            "unit": "ns/op",
            "extra": "82813299 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "82813299 times\n4 procs"
          },
          {
            "name": "BenchmarkUsageTracker_Record (github.com/afreidah/s3-orchestrator/internal/counter) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "82813299 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 822134,
            "unit": "ns/op\t1275.43 MB/s\t 2427100 B/op\t      41 allocs/op",
            "extra": "1516 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 822134,
            "unit": "ns/op",
            "extra": "1516 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 1275.43,
            "unit": "MB/s",
            "extra": "1516 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 2427100,
            "unit": "B/op",
            "extra": "1516 times\n4 procs"
          },
          {
            "name": "BenchmarkEncryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 41,
            "unit": "allocs/op",
            "extra": "1516 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 604877,
            "unit": "ns/op\t1733.54 MB/s\t 1124271 B/op\t      25 allocs/op",
            "extra": "2110 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 604877,
            "unit": "ns/op",
            "extra": "2110 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 1733.54,
            "unit": "MB/s",
            "extra": "2110 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 1124271,
            "unit": "B/op",
            "extra": "2110 times\n4 procs"
          },
          {
            "name": "BenchmarkDecryptReader (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 25,
            "unit": "allocs/op",
            "extra": "2110 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 2024276,
            "unit": "ns/op\t 518.00 MB/s\t 5794084 B/op\t      92 allocs/op",
            "extra": "537 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 2024276,
            "unit": "ns/op",
            "extra": "537 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - MB/s",
            "value": 518,
            "unit": "MB/s",
            "extra": "537 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 5794084,
            "unit": "B/op",
            "extra": "537 times\n4 procs"
          },
          {
            "name": "BenchmarkRoundTrip (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 92,
            "unit": "allocs/op",
            "extra": "537 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 9.891,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "121771134 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 9.891,
            "unit": "ns/op",
            "extra": "121771134 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "121771134 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "121771134 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 9330,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "127924 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 9330,
            "unit": "ns/op",
            "extra": "127924 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "127924 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveNonce_Sequential (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "127924 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption)",
            "value": 6.84,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "174114363 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - ns/op",
            "value": 6.84,
            "unit": "ns/op",
            "extra": "174114363 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "174114363 times\n4 procs"
          },
          {
            "name": "BenchmarkChunkNonce (github.com/afreidah/s3-orchestrator/internal/encryption) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "174114363 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 185,
            "unit": "ns/op\t     480 B/op\t       2 allocs/op",
            "extra": "6416413 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 185,
            "unit": "ns/op",
            "extra": "6416413 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 480,
            "unit": "B/op",
            "extra": "6416413 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "6416413 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 92.69,
            "unit": "ns/op\t     160 B/op\t       1 allocs/op",
            "extra": "13161141 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 92.69,
            "unit": "ns/op",
            "extra": "13161141 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 160,
            "unit": "B/op",
            "extra": "13161141 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_WithoutCallback (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "13161141 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit)",
            "value": 60.05,
            "unit": "ns/op\t     160 B/op\t       1 allocs/op",
            "extra": "19476298 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - ns/op",
            "value": 60.05,
            "unit": "ns/op",
            "extra": "19476298 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - B/op",
            "value": 160,
            "unit": "B/op",
            "extra": "19476298 times\n4 procs"
          },
          {
            "name": "BenchmarkLog_Concurrent (github.com/afreidah/s3-orchestrator/internal/observe/audit) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "19476298 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 1475,
            "unit": "ns/op\t2777.68 MB/s\t    4256 B/op\t       5 allocs/op",
            "extra": "775588 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 1475,
            "unit": "ns/op",
            "extra": "775588 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 2777.68,
            "unit": "MB/s",
            "extra": "775588 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 4256,
            "unit": "B/op",
            "extra": "775588 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/4KB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "775588 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 203259,
            "unit": "ns/op\t5158.81 MB/s\t 1048739 B/op\t       5 allocs/op",
            "extra": "5562 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 203259,
            "unit": "ns/op",
            "extra": "5562 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 5158.81,
            "unit": "MB/s",
            "extra": "5562 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 1048739,
            "unit": "B/op",
            "extra": "5562 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/1MB_memory (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "5562 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 2513474,
            "unit": "ns/op\t13349.82 MB/s\t33554594 B/op\t       5 allocs/op",
            "extra": "476 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 2513474,
            "unit": "ns/op",
            "extra": "476 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 13349.82,
            "unit": "MB/s",
            "extra": "476 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 33554594,
            "unit": "B/op",
            "extra": "476 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/32MB_memory_at_threshold (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 5,
            "unit": "allocs/op",
            "extra": "476 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 28182511,
            "unit": "ns/op\t2381.22 MB/s\t     751 B/op\t       9 allocs/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 28182511,
            "unit": "ns/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 2381.22,
            "unit": "MB/s",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 751,
            "unit": "B/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink/64MB_tempfile (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 9,
            "unit": "allocs/op",
            "extra": "37 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 59.83,
            "unit": "ns/op\t      64 B/op\t       2 allocs/op",
            "extra": "19435970 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 59.83,
            "unit": "ns/op",
            "extra": "19435970 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 64,
            "unit": "B/op",
            "extra": "19435970 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "19435970 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 30593,
            "unit": "ns/op\t     236 B/op\t       8 allocs/op",
            "extra": "39591 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 30593,
            "unit": "ns/op",
            "extra": "39591 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 236,
            "unit": "B/op",
            "extra": "39591 times\n4 procs"
          },
          {
            "name": "BenchmarkCopyMaterializeSink_Reset/65536KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 8,
            "unit": "allocs/op",
            "extra": "39591 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 77.96,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "15352832 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 77.96,
            "unit": "ns/op",
            "extra": "15352832 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "15352832 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Hit (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "15352832 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 19.59,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "59705199 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 19.59,
            "unit": "ns/op",
            "extra": "59705199 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "59705199 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Get_Miss (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "59705199 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 99.81,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12077983 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 99.81,
            "unit": "ns/op",
            "extra": "12077983 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12077983 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Set (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12077983 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 110.8,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "10868506 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 110.8,
            "unit": "ns/op",
            "extra": "10868506 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "10868506 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_ReadHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "10868506 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 77.6,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "15233181 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 77.6,
            "unit": "ns/op",
            "extra": "15233181 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "15233181 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Concurrent_WriteHeavy (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "15233181 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 93.64,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "12887608 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 93.64,
            "unit": "ns/op",
            "extra": "12887608 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "12887608 times\n4 procs"
          },
          {
            "name": "BenchmarkLocationCache_Contention_GetSetDelete (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "12887608 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 41.58,
            "unit": "ns/op\t98520.44 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "26601861 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 41.58,
            "unit": "ns/op",
            "extra": "26601861 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 98520.44,
            "unit": "MB/s",
            "extra": "26601861 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "26601861 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/4KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "26601861 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 41.66,
            "unit": "ns/op\t1573264.89 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "28484016 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 41.66,
            "unit": "ns/op",
            "extra": "28484016 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1573264.89,
            "unit": "MB/s",
            "extra": "28484016 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "28484016 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "28484016 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 44.35,
            "unit": "ns/op\t23645786.11 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "26789920 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 44.35,
            "unit": "ns/op",
            "extra": "26789920 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 23645786.11,
            "unit": "MB/s",
            "extra": "26789920 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "26789920 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "26789920 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 34.32,
            "unit": "ns/op\t488803242.52 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "35259396 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 34.32,
            "unit": "ns/op",
            "extra": "35259396 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 488803242.52,
            "unit": "MB/s",
            "extra": "35259396 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "35259396 times\n4 procs"
          },
          {
            "name": "BenchmarkIOCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "35259396 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 55187,
            "unit": "ns/op\t1187.52 MB/s\t  204448 B/op\t      30 allocs/op",
            "extra": "21660 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 55187,
            "unit": "ns/op",
            "extra": "21660 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1187.52,
            "unit": "MB/s",
            "extra": "21660 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 204448,
            "unit": "B/op",
            "extra": "21660 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/64KB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 30,
            "unit": "allocs/op",
            "extra": "21660 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 635413,
            "unit": "ns/op\t1650.23 MB/s\t 3277642 B/op\t      39 allocs/op",
            "extra": "1641 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 635413,
            "unit": "ns/op",
            "extra": "1641 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 1650.23,
            "unit": "MB/s",
            "extra": "1641 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 3277642,
            "unit": "B/op",
            "extra": "1641 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/1MB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 39,
            "unit": "allocs/op",
            "extra": "1641 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy)",
            "value": 5360835,
            "unit": "ns/op\t3129.59 MB/s\t53003278 B/op\t      47 allocs/op",
            "extra": "224 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - ns/op",
            "value": 5360835,
            "unit": "ns/op",
            "extra": "224 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - MB/s",
            "value": 3129.59,
            "unit": "MB/s",
            "extra": "224 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - B/op",
            "value": 53003278,
            "unit": "B/op",
            "extra": "224 times\n4 procs"
          },
          {
            "name": "BenchmarkStreamCopy/16MB (github.com/afreidah/s3-orchestrator/internal/proxy) - allocs/op",
            "value": 47,
            "unit": "allocs/op",
            "extra": "224 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6206,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "193099 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6206,
            "unit": "ns/op",
            "extra": "193099 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "193099 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "193099 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 2509,
            "unit": "ns/op\t    2144 B/op\t      29 allocs/op",
            "extra": "474796 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 2509,
            "unit": "ns/op",
            "extra": "474796 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 2144,
            "unit": "B/op",
            "extra": "474796 times\n4 procs"
          },
          {
            "name": "BenchmarkDeriveSigningKey (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 29,
            "unit": "allocs/op",
            "extra": "474796 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 193.9,
            "unit": "ns/op\t     336 B/op\t       2 allocs/op",
            "extra": "6188235 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 193.9,
            "unit": "ns/op",
            "extra": "6188235 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 336,
            "unit": "B/op",
            "extra": "6188235 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/map (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 2,
            "unit": "allocs/op",
            "extra": "6188235 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 45.57,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "26496478 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 45.57,
            "unit": "ns/op",
            "extra": "26496478 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "26496478 times\n4 procs"
          },
          {
            "name": "BenchmarkParseSigV4Fields/direct (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "26496478 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6119,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "189889 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6119,
            "unit": "ns/op",
            "extra": "189889 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "189889 times\n4 procs"
          },
          {
            "name": "BenchmarkAuthenticateAndResolveBucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "189889 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 6054,
            "unit": "ns/op\t    4024 B/op\t      56 allocs/op",
            "extra": "190107 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 6054,
            "unit": "ns/op",
            "extra": "190107 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4024,
            "unit": "B/op",
            "extra": "190107 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/0_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 56,
            "unit": "allocs/op",
            "extra": "190107 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 7423,
            "unit": "ns/op\t    4680 B/op\t      68 allocs/op",
            "extra": "159742 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 7423,
            "unit": "ns/op",
            "extra": "159742 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 4680,
            "unit": "B/op",
            "extra": "159742 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/5_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 68,
            "unit": "allocs/op",
            "extra": "159742 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 13190,
            "unit": "ns/op\t    8352 B/op\t     104 allocs/op",
            "extra": "89749 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 13190,
            "unit": "ns/op",
            "extra": "89749 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 8352,
            "unit": "B/op",
            "extra": "89749 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifySigV4_WithQueryParams/20_params (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 104,
            "unit": "allocs/op",
            "extra": "89749 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 2234,
            "unit": "ns/op\t     896 B/op\t      17 allocs/op",
            "extra": "515168 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 2234,
            "unit": "ns/op",
            "extra": "515168 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 896,
            "unit": "B/op",
            "extra": "515168 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildCanonicalRequest (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 17,
            "unit": "allocs/op",
            "extra": "515168 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 9898,
            "unit": "ns/op\t    6088 B/op\t      82 allocs/op",
            "extra": "122077 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 9898,
            "unit": "ns/op",
            "extra": "122077 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 6088,
            "unit": "B/op",
            "extra": "122077 times\n4 procs"
          },
          {
            "name": "BenchmarkVerifyPresignedSigV4 (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 82,
            "unit": "allocs/op",
            "extra": "122077 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 190.3,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "6117620 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 190.3,
            "unit": "ns/op",
            "extra": "6117620 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "6117620 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/1_bucket (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "6117620 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 279.8,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "4258065 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 279.8,
            "unit": "ns/op",
            "extra": "4258065 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "4258065 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/5_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "4258065 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth)",
            "value": 694.4,
            "unit": "ns/op\t      48 B/op\t       1 allocs/op",
            "extra": "1727722 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - ns/op",
            "value": 694.4,
            "unit": "ns/op",
            "extra": "1727722 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "1727722 times\n4 procs"
          },
          {
            "name": "BenchmarkTokenAuth/20_buckets (github.com/afreidah/s3-orchestrator/internal/transport/auth) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "1727722 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 29.98,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "39610570 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 29.98,
            "unit": "ns/op",
            "extra": "39610570 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "39610570 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "39610570 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 69.3,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "17291070 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 69.3,
            "unit": "ns/op",
            "extra": "17291070 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "17291070 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_AcquireRelease_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "17291070 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 30.26,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "39601136 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 30.26,
            "unit": "ns/op",
            "extra": "39601136 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "39601136 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/read (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "39601136 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 29.99,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "39712293 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 29.99,
            "unit": "ns/op",
            "extra": "39712293 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "39712293 times\n4 procs"
          },
          {
            "name": "BenchmarkAdmission_SplitPool_AcquireRelease/write (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "39712293 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 40.99,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "29720648 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 40.99,
            "unit": "ns/op",
            "extra": "29720648 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "29720648 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_only (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "29720648 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 45.35,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "23404177 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 45.35,
            "unit": "ns/op",
            "extra": "23404177 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "23404177 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/bucket_and_key (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "23404177 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 41.99,
            "unit": "ns/op\t      32 B/op\t       1 allocs/op",
            "extra": "28616998 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 41.99,
            "unit": "ns/op",
            "extra": "28616998 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 32,
            "unit": "B/op",
            "extra": "28616998 times\n4 procs"
          },
          {
            "name": "BenchmarkParsePath/deep_path (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "28616998 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 25.21,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "48177154 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 25.21,
            "unit": "ns/op",
            "extra": "48177154 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "48177154 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_32 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "48177154 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 47.94,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "25602788 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 47.94,
            "unit": "ns/op",
            "extra": "25602788 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "25602788 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/valid_64 (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "25602788 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 10.31,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 10.31,
            "unit": "ns/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/invalid_chars (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "100000000 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1.558,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "769611686 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1.558,
            "unit": "ns/op",
            "extra": "769611686 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "769611686 times\n4 procs"
          },
          {
            "name": "BenchmarkIsValidRequestID/empty (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "769611686 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 243.8,
            "unit": "ns/op\t      48 B/op\t       3 allocs/op",
            "extra": "4889239 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 243.8,
            "unit": "ns/op",
            "extra": "4889239 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "4889239 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/no_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 3,
            "unit": "allocs/op",
            "extra": "4889239 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 684.9,
            "unit": "ns/op\t     432 B/op\t       8 allocs/op",
            "extra": "1749998 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 684.9,
            "unit": "ns/op",
            "extra": "1749998 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 432,
            "unit": "B/op",
            "extra": "1749998 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/3_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 8,
            "unit": "allocs/op",
            "extra": "1749998 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1926,
            "unit": "ns/op\t    1160 B/op\t      18 allocs/op",
            "extra": "638359 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1926,
            "unit": "ns/op",
            "extra": "638359 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 1160,
            "unit": "B/op",
            "extra": "638359 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/10_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 18,
            "unit": "allocs/op",
            "extra": "638359 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 8679,
            "unit": "ns/op\t    5320 B/op\t      62 allocs/op",
            "extra": "138372 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 8679,
            "unit": "ns/op",
            "extra": "138372 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 5320,
            "unit": "B/op",
            "extra": "138372 times\n4 procs"
          },
          {
            "name": "BenchmarkExtractUserMetadata/50_meta (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 62,
            "unit": "allocs/op",
            "extra": "138372 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 63.09,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "18825973 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 63.09,
            "unit": "ns/op",
            "extra": "18825973 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "18825973 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/small_2keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "18825973 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 482.4,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "2493084 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 482.4,
            "unit": "ns/op",
            "extra": "2493084 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "2493084 times\n4 procs"
          },
          {
            "name": "BenchmarkValidateUserMetadata/large_20keys (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "2493084 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1212,
            "unit": "ns/op\t    1299 B/op\t      14 allocs/op",
            "extra": "883138 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1212,
            "unit": "ns/op",
            "extra": "883138 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 1299,
            "unit": "B/op",
            "extra": "883138 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteS3Error (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 14,
            "unit": "allocs/op",
            "extra": "883138 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 18844,
            "unit": "ns/op\t    7945 B/op\t      30 allocs/op",
            "extra": "63194 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 18844,
            "unit": "ns/op",
            "extra": "63194 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 7945,
            "unit": "B/op",
            "extra": "63194 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/10_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 30,
            "unit": "allocs/op",
            "extra": "63194 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 149071,
            "unit": "ns/op\t   24705 B/op\t     122 allocs/op",
            "extra": "8172 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 149071,
            "unit": "ns/op",
            "extra": "8172 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 24705,
            "unit": "B/op",
            "extra": "8172 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/100_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 122,
            "unit": "allocs/op",
            "extra": "8172 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 1457243,
            "unit": "ns/op\t  190877 B/op\t    1022 allocs/op",
            "extra": "818 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 1457243,
            "unit": "ns/op",
            "extra": "818 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 190877,
            "unit": "B/op",
            "extra": "818 times\n4 procs"
          },
          {
            "name": "BenchmarkWriteXML_ListV2/1000_objects (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1022,
            "unit": "allocs/op",
            "extra": "818 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 83521,
            "unit": "ns/op\t   97776 B/op\t    1002 allocs/op",
            "extra": "14287 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 83521,
            "unit": "ns/op",
            "extra": "14287 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 97776,
            "unit": "B/op",
            "extra": "14287 times\n4 procs"
          },
          {
            "name": "BenchmarkBuildListContents/1000_objects_3_prefixes (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 1002,
            "unit": "allocs/op",
            "extra": "14287 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 185.4,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6458419 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 185.4,
            "unit": "ns/op",
            "extra": "6458419 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6458419 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_SingleIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6458419 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 185.9,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6451294 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 185.9,
            "unit": "ns/op",
            "extra": "6451294 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6451294 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_MultiIP (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6451294 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api)",
            "value": 180.1,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "6980494 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - ns/op",
            "value": 180.1,
            "unit": "ns/op",
            "extra": "6980494 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "6980494 times\n4 procs"
          },
          {
            "name": "BenchmarkRateLimiter_Allow_Concurrent (github.com/afreidah/s3-orchestrator/internal/transport/s3api) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "6980494 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui)",
            "value": 64969853,
            "unit": "ns/op\t   16941 B/op\t      82 allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - ns/op",
            "value": 64969853,
            "unit": "ns/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - B/op",
            "value": 16941,
            "unit": "B/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_InvalidKey (github.com/afreidah/s3-orchestrator/internal/transport/ui) - allocs/op",
            "value": 82,
            "unit": "allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui)",
            "value": 64939266,
            "unit": "ns/op\t   16852 B/op\t      81 allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - ns/op",
            "value": 64939266,
            "unit": "ns/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - B/op",
            "value": 16852,
            "unit": "B/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkLogin_ValidKeyWrongSecret (github.com/afreidah/s3-orchestrator/internal/transport/ui) - allocs/op",
            "value": 81,
            "unit": "allocs/op",
            "extra": "18 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 54.91,
            "unit": "ns/op\t74591.25 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "22002050 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 54.91,
            "unit": "ns/op",
            "extra": "22002050 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 74591.25,
            "unit": "MB/s",
            "extra": "22002050 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "22002050 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/4KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "22002050 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 54.76,
            "unit": "ns/op\t1196866.70 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "21830013 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 54.76,
            "unit": "ns/op",
            "extra": "21830013 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 1196866.7,
            "unit": "MB/s",
            "extra": "21830013 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "21830013 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/64KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "21830013 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 56.06,
            "unit": "ns/op\t18702957.19 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "21012361 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 56.06,
            "unit": "ns/op",
            "extra": "21012361 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 18702957.19,
            "unit": "MB/s",
            "extra": "21012361 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "21012361 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/1024KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "21012361 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool)",
            "value": 52.4,
            "unit": "ns/op\t320188737.37 MB/s\t      48 B/op\t       1 allocs/op",
            "extra": "22463647 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - ns/op",
            "value": 52.4,
            "unit": "ns/op",
            "extra": "22463647 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - MB/s",
            "value": 320188737.37,
            "unit": "MB/s",
            "extra": "22463647 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - B/op",
            "value": 48,
            "unit": "B/op",
            "extra": "22463647 times\n4 procs"
          },
          {
            "name": "BenchmarkCopy/16384KB (github.com/afreidah/s3-orchestrator/internal/util/bufpool) - allocs/op",
            "value": 1,
            "unit": "allocs/op",
            "extra": "22463647 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 6586,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "183844 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 6586,
            "unit": "ns/op",
            "extra": "183844 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "183844 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/100_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "183844 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 52282,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "22864 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 52282,
            "unit": "ns/op",
            "extra": "22864 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "22864 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/1000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "22864 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil)",
            "value": 498187,
            "unit": "ns/op\t       0 B/op\t       0 allocs/op",
            "extra": "2428 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - ns/op",
            "value": 498187,
            "unit": "ns/op",
            "extra": "2428 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - B/op",
            "value": 0,
            "unit": "B/op",
            "extra": "2428 times\n4 procs"
          },
          {
            "name": "BenchmarkTTLCache_Eviction/10000_entries (github.com/afreidah/s3-orchestrator/internal/util/syncutil) - allocs/op",
            "value": 0,
            "unit": "allocs/op",
            "extra": "2428 times\n4 procs"
          }
        ]
      }
    ]
  }
}