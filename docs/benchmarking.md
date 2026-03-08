---
title: Benchmarking
weight: 80
---

# Benchmarking

The project includes micro-benchmarks for hot-path operations. Use them to
catch performance regressions before merging a branch.

## Prerequisites

```bash
make tools
```

This installs `benchstat` along with other dev dependencies.

## Workflow

### 1. Capture baseline on main (or before your changes)

```bash
go test -bench=. -benchmem -count=6 -run='^$' ./... 2>/dev/null | tee bench-before.txt
```

The `-count=6` flag gives `benchstat` enough samples for statistical
significance. For a quick sanity check, `make bench` (runs once) is fine.

### 2. Implement your changes

If your feature touches a hot path, add benchmarks alongside it. Place them in
a `*_bench_test.go` file next to the code under test. See existing examples:

| File | What it covers |
|------|---------------|
| `internal/auth/auth_bench_test.go` | SigV4 verification, token auth, canonical request building |
| `internal/storage/location_cache_bench_test.go` | Cache get/set/delete, concurrent contention, eviction |
| `internal/server/helpers_bench_test.go` | Path parsing, metadata extraction, XML encoding, error responses |

### 3. Run benchmarks again

```bash
go test -bench=. -benchmem -count=6 -run='^$' ./... 2>/dev/null | tee bench-after.txt
```

### 4. Compare

```bash
benchstat bench-before.txt bench-after.txt
```

Sample output:

```
goos: linux
goarch: amd64
pkg: github.com/afreidah/s3-orchestrator/internal/storage
                              │ bench-before │          bench-after          │
                              │    sec/op    │   sec/op    vs base           │
LocationCache_Get_Hit-8          45.2ns ± 1%   44.8ns ± 2%  ~ (p=0.35 n=6)
LocationCache_Set-8              112ns  ± 3%   245ns  ± 1%  +119% (p=0.002)
```

### 5. Interpret results

- **`~` (no change)**: Not statistically significant. No action needed.
- **Regression with low p-value (p < 0.05)**: Real slowdown. Investigate before merging.
- **Improvement**: Nice.

#### What counts as a meaningful regression

Use judgement based on the operation:

- **Auth/crypto paths** (called on every request): flag regressions > 10%.
- **Cache operations** (high frequency): flag regressions > 20%.
- **Bulk operations** like XML list encoding or eviction sweeps: flag regressions > 50%.
- **Cold paths** like error formatting: generally not worth blocking on.

## Running a subset

```bash
go test -bench=BenchmarkLocationCache -benchmem -count=6 -run='^$' ./internal/storage/ | tee bench.txt
```

## Writing new benchmarks

- File name: `*_bench_test.go` in the same package as the code.
- Use `b.Loop()` (Go 1.24+) instead of `for i := 0; i < b.N; i++`.
- Use `b.Run(name, ...)` sub-benchmarks for multiple input sizes.
- Use `b.RunParallel` for concurrency-sensitive code.
- Call `b.ResetTimer()` after expensive setup that should not be measured.

```go
func BenchmarkMyFeature(b *testing.B) {
    thing := buildExpensiveThing()
    b.ResetTimer()

    for b.Loop() {
        thing.DoWork()
    }
}
```

## Quick reference

| Command | Purpose |
|---------|---------|
| `make bench` | Run all benchmarks once (quick sanity check) |
| `go test -bench=. -benchmem -count=6 -run='^$' ./... 2>/dev/null \| tee out.txt` | Run with enough samples for benchstat |
| `benchstat before.txt after.txt` | Compare two runs |
