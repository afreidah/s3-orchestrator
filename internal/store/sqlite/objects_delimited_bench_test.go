// -------------------------------------------------------------------------------
// SQLite Delimited ListObjects - Benchmark
//
// Author: Alex Freidah
//
// Measures the loose-index-scan delimiter query as the underlying keyspace
// grows. Latency and allocations should stay roughly flat across sizes - that is
// the whole point of the skip-scan: cost tracks the page returned, not the keys
// under it or the children past it.
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
	s, err := NewStore(ctx, &config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"}, nil, testInstanceID)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	b.Cleanup(func() { s.Close() })
	if err := s.RunMigrations(ctx); err != nil {
		b.Fatalf("RunMigrations: %v", err)
	}
	if err := s.SyncQuotaLimits(ctx, []config.BackendConfig{{Name: "backend-a", QuotaBytes: 1 << 40}}); err != nil {
		b.Fatalf("SyncQuotaLimits: %v", err)
	}
	return s
}

// BenchmarkListObjectsDelimited spreads each keyspace across second-level
// directories. With 8 of them a list emits 8 CommonPrefixes however many keys
// back them, and with one per key it stops at the page however many follow.
func BenchmarkListObjectsDelimited(b *testing.B) {
	const pageSize = 1000
	ctx := context.Background()
	for _, tc := range []struct{ keys, dirs int }{
		{1_000, 8}, {10_000, 8}, {100_000, 8}, {100_000, 100_000},
	} {
		b.Run(fmt.Sprintf("keys=%d/dirs=%d", tc.keys, tc.dirs), func(b *testing.B) {
			s := newBenchStore(b)
			seedDelimitedKeys(b, s, tc.keys, tc.dirs)
			want := min(tc.dirs, pageSize)

			b.ResetTimer()
			for b.Loop() {
				res, err := s.ListObjects(ctx, "logs/", "/", "", pageSize)
				if err != nil {
					b.Fatalf("ListObjects: %v", err)
				}
				if len(res.CommonPrefixes) != want {
					b.Fatalf("got %d prefixes, want %d", len(res.CommonPrefixes), want)
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
		if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Copies: []core.ObjectCopy{{Backend: "backend-a"}}, Size: 1}); err != nil {
			b.Fatalf("RecordObject: %v", err)
		}
	}
}
