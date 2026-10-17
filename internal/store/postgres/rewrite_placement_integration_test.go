// -------------------------------------------------------------------------------
// In-Place Rewrite Placement Integration Tests
//
// Author: Alex Freidah
//
// A bulk compression or encryption pass records its rewritten bytes as a
// placement back onto the copy's own backend. These pin what that placement
// does against a real PostgreSQL: the row is replaced at the new path with the
// new form and size, the content hash survives, the backend's bytes_used moves
// by exactly the growth, admission judges only that growth and still admits a
// cordoned backend, and a source a client replaced mid-pass records nothing.
// -------------------------------------------------------------------------------

//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/store/storetest"
)

// rewrittenSuffix is the path suffix a rewrite writes its new bytes at.
const rewrittenSuffix = "!rw"

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// recordPlain records one verbatim write of the fixture's key on its source
// backend at the path suffix names, carrying a content hash.
func (f *placementFixture) recordPlain(t *testing.T, suffix string, size int64) {
	t.Helper()
	if _, _, err := f.s.RecordObject(context.Background(), &core.RecordObjectRequest{
		Key: f.key, Size: size, Form: &core.StoredForm{ContentHash: "plain-hash"},
		Copies: []core.ObjectCopy{{Backend: f.src, StorageKey: f.key + suffix}},
	}); err != nil {
		t.Fatalf("RecordObject(%s): %v", suffix, err)
	}
}

// rewrite places source back onto its own backend at the rewritten path,
// carrying form and size.
func (f *placementFixture) rewrite(source *core.ObjectLocation, form *core.StoredForm, size int64) (int64, error) {
	return f.s.PlaceCopy(context.Background(), &core.Placement{
		Source: source, Target: source.BackendName, StorageKey: f.key + rewrittenSuffix,
		RemoveSource: true, Form: form, Size: size,
	})
}

// sealedForm is the envelope an encrypt pass records for 100 plaintext bytes.
func sealedForm() *core.StoredForm {
	return &core.StoredForm{Encrypted: true, EncryptionKey: []byte("dek"), KeyID: "kid-2", PlaintextSize: 100}
}

// flagSource sets flag on the fixture's source backend and clears it when the
// test ends.
func (f *placementFixture) flagSource(t *testing.T, flag core.BackendFlags) {
	t.Helper()
	if set, err := f.s.SetBackendFlag(context.Background(), f.src, flag); err != nil || !set {
		t.Fatalf("SetBackendFlag(%s) = %v, %v", flag, set, err)
	}
	t.Cleanup(func() { _, _ = f.s.ClearBackendState(context.Background(), f.src) })
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// TestPgRewrite_EncryptReplacesTheRow verifies an encrypt rewrite replaces the
// plain copy with an encrypted one at the new path, keeps its hash, and charges
// the backend the envelope's growth.
func TestPgRewrite_EncryptReplacesTheRow(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordPlain(t, firstWriteSuffix, 100)
	before := readBytesUsed(t, f.s, f.src)

	placed, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), sealedForm(), 200)
	if err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if placed != 200 {
		t.Errorf("placed = %d, want 200", placed)
	}

	got := readBackOne(t, f.s, f.key)
	if got.BackendName != f.src || got.StorageKey != f.key+rewrittenSuffix || got.SizeBytes != 200 {
		t.Errorf("copy = %s/%s at %d bytes, want %s/%s at 200", got.BackendName, got.StorageKey, got.SizeBytes, f.src, f.key+rewrittenSuffix)
	}
	want := sealedForm()
	want.ContentHash = "plain-hash"
	assertFormPreserved(t, got, want)
	if got.ObjectSize() != 100 {
		t.Errorf("ObjectSize = %d, want the 100 plaintext bytes", got.ObjectSize())
	}
	if delta := readBytesUsed(t, f.s, f.src) - before; delta != 100 {
		t.Errorf("bytes_used delta = %d, want +100", delta)
	}
}

// TestPgRewrite_DecryptCreditsTheEnvelope verifies a decrypt rewrite clears
// every envelope column and credits the backend the bytes the envelope cost.
func TestPgRewrite_DecryptCreditsTheEnvelope(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordWrite(t, firstWriteSuffix, 200)
	before := readBytesUsed(t, f.s, f.src)

	if _, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), &core.StoredForm{}, 100); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}

	got := readBackOne(t, f.s, f.key)
	assertFormPreserved(t, got, &core.StoredForm{ContentHash: fullyPopulatedForm().ContentHash})
	if delta := readBytesUsed(t, f.s, f.src) - before; delta != -100 {
		t.Errorf("bytes_used delta = %d, want -100", delta)
	}
}

// TestPgRewrite_ZeroGrowthLeavesTheCounter verifies a rewrite that lands on
// the same size leaves bytes_used where it was.
func TestPgRewrite_ZeroGrowthLeavesTheCounter(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordPlain(t, firstWriteSuffix, 100)
	before := readBytesUsed(t, f.s, f.src)

	if _, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), sealedForm(), 100); err != nil {
		t.Fatalf("PlaceCopy: %v", err)
	}
	if got := readBytesUsed(t, f.s, f.src); got != before {
		t.Errorf("bytes_used = %d, want %d", got, before)
	}
}

// TestPgRewrite_AdmissionJudgesOnlyTheGrowth verifies a full backend still
// takes a rewrite that shrinks the copy, and refuses one whose growth does not
// fit while leaving the row and its bytes as they were.
func TestPgRewrite_AdmissionJudgesOnlyTheGrowth(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	if err := f.s.SyncQuotaLimits(context.Background(), backendQuota(f.src, 150, f.dst)); err != nil {
		t.Fatalf("SyncQuotaLimits: %v", err)
	}
	f.recordWrite(t, firstWriteSuffix, 120)

	before := f.copies(t)
	_, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), sealedForm(), 200)
	f.assertDeclined(t, err, core.ErrTargetRefused, before, 120, 0)

	if _, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), sealedForm(), 140); err != nil {
		t.Fatalf("PlaceCopy of 20 bytes growth: %v", err)
	}
	if got := readBytesUsed(t, f.s, f.src); got != 140 {
		t.Errorf("bytes_used = %d, want 140", got)
	}
}

// TestPgRewrite_CordonedAdmitsDrainingRefuses verifies a cordoned backend
// still has its copies rewritten in place, while a draining one refuses.
func TestPgRewrite_CordonedAdmitsDrainingRefuses(t *testing.T) {
	t.Run("cordoned", func(t *testing.T) {
		f := newPlacementFixture(t, 1<<30)
		f.recordPlain(t, firstWriteSuffix, 100)
		f.flagSource(t, core.FlagCordoned)

		if _, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), sealedForm(), 200); err != nil {
			t.Fatalf("PlaceCopy on a cordoned backend: %v", err)
		}
		if got := readBackOne(t, f.s, f.key); !got.Encrypted {
			t.Error("the cordoned backend's copy was not rewritten")
		}
	})
	t.Run("draining", func(t *testing.T) {
		f := newPlacementFixture(t, 1<<30)
		f.recordPlain(t, firstWriteSuffix, 100)
		f.flagSource(t, core.FlagDraining)
		before := f.copies(t)

		_, err := f.rewrite(storetest.LocationOn(t, f.s, f.key, f.src), sealedForm(), 200)
		f.assertDeclined(t, err, core.ErrTargetRefused, before, 100, 0)
	})
}

// TestPgRewrite_ReplacedSourceIsCopyChanged verifies a rewrite whose source a
// client overwrote after the pass read it records nothing, so the newer write
// survives untouched.
func TestPgRewrite_ReplacedSourceIsCopyChanged(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordPlain(t, firstWriteSuffix, 100)
	read := storetest.LocationOn(t, f.s, f.key, f.src)
	f.recordPlain(t, secondWriteSuffix, 140)
	before := f.copies(t)

	_, err := f.rewrite(read, sealedForm(), 200)
	f.assertDeclined(t, err, core.ErrCopyChanged, before, 140, 0)
	if got := readBackOne(t, f.s, f.key); got.Encrypted || got.SizeBytes != 140 {
		t.Errorf("copy = %+v, want the client's plain 140 bytes", got)
	}
}

// TestPgRewrite_InPlaceWithoutRemoveIsRefused verifies a placement onto the
// source's own backend that keeps the source records nothing.
func TestPgRewrite_InPlaceWithoutRemoveIsRefused(t *testing.T) {
	f := newPlacementFixture(t, 1<<30)
	f.recordPlain(t, firstWriteSuffix, 100)
	before := f.copies(t)
	src := storetest.LocationOn(t, f.s, f.key, f.src)

	_, err := f.s.PlaceCopy(context.Background(), &core.Placement{
		Source: src, Target: f.src, StorageKey: f.key + rewrittenSuffix, Form: sealedForm(), Size: 200,
	})
	f.assertDeclined(t, err, core.ErrTargetRefused, before, 100, 0)
}

// TestPgBackendAdmits_AcceptMask verifies the accept mask decides which flags
// a backend may carry and still admit: a cordoned backend admits a rewrite but
// not a write, and a draining one admits neither.
func TestPgBackendAdmits_AcceptMask(t *testing.T) {
	s := adapterPgStore(t)
	ctx := context.Background()
	cases := []struct {
		flag                   core.BackendFlags
		wantWrite, wantRewrite bool
	}{
		{0, true, true},
		{core.FlagCordoned, false, true},
		{core.FlagDraining, false, false},
	}
	for _, tc := range cases {
		backend := drainBackend(t, s)
		if tc.flag != 0 {
			if set, err := s.SetBackendFlag(ctx, backend, tc.flag); err != nil || !set {
				t.Fatalf("SetBackendFlag(%s) = %v, %v", tc.flag, set, err)
			}
		}
		withPgAdapter(t, s, func(a *pgTxAdapter) {
			for accepts, want := range map[core.BackendFlags]bool{
				core.AcceptForWrite:   tc.wantWrite,
				core.AcceptForRewrite: tc.wantRewrite,
			} {
				got, err := a.BackendAdmits(ctx, backend, 1, accepts)
				if err != nil {
					t.Fatalf("BackendAdmits: %v", err)
				}
				if got != want {
					t.Errorf("flags %s, accepts %s: admits = %v, want %v", tc.flag, accepts, got, want)
				}
			}
		})
	}
}

// backendQuota is the quota set that gives src limit bytes and keeps dst
// unlimited for the test's lifetime.
func backendQuota(src string, limit int64, dst string) []config.BackendConfig {
	return []config.BackendConfig{{Name: src, QuotaBytes: limit}, {Name: dst, QuotaBytes: 1 << 30}}
}

// readBytesUsed returns the current bytes_used value for backendName.
func readBytesUsed(t *testing.T, s *Store, backendName string) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT GREATEST(0, COALESCE(SUM(bytes_used), 0)) FROM backend_quota_stripes WHERE backend_name = $1`,
		backendName,
	).Scan(&v); err != nil {
		t.Fatalf("read bytes_used: %v", err)
	}
	return v
}

// resetBytesUsed zeroes the bytes_used column on a backend so each test
// starts from a known baseline regardless of which other tests in the
// shared fixture have already run.
func resetBytesUsed(t *testing.T, s *Store, backendName string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`DELETE FROM backend_quota_stripes WHERE backend_name = $1`, backendName,
	); err != nil {
		t.Fatalf("reset bytes_used: %v", err)
	}
}
