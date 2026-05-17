// -------------------------------------------------------------------------------
// Write Coordinator - Branch Tests
//
// Author: Alex Freidah
//
// Targeted tests for the branches of Coordinator that the existing
// PUT/multipart end-to-end tests do not exercise: encryption metadata
// copy on InsertPendingIntent, InsertPending error propagation, and the
// "backend not registered" branch of RecordObjectAndPromoteIntent when
// intentID is empty.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/mock/gomock"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/proxy/infra"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// newCoordinatorWithStore builds a minimal Coordinator backed by the
// supplied store. Avoids the BackendManager constructor so coordinator
// branches can be exercised in isolation without dragging the full
// manager assembly into every test.
func newCoordinatorWithStore(store core.MetadataStore, pendingEnabled bool) *Coordinator {
	c := infra.New(&infra.Config{
		Backends: map[string]s3be.ObjectBackend{},
	})
	return New(c, store, pendingEnabled)
}

// TestInsertPendingIntent_CopiesEncryptionMeta drives the enc != nil
// branch so the PendingObject is populated with the wrapped DEK,
// keyID, plaintext size, and content hash.
func TestInsertPendingIntent_CopiesEncryptionMeta(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)

	var got core.PendingObject
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, p *core.PendingObject) error {
			got = *p
			return nil
		}).Times(1)
	storetest.Permissive(store)

	coord := newCoordinatorWithStore(store, true)
	enc := &core.EncryptionMeta{
		Encrypted:     true,
		EncryptionKey: []byte("wrapped-dek-bytes"),
		KeyID:         "kid-1",
		PlaintextSize: 4096,
		ContentHash:   "deadbeef",
	}

	intentID, err := coord.InsertPendingIntent(context.Background(), "k", "b1", 4096, enc)
	if err != nil {
		t.Fatalf("InsertPendingIntent: %v", err)
	}
	if intentID == "" {
		t.Fatal("expected non-empty intentID")
	}
	if !got.Encrypted || got.KeyID != "kid-1" || got.PlaintextSize != 4096 || got.ContentHash != "deadbeef" {
		t.Errorf("encryption metadata not copied onto PendingObject: %+v", got)
	}
	if string(got.EncryptionKey) != "wrapped-dek-bytes" {
		t.Errorf("EncryptionKey not copied: %q", got.EncryptionKey)
	}
}

// TestInsertPendingIntent_StoreError covers the InsertPending failure
// branch: the wrapped error is returned and the intent ID is empty.
func TestInsertPendingIntent_StoreError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	store.EXPECT().InsertPending(gomock.Any(), gomock.Any()).
		Return(errors.New("db down")).Times(1)
	storetest.Permissive(store)

	coord := newCoordinatorWithStore(store, true)

	intentID, err := coord.InsertPendingIntent(context.Background(), "k", "b1", 4096, nil)
	if err == nil {
		t.Fatal("expected error from InsertPending failure")
	}
	if intentID != "" {
		t.Errorf("expected empty intentID on error, got %q", intentID)
	}
}

// TestRecordObjectAndPromoteIntent_UnknownBackend covers the legacy
// fallback path's "backend not registered" branch: with intentID empty
// and an unknown backend name, the method returns an error before any
// store call.
func TestRecordObjectAndPromoteIntent_UnknownBackend(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	store := storetest.NewMockMetadataStore(ctrl)
	storetest.Permissive(store)

	coord := newCoordinatorWithStore(store, false)

	tracer := noop.NewTracerProvider().Tracer("test")
	_, sp := tracer.Start(context.Background(), "test")
	defer sp.End()

	err := coord.RecordObjectAndPromoteIntent(context.Background(), sp, "k", "no-such-backend", 1024, nil, "")
	if err == nil {
		t.Fatal("expected error for unregistered backend")
	}
}
