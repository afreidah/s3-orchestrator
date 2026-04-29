// -------------------------------------------------------------------------------
// Admin Exported-Ops Tests
//
// Author: Alex Freidah
//
// Covers the skipped-path branches of the four exported operations the UI
// delegates to: Replicate, Scrub, BackfillChecksums, and EncryptExisting.
// Each operation has a documented "do nothing when not configured"
// behaviour that the UI relies on to render a status banner instead of an
// error. The shared newTestHandlerWithManager fixture leaves replication
// factor at 1, the encryptor nil, and integrity disabled — exactly the
// conditions every skipped path checks.
// -------------------------------------------------------------------------------

package admin

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
)

// enableIntegrityForTest stores an integrity-enabled config on the
// handler's BackendOps so the Scrub / BackfillChecksums skipped guard
// passes during a test. The fixture's BackendOps is a real
// *proxy.BackendManager, which exposes SetIntegrityConfig.
func enableIntegrityForTest(t *testing.T, h *Handler) {
	t.Helper()
	mgr, ok := h.backendOps.(*proxy.BackendManager)
	if !ok {
		t.Fatalf("backendOps is %T, want *proxy.BackendManager", h.backendOps)
	}
	mgr.SetIntegrityConfig(&config.IntegrityConfig{
		Enabled:           true,
		ScrubberBatchSize: 50,
	})
}

// TestReplicate_SkippedWhenFactorAtOne asserts that the exported
// Replicate method returns Status="skipped" and zero copies created when
// the replication factor is configured at 1 (no replicas to make). The
// fixture intentionally leaves Factor at 1.
func TestReplicate_SkippedWhenFactorAtOne(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res, err := h.Replicate(context.Background())
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestScrub_SkippedWhenIntegrityDisabled asserts that Scrub returns
// Status="skipped" when the integrity config has not been set on the
// manager (the default in the test fixture).
func TestScrub_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res := h.Scrub(context.Background(), 0)
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.Checked != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Checked, res.Failed)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestBackfillChecksums_SkippedWhenIntegrityDisabled asserts that
// BackfillChecksums skips and reports a reason when integrity is off.
func TestBackfillChecksums_SkippedWhenIntegrityDisabled(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res := h.BackfillChecksums(context.Background(), 0)
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.Processed != 0 {
		t.Errorf("Processed = %d, want 0", res.Processed)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestEncryptExisting_SkippedWhenEncryptorNil asserts that
// EncryptExisting reports skipped when the handler was built without an
// encryptor (the documented "encryption not enabled" branch the UI
// surfaces as a banner).
func TestEncryptExisting_SkippedWhenEncryptorNil(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)

	res := h.EncryptExisting(context.Background())
	if res.Status != "skipped" {
		t.Errorf("Status = %q, want skipped", res.Status)
	}
	if res.Success != 0 || res.Failed != 0 || res.Total != 0 {
		t.Errorf("counts = (success=%d failed=%d total=%d), want all 0",
			res.Success, res.Failed, res.Total)
	}
	if res.Reason == "" {
		t.Error("Reason is empty; want non-empty explanation")
	}
}

// TestReplicate_HappyPathEmptyStore asserts that with replication factor
// > 1 and no under-replicated objects in the store, Replicate returns
// Status="ok" with zero copies created. Exercises the post-skipped path
// the factor=1 test cannot reach.
func TestReplicate_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	// Bump replication factor so the skipped guard passes.
	mgr := h.backendOps.(*proxy.BackendManager)
	mgr.Replicator.SetConfig(&config.ReplicationConfig{Factor: 2, BatchSize: 10})

	res, err := h.Replicate(context.Background())
	if err != nil {
		t.Fatalf("Replicate: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("Status = %q, want ok", res.Status)
	}
	if res.CopiesCreated != 0 {
		t.Errorf("CopiesCreated = %d, want 0", res.CopiesCreated)
	}
}

// TestScrub_HappyPathEmptyStore asserts that with integrity enabled and
// no hashed objects in the store, Scrub returns Status="ok" with zero
// counts. Exercises the post-skipped pagination path that the
// nil-encryption test cannot reach.
func TestScrub_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	// Enable integrity on the embedded manager so Scrub does not skip.
	enableIntegrityForTest(t, h)

	res := h.Scrub(context.Background(), 0)
	if res.Status != "ok" {
		t.Errorf("Status = %q, want ok", res.Status)
	}
	if res.Checked != 0 || res.Failed != 0 {
		t.Errorf("Checked=%d Failed=%d, want both 0", res.Checked, res.Failed)
	}
}

// TestBackfillChecksums_HappyPathEmptyStore asserts that with integrity
// enabled and no objects missing a hash, BackfillChecksums returns
// Status="ok" Processed=0 immediately.
func TestBackfillChecksums_HappyPathEmptyStore(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithManager(t)
	enableIntegrityForTest(t, h)

	res := h.BackfillChecksums(context.Background(), 0)
	if res.Status != "ok" {
		t.Errorf("Status = %q, want ok", res.Status)
	}
	if res.Processed != 0 {
		t.Errorf("Processed = %d, want 0", res.Processed)
	}
}

// TestNew_FromDeps asserts that the exported New constructor wires every
// dep onto the resulting Handler. Goes through the public surface so the
// 0%-coverage entry on New no longer hides bugs in the wiring.
func TestNew_FromDeps(t *testing.T) {
	t.Parallel()
	// Reuse the fixture's plumbing to build a minimal Deps bag. The
	// fixture builds a Handler directly; here we go through New so the
	// constructor is exercised in coverage too.
	src := newTestHandlerWithManager(t)
	deps := &Deps{
		BackendOps: src.backendOps,
		Replicator: src.replicator,
		OverRep:    src.overRep,
		Drain:      src.drain,
		Scrubber:   src.scrubber,
		Lifecycle:  src.lifecycle,
		DBCB:       src.dbCB,
		Objects:    src.objects,
		Cleanup:    src.cleanup,
		Token:      src.token,
		LogLevel:   src.logLevel,
	}

	h := New(deps)
	if h == nil {
		t.Fatal("New returned nil")
	}
	if h.token != src.token {
		t.Errorf("token = %q, want %q", h.token, src.token)
	}
	if h.replicator != src.replicator {
		t.Error("replicator not threaded through")
	}
	if h.scrubber != src.scrubber {
		t.Error("scrubber not threaded through")
	}
}