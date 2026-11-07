// -------------------------------------------------------------------------------
// Settling After the Response - Multipart Tests
//
// Author: Alex Freidah
//
// A client's abort is answered once its transaction commits, with the parts'
// bytes deleted behind the response. The stale-multipart sweep goes through the
// same code with no client waiting, so it settles before it moves on.
// -------------------------------------------------------------------------------

package multipart

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// TestAbort_SettlesBehindTheResponseForAClientOnly verifies an abort waits on
// the slow backend only when no client is waiting on it.
func TestAbort_SettlesBehindTheResponseForAClientOnly(t *testing.T) {
	const backendDelete = time.Second
	tests := map[string]struct {
		workload audit.Workload
		want     time.Duration
	}{
		"client":     {workload: audit.WorkloadClient},
		"background": {workload: audit.WorkloadCleanup, want: backendDelete},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := storetest.NewMockMetadataStore(gomock.NewController(t))
				store.EXPECT().GetMultipartUpload(gomock.Any(), "upload-1").
					Return(&core.MultipartUpload{UploadID: "upload-1", ObjectKey: "multi/key", BackendName: "b1"}, nil).AnyTimes()
				store.EXPECT().DeleteMultipartUpload(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					Return([]core.CleanupItem{{ID: 1, BackendName: "b1", ObjectKey: "multi/key", StorageKey: "part"}}, nil).AnyTimes()
				storetest.Permissive(store)

				be := backendtest.NewSlow(backendtest.NewInMemory(), backendDelete)
				be.DelayDeletes = true
				mgr := newFleet(t, store, map[string]backend.ObjectBackend{"b1": be}, nil)

				start := time.Now()
				ctx := audit.WithWorkload(context.Background(), tc.workload)
				if err := mgr.AbortMultipartUpload(ctx, "multi", "key", "upload-1"); err != nil {
					t.Fatal(err)
				}
				if took := time.Since(start); took != tc.want {
					t.Errorf("returned after %s, want %s", took, tc.want)
				}
				time.Sleep(2 * backendDelete)
			})
		})
	}
}
