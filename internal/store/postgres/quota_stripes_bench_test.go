// -------------------------------------------------------------------------------
// Striped Quota Counter Benchmarks
//
// Author: Alex Freidah
//
// Measures the two things a write does to the quota tables: charging the
// backend's byte counter inside its transaction, and claiming the space before
// the upload starts. Both are per-object costs on the write path, and both are
// about what happens when several writes charge one backend at once, so every
// case here is parallel.
//
// The charge benchmark varies the stripe fan-out rather than using
// QuotaStripeCount, because the question it answers is what the fan-out is
// worth: at one stripe every writer takes the same row lock and holds it to
// commit, which is the behavior striping exists to remove.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// The backends the benchmarks charge, kept apart from the ones the tests seed
// so a benchmark's bytes never move a total a test asserts on.
const (
	benchChargeBackend = "bench-charge"
	benchClaimBackend  = "bench-claim"
	benchFullBackend   = "bench-full"
)

// benchObjectSize is the per-operation charge. Small enough that neither
// benchmark can exhaust its backend's ceiling within a run.
const benchObjectSize = 1 << 10

// -------------------------------------------------------------------------
// FIXTURE
// -------------------------------------------------------------------------

// benchQuotaStore returns the suite's store with the benchmark backends
// seeded: two with room to spare and one whose ceiling is a single byte, so a
// claim against it always declines.
//
// Leftover intents from an earlier run are cleared, because a pending row
// holds its bytes against the headroom the claim benchmark measures.
func benchQuotaStore(b *testing.B) *Store {
	b.Helper()
	s := adapterPgStore(b)
	ctx := b.Context()

	err := s.SyncQuotaLimits(ctx, []config.BackendConfig{
		{Name: benchChargeBackend, QuotaBytes: 1 << 50},
		{Name: benchClaimBackend, QuotaBytes: 1 << 50},
		{Name: benchFullBackend, QuotaBytes: 1},
	})
	if err != nil {
		b.Fatalf("SyncQuotaLimits: %v", err)
	}

	for _, backend := range []string{benchClaimBackend, benchFullBackend} {
		if err := s.DeletePendingByBackend(ctx, backend); err != nil {
			b.Fatalf("DeletePendingByBackend(%s): %v", backend, err)
		}
	}
	return s
}

// benchPendingObject builds the minimal intent the claim path inserts.
func benchPendingObject(intentID, backend string) *core.PendingObject {
	return &core.PendingObject{
		IntentID:    intentID,
		ObjectKey:   "bench/" + intentID,
		BackendName: backend,
		SizeBytes:   benchObjectSize,
	}
}

// -------------------------------------------------------------------------
// CHARGE
// -------------------------------------------------------------------------

// BenchmarkQuotaStripes_ConcurrentCharge measures concurrent charges against
// one backend as the stripe fan-out grows.
//
// Each iteration is a transaction that adjusts one stripe and commits, which
// is how the write path charges: the counter update lives inside the
// transaction that writes object_locations, so the row lock is held for the
// length of it rather than released as soon as the statement returns.
//
// A goroutine takes its stripe once and walks forward from there, so the
// spread across stripes does not itself become a contended atomic.
func BenchmarkQuotaStripes_ConcurrentCharge(b *testing.B) {
	s := benchQuotaStore(b)
	ctx := b.Context()

	for _, stripes := range []int64{1, 4, 16, 64} {
		b.Run(fmt.Sprintf("stripes=%d", stripes), func(b *testing.B) {
			var goroutines atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				n := goroutines.Add(1)
				for pb.Next() {
					stripe := int16(n % stripes)
					n++
					err := s.WithTx(ctx, func(ctx context.Context, tx core.TxAdapter) error {
						return tx.AdjustQuotaStripe(ctx, benchChargeBackend, stripe, benchObjectSize)
					})
					if err != nil {
						b.Errorf("AdjustQuotaStripe: %v", err)
						return
					}
				}
			})
		})
	}
}

// -------------------------------------------------------------------------
// CLAIM
// -------------------------------------------------------------------------

// BenchmarkQuotaClaim_HasRoom measures the admitting claim: the conditional
// insert that reads a backend's committed bytes, orphans and in-flight rows
// and records the intent if they leave room.
//
// Claim and release are timed as a pair. Measuring the insert alone would grow
// pending_objects for the length of the run and the in-flight subquery with
// it, so the cost would climb with b.N and describe the benchmark rather than
// the write path. A completing write releases its intent anyway.
func BenchmarkQuotaClaim_HasRoom(b *testing.B) {
	s := benchQuotaStore(b)
	ctx := b.Context()

	var goroutines atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		g := goroutines.Add(1)
		i := 0
		for pb.Next() {
			intentID := fmt.Sprintf("bench-claim-%d-%d", g, i)
			i++

			admitted, err := s.InsertPendingIfFits(ctx, benchPendingObject(intentID, benchClaimBackend))
			if err != nil {
				b.Errorf("InsertPendingIfFits: %v", err)
				return
			}
			if !admitted {
				b.Errorf("claim declined on a backend with room")
				return
			}
			if err := s.DeletePending(ctx, intentID); err != nil {
				b.Errorf("DeletePending: %v", err)
				return
			}
		}
	})
}

// BenchmarkQuotaClaim_AtCeiling measures a claim a backend refuses, which is
// what routing pays per candidate it has to skip before finding one with room.
//
// Nothing is inserted, so no cleanup is needed and the table the headroom
// subqueries scan stays empty for the whole run.
func BenchmarkQuotaClaim_AtCeiling(b *testing.B) {
	s := benchQuotaStore(b)
	ctx := b.Context()

	var goroutines atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		g := goroutines.Add(1)
		i := 0
		for pb.Next() {
			intentID := fmt.Sprintf("bench-full-%d-%d", g, i)
			i++

			admitted, err := s.InsertPendingIfFits(ctx, benchPendingObject(intentID, benchFullBackend))
			if err != nil {
				b.Errorf("InsertPendingIfFits: %v", err)
				return
			}
			if admitted {
				b.Errorf("claim admitted on a backend at its ceiling")
				return
			}
		}
	})
}
