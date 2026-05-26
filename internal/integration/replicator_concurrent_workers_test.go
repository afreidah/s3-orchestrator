// -------------------------------------------------------------------------------
// Integration Tests - Replicator Concurrent Instances
//
// Author: Alex Freidah
//
// Pins the worker-level invariant that multiple Replicator instances
// racing on the same under-replicated batch never produce duplicate
// object_locations rows or double-count quota. The store-side safety
// gate is the ON CONFLICT DO NOTHING + WHERE source-exists clause in
// InsertReplicaConditional; this test exercises the full
// scan -> copy -> RecordReplica loop concurrently so a regression
// breaking either the SQL or the worker's outcome accounting fails CI.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/worker"
)

// newAuxReplicator builds an additional Replicator that shares the
// same testManager / testStore as the bundled testWorkers.Replicator
// but participates as a separate worker instance.
func newAuxReplicator() *worker.Replicator {
	return worker.NewReplicator(testManager, testStore)
}

// TestInt_Replicator_ConcurrentInstancesNoDoubleCopy enqueues several
// under-replicated objects, then races two Replicator instances on
// the same batch. Asserts the combined Created count equals the
// number of replicas needed (no double-write) and that each key ends
// at the target factor.
func TestInt_Replicator_ConcurrentInstancesNoDoubleCopy(t *testing.T) {
	resetState(t)

	const objects = 8
	const targetFactor = 2

	ctx := context.Background()
	keys := seedUnderReplicatedObjects(t, ctx, objects)

	replCfg := config.ReplicationConfig{
		Factor:         targetFactor,
		WorkerInterval: time.Minute,
		BatchSize:      50,
	}
	created := raceReplicators(t, ctx, replCfg, testWorkers.Replicator, newAuxReplicator())

	// Total across both replicators must equal N: any "duplicate insert
	// fell through" bug pushes this above N; lost work pushes below.
	if created != int64(objects) {
		t.Errorf("created across replicators = %d, want %d (over=double-insert, under=lost work)", created, objects)
	}
	assertEachAtFactor(t, keys, targetFactor)
}

// seedUnderReplicatedObjects PUTs N objects via the S3 client and
// returns their keys. Each ends up with a single copy so they're
// eligible for replication to targetFactor.
func seedUnderReplicatedObjects(t *testing.T, ctx context.Context, n int) []string {
	t.Helper()
	client := newS3Client(t)
	keys := make([]string, n)
	for i := range n {
		k := uniqueKey(t, fmt.Sprintf("repl-race-%d", i))
		keys[i] = k
		body := bytes.Repeat([]byte{byte('A' + i)}, 100)
		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(100),
		})
		if err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
		if copies := queryObjectCopies(t, k); copies != 1 {
			t.Fatalf("seed object %s: copies = %d, want 1", k, copies)
		}
	}
	return keys
}

// raceReplicators runs Replicate concurrently across the supplied
// replicators and returns the summed Created count.
func raceReplicators(t *testing.T, ctx context.Context, cfg config.ReplicationConfig, rs ...*worker.Replicator) int64 {
	t.Helper()
	var total atomic.Int64
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := r.Replicate(ctx, cfg)
			if err != nil {
				t.Errorf("Replicate: %v", err)
				return
			}
			total.Add(int64(created))
		}()
	}
	wg.Wait()
	return total.Load()
}

// assertEachAtFactor verifies every key ended at exactly factor copies
// on distinct backends.
func assertEachAtFactor(t *testing.T, keys []string, factor int) {
	t.Helper()
	for _, k := range keys {
		if copies := queryObjectCopies(t, k); copies != factor {
			t.Errorf("key %q final copies = %d, want %d", k, copies, factor)
		}
		backends := queryObjectBackends(t, k)
		if len(backends) != factor {
			t.Errorf("key %q backends = %v (len=%d), want %d", k, backends, len(backends), factor)
		}
		if hasDuplicate(backends) {
			t.Errorf("key %q has duplicate backend in %v", k, backends)
		}
	}
}

// hasDuplicate reports whether the slice contains any value twice.
func hasDuplicate(ss []string) bool {
	seen := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; ok {
			return true
		}
		seen[s] = struct{}{}
	}
	return false
}
