// -------------------------------------------------------------------------------
// Promotion Tag Check
//
// Author: Alex Freidah
//
// A write whose metadata commit fails is finished by the pending reaper, and
// the object it promotes has to carry the tags that write asked for. The check
// runs against both engines, so the tag column, its round trip through the
// stale scan and the promotion that applies it are held to one behavior.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// PromotionTagger is the slice of a store CheckPromotedTags drives.
type PromotionTagger interface {
	RecordObject(ctx context.Context, req *core.RecordObjectRequest) ([]core.CleanupItem, error)
	InsertPendingIfFits(ctx context.Context, p *core.PendingObject, accepts core.BackendFlags) (bool, error)
	GetStalePending(ctx context.Context, olderThan time.Time, limit int) ([]core.PendingObject, error)
	PromotePending(ctx context.Context, p *core.PendingObject) (core.PendingPromoteResult, []core.CleanupItem, error)
	GetObjectTags(ctx context.Context, key string) ([]core.Tag, error)
}

// CheckPromotedTags records a tagged object on backend, leaves an overwrite of
// it as a pending intent carrying a different tag set, and fails unless the
// stale scan returns that set and promoting the intent replaces the object's
// tags with it.
func CheckPromotedTags(t *testing.T, s PromotionTagger, backend string) {
	t.Helper()
	ctx := context.Background()
	key := "bucket/" + t.Name()
	if _, err := s.RecordObject(ctx, &core.RecordObjectRequest{
		Key: key, Size: 10, Tags: []core.Tag{{Key: "old", Value: "1"}},
		Copies: []core.ObjectCopy{{Backend: backend, StorageKey: key + "!old"}},
	}); err != nil {
		t.Fatalf("RecordObject: %v", err)
	}

	want := []core.Tag{{Key: "a", Value: "x"}, {Key: "b", Value: "y"}}
	intent := &core.PendingObject{
		IntentID: t.Name() + "-overwrite", ObjectKey: key, StorageKey: key + "!new",
		BackendName: backend, SizeBytes: 10, Tags: want,
	}
	if ok, err := s.InsertPendingIfFits(ctx, intent, core.AcceptForWrite); err != nil || !ok {
		t.Fatalf("InsertPendingIfFits = %v, %v", ok, err)
	}

	stale, err := s.GetStalePending(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("GetStalePending: %v", err)
	}
	i := slices.IndexFunc(stale, func(p core.PendingObject) bool { return p.IntentID == intent.IntentID })
	if i < 0 {
		t.Fatal("stale scan did not return the intent")
	}
	if !slices.Equal(stale[i].Tags, want) {
		t.Errorf("stale intent tags = %v, want %v", stale[i].Tags, want)
	}

	if result, _, err := s.PromotePending(ctx, &stale[i]); err != nil || result != core.PendingPromoteCommitted {
		t.Fatalf("PromotePending = %v, %v; want committed", result, err)
	}
	got, err := s.GetObjectTags(ctx, key)
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("promoted object tags = %v, want the write's %v, not the replaced object's", got, want)
	}
}
