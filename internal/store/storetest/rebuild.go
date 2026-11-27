// -------------------------------------------------------------------------------
// Factored Rebuild Check
//
// Author: Alex Freidah
//
// A rebuild of the replication queue holds each key to its bucket's factor and
// every other key to the deployment's. Both engines run this check against a
// real store, since each matches a key's bucket in its own SQL.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// ReplicationRebuilder is the store surface CheckFactoredRebuild drives,
// which both engines satisfy.
type ReplicationRebuilder interface {
	RecordObject(ctx context.Context, req *core.RecordObjectRequest) ([]core.CleanupItem, error)
	RebuildReplicationQueue(ctx context.Context, factors *core.ReplicationFactors, excluded []string) (int64, error)
	ListReplicationQueue(ctx context.Context, direction core.ReplicationDirection, limit int, excluded []string) ([]core.QueuedReplication, error)
}

// CheckFactoredRebuild records keys under buckets named from prefix, which
// nothing else may write under, and fails unless a rebuild at one copy, with
// one bucket kept at two, queues exactly the keys off their own factor. A
// bucket whose name only starts with the kept one's takes the deployment's.
func CheckFactoredRebuild(t *testing.T, s ReplicationRebuilder, prefix, backend, replica string) {
	t.Helper()
	ctx := context.Background()
	kept, other := prefix+"kept", prefix+"other"
	for key, backends := range map[string][]string{
		kept + "/short": {backend},
		kept + "/at":    {backend, replica},
		kept + "x/at":   {backend},
		other + "/over": {backend, replica},
		other + "/at":   {backend},
	} {
		copies := make([]core.ObjectCopy, len(backends))
		for i, b := range backends {
			copies[i] = core.ObjectCopy{Backend: b}
		}
		if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{Key: key, Size: 64, Copies: copies}); err != nil {
			t.Fatalf("RecordObject %s: %v", key, err)
		}
	}

	factors := core.ReplicationFactors{Default: 1, Buckets: map[string]int{kept: 2}}
	if _, err := s.RebuildReplicationQueue(ctx, &factors, nil); err != nil {
		t.Fatalf("RebuildReplicationQueue(%s): %v", factors.String(), err)
	}
	for side, want := range map[core.ReplicationDirection][]string{
		core.ReplicationShort: {kept + "/short"},
		core.ReplicationOver:  {other + "/over"},
	} {
		if got := queuedUnder(t, s, side, prefix); !slices.Equal(got, want) {
			t.Errorf("side %v queued %v, want %v", side, got, want)
		}
	}
}

// queuedUnder returns the keys under prefix queued on side, sorted.
func queuedUnder(t *testing.T, s ReplicationRebuilder, side core.ReplicationDirection, prefix string) []string {
	t.Helper()
	due, err := s.ListReplicationQueue(context.Background(), side, 100000, nil)
	if err != nil {
		t.Fatalf("ListReplicationQueue: %v", err)
	}
	var keys []string
	for _, q := range due {
		if strings.HasPrefix(q.Key, prefix) {
			keys = append(keys, q.Key)
		}
	}
	slices.Sort(keys)
	return keys
}
