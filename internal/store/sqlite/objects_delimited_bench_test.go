// -------------------------------------------------------------------------------
// SQLite ListObjectsDelimited - Benchmark
//
// Author: Alex Freidah
//
// Measures the loose-index-scan delimiter query as the underlying keyspace
// grows while the number of emitted CommonPrefixes stays fixed. Latency and
// allocations should stay roughly flat across sizes - that is the whole point of
// the skip-scan: cost tracks the prefixes returned, not the keys under them.
// -------------------------------------------------------------------------------

package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// newBenchStore builds an in-memory store for benchmarks (newTestStore is
// *testing.T only).
func newBenchStore(b *testing.B) *Store {
	b.Helper()
	ctx := context.Background()
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	b.Cleanup(func() { s.Close() })
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{{Name: "backend-a", QuotaBytes: 1 << 40}}); err != nil {
		b.Fatalf("SyncQuotaLimits: %v", err)
	}
	return s
}

// BenchmarkListObjectsDelimited spreads each keyspace across 8 second-level
// directories, so a delimiter list always emits 8 CommonPrefixes regardless of
// how many keys back them.
func BenchmarkListObjectsDelimited(b *testing.B) {
	const dirs = 8
	ctx := context.Background()
	for _, n := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("keys=%d", n), func(b *testing.B) {
			s := newBenchStore(b)
			seedDelimitedKeys(b, s, n, dirs)

			b.ResetTimer()
			for b.Loop() {
				res, err := s.ListObjectsDelimited(ctx, "logs/", "/", "", 1000)
				if err != nil {
					b.Fatalf("ListObjectsDelimited: %v", err)
				}
				if len(res.CommonPrefixes) != dirs {
					b.Fatalf("got %d prefixes, want %d", len(res.CommonPrefixes), dirs)
				}
			}
		})
	}
}

// seedDelimitedKeys records n keys spread across dirs second-level directories
// as "logs/dir<NN>/key<i>.txt".
func seedDelimitedKeys(b *testing.B, s *Store, n, dirs int) {
	b.Helper()
	ctx := context.Background()
	for i := range n {
		key := fmt.Sprintf("logs/dir%02d/key%08d.txt", i%dirs, i)
		if _, _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1}); err != nil {
			b.Fatalf("RecordObject: %v", err)
		}
	}
}
