// -------------------------------------------------------------------------------
// Replica Verification Tests
//
// Author: Alex Freidah
//
// Covers integrity.verify_on_replicate: the gate that decides whether a new copy
// is read back at all, the four verdicts a read-back can reach, and what
// ReplicateObject does with each. The load-bearing assertions are the negative
// ones - a copy that could not be checked is still recorded, and only a digest
// that actually disagreed is thrown away.
// -------------------------------------------------------------------------------

package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/backend/backendtest"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// verifyOnBody is the plaintext every verification test replicates, kept short
// so a mismatch is obvious in a failure message.
const verifyOnBody = "replicated payload"

// verifyingReplicator builds a replicator whose integrity config asks for
// replica verification, with a fleet that can afford any read. Returns the
// worker plus the mocks the caller stubs per scenario.
func verifyingReplicator(t *testing.T, cfg *config.IntegrityConfig) (*Replicator, *MockOps, *MockPlacement, *mockMetadataStore) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	pl := NewMockPlacement(ctrl)
	ms := &mockMetadataStore{}

	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	// A recorded replica inserts cleanly unless a test says otherwise, so
	// "was it recorded" is a question about verification and not about races.
	ms.recordReplicaOK = true
	ms.recordReplicaSize = int64(len(verifyOnBody))

	r := newTestReplicator(ops, pl, ms)
	r.SetIntegrityConfig(cfg)
	return r, ops, pl, ms
}

// sourceRow is the ledger row of the copy being replicated from.
func sourceRow(hash string) *core.ObjectLocation {
	return &core.ObjectLocation{
		ObjectKey:   "bucket/key1",
		BackendName: "b1",
		SizeBytes:   int64(len(verifyOnBody)),
		ContentHash: hash,
	}
}

// expectReadBack stubs the target-side GET that verification performs, serving
// body as the stored bytes.
func expectReadBack(ops *MockOps, target string, body []byte) {
	be := backendtest.NewMockObjectBackend(gomock.NewController(&testing.T{}))
	ops.EXPECT().GetBackend(target).Return(be, nil)
	ops.EXPECT().GetWithTimeout(gomock.Any(), be, "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(body)),
		Size: int64(len(body)),
	}, func() {}, nil)
}

// -------------------------------------------------------------------------
// THE GATE
// -------------------------------------------------------------------------

// TestVerifyReplica_GateTable pins which config combinations read a copy back.
// Verification costs a full extra read of every replica, so the default has to
// stay off: an operator who enables hashing has not thereby agreed to double
// what replication spends on egress.
func TestVerifyReplica_GateTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *config.IntegrityConfig
	}{
		{"no integrity config at all", nil},
		{"integrity off", &config.IntegrityConfig{Enabled: false, VerifyOnReplicate: true}},
		{"integrity on, verify off", &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: false}},
		{"integrity on, verify unset", &config.IntegrityConfig{Enabled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, ops, _, _ := verifyingReplicator(t, tc.cfg)
			// A gate that lets anything through would have to read the copy.
			ops.EXPECT().GetBackend(gomock.Any()).Times(0)
			ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

			got := r.verifyReplica(context.Background(), "b2", sourceRow(hashString(verifyOnBody)))
			if got != replicaNotChecked {
				t.Errorf("verdict = %v, want replicaNotChecked", got)
			}
		})
	}
}

// -------------------------------------------------------------------------
// THE VERDICTS
// -------------------------------------------------------------------------

// TestVerifyReplica_MatchingDigestVerifies is the happy path: the copy that
// landed hashes to what the source row recorded.
func TestVerifyReplica_MatchingDigestVerifies(t *testing.T) {
	t.Parallel()
	r, ops, _, _ := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})
	expectReadBack(ops, "b2", []byte(verifyOnBody))

	got := r.verifyReplica(context.Background(), "b2", sourceRow(hashString(verifyOnBody)))
	if got != replicaVerified {
		t.Errorf("verdict = %v, want replicaVerified", got)
	}
}

// TestVerifyReplica_DisagreeingDigestRejects is the only outcome that discards
// a copy, so it is the one that has to be exact.
//
// Deliberately not parallel: the assertion is a delta on a process-wide
// counter, and other tests in this package increment the same one.
func TestVerifyReplica_DisagreeingDigestRejects(t *testing.T) {
	r, ops, _, _ := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})
	expectReadBack(ops, "b2", []byte("corrupted on arrival"))

	before := promtest.ToFloat64(telemetry.IntegrityErrorsTotal.WithLabelValues(integrityOpReplicate))
	got := r.verifyReplica(context.Background(), "b2", sourceRow(hashString(verifyOnBody)))
	if got != replicaMismatch {
		t.Fatalf("verdict = %v, want replicaMismatch", got)
	}
	if after := promtest.ToFloat64(telemetry.IntegrityErrorsTotal.WithLabelValues(integrityOpReplicate)); after != before+1 {
		t.Errorf("integrity errors = %v, want %v", after, before+1)
	}
}

// TestVerifyReplica_NoStoredHashKeepsTheCopy covers a deployment that turned
// integrity on without running backfill. There is nothing to compare the copy
// against, and refusing to replicate on that basis would leave objects written
// before the setting permanently under-replicated.
func TestVerifyReplica_NoStoredHashKeepsTheCopy(t *testing.T) {
	t.Parallel()
	r, ops, _, _ := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})
	// Reading the copy would spend egress to produce a digest with nothing to
	// judge it by, so no read is attempted.
	ops.EXPECT().GetBackend(gomock.Any()).Times(0)
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	got := r.verifyReplica(context.Background(), "b2", sourceRow(""))
	if got != replicaUnverified {
		t.Errorf("verdict = %v, want replicaUnverified", got)
	}
}

// TestVerifyReplica_UnreadableCopyIsKept is the read-after-write case. A
// backend that has not yet made the new copy visible must not cost the object a
// replica: "cannot read" is not "corrupt", and treating it as one would spin
// through every backend in the fleet discarding good copies.
func TestVerifyReplica_UnreadableCopyIsKept(t *testing.T) {
	t.Parallel()
	r, ops, _, _ := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})
	be := backendtest.NewMockObjectBackend(gomock.NewController(t))
	ops.EXPECT().GetBackend("b2").Return(be, nil)
	ops.EXPECT().GetWithTimeout(gomock.Any(), be, "bucket/key1", "").
		Return(nil, nil, errors.New("not found yet"))

	got := r.verifyReplica(context.Background(), "b2", sourceRow(hashString(verifyOnBody)))
	if got != replicaUnverified {
		t.Errorf("verdict = %v, want replicaUnverified", got)
	}
}

// TestVerifyReplica_DeclinedByUsageLimitsKeepsTheCopy pins the budget arm: a
// target with no egress headroom left is not read, and the copy it holds is
// recorded unchecked rather than thrown away.
//
// Deliberately not parallel: the assertion is a delta on a process-wide
// counter, and the scrubber's own decline path increments the same one.
func TestVerifyReplica_DeclinedByUsageLimitsKeepsTheCopy(t *testing.T) {
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}

	tracker := counter.NewUsageTracker(counter.NewLocalCounterBackend([]string{"b1", "b2"}), nil)
	tracker.UpdateLimits(map[string]core.UsageLimits{"b2": {EgressByteLimit: 100}})
	tracker.SetBaseline("b2", core.UsageStat{EgressBytes: 99})
	ops.EXPECT().Usage().Return(tracker).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()
	ops.EXPECT().GetBackend(gomock.Any()).Times(0)
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	r := newTestReplicator(ops, NewMockPlacement(ctrl), ms)
	r.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})

	before := promtest.ToFloat64(telemetry.IntegrityUsageDeclinedTotal)
	got := r.verifyReplica(context.Background(), "b2", sourceRow(hashString(verifyOnBody)))
	if got != replicaUnverified {
		t.Fatalf("verdict = %v, want replicaUnverified", got)
	}
	if after := promtest.ToFloat64(telemetry.IntegrityUsageDeclinedTotal); after != before+1 {
		t.Errorf("usage declined = %v, want %v", after, before+1)
	}
}

// TestVerifyReplica_CompressedCopyIsDecodedFirst proves the replicator undoes
// the stored form the same way the scrubber does. Hashing the stored bytes
// instead of the plaintext would read as corruption on every compressed object
// in the fleet, which is the loudest possible way to get this wrong.
func TestVerifyReplica_CompressedCopyIsDecodedFirst(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	codec := newScrubCodec(t)
	plain, stored := encodeForScrub(t, codec)

	r := NewReplicator(ReplicatorDeps{
		Ops:       ops,
		Placement: NewMockPlacement(ctrl),
		Store:     ms,
		Codec:     codec,
	})
	r.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})

	source := compressedRow(hashString(string(plain)), len(stored))
	expectReadBack(ops, "b2", stored)

	if got := r.verifyReplica(context.Background(), "b2", &source); got != replicaVerified {
		t.Errorf("verdict = %v, want replicaVerified", got)
	}
}

// TestVerifyReplica_EncryptedCopyIsDecryptedFirst is the encryption half of the
// same contract as the compressed case: content_hash covers plaintext, so an
// envelope has to be opened before it can be compared against one.
func TestVerifyReplica_EncryptedCopyIsDecryptedFirst(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	ops := NewMockOps(ctrl)
	ms := &mockMetadataStore{}
	ops.EXPECT().Usage().Return(newTestUsageTracker()).AnyTimes()
	ops.EXPECT().Acct().Return(newTestRecorder()).AnyTimes()

	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	provider, err := encryption.NewConfigKeyProvider(key, "key-1")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(provider, 4096)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	res, err := enc.Encrypt(context.Background(), bytes.NewReader([]byte(verifyOnBody)), int64(len(verifyOnBody)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	r := NewReplicator(ReplicatorDeps{
		Ops:       ops,
		Placement: NewMockPlacement(ctrl),
		Store:     ms,
		Encryptor: enc,
	})
	r.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})

	source := &core.ObjectLocation{
		ObjectKey:     "bucket/key1",
		BackendName:   "b1",
		SizeBytes:     int64(len(ciphertext)),
		ContentHash:   hashString(verifyOnBody),
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(res.BaseNonce, res.WrappedDEK),
		KeyID:         res.KeyID,
		PlaintextSize: int64(len(verifyOnBody)),
	}
	expectReadBack(ops, "b2", ciphertext)

	if got := r.verifyReplica(context.Background(), "b2", source); got != replicaVerified {
		t.Errorf("verdict = %v, want replicaVerified", got)
	}
}

// TestReplicator_IntegrityConfigRoundTrips covers the accessor the reload hook
// and the verification gate both read.
func TestReplicator_IntegrityConfigRoundTrips(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	r := newTestReplicator(NewMockOps(ctrl), NewMockPlacement(ctrl), &mockMetadataStore{})

	if got := r.IntegrityConfig(); got != nil {
		t.Errorf("IntegrityConfig() = %+v, want nil before any config is pushed", got)
	}
	r.SetIntegrityConfig(&config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})
	if got := r.IntegrityConfig(); got == nil || !got.ShouldVerifyOnReplicate() {
		t.Errorf("IntegrityConfig() = %+v, want replica verification on", got)
	}
}

// TestVerifyReplica_CompressedCopyWithoutCodecIsKept covers an orchestrator
// that cannot decode what it is holding. That is a copy it cannot judge, not a
// copy it knows is bad.
func TestVerifyReplica_CompressedCopyWithoutCodecIsKept(t *testing.T) {
	t.Parallel()
	r, ops, _, _ := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})
	source := compressedRow(hashString(verifyOnBody), len(verifyOnBody))
	expectReadBack(ops, "b2", []byte(verifyOnBody))

	if got := r.verifyReplica(context.Background(), "b2", &source); got != replicaUnverified {
		t.Errorf("verdict = %v, want replicaUnverified", got)
	}
}

// -------------------------------------------------------------------------
// WHAT REPLICATEOBJECT DOES WITH A VERDICT
// -------------------------------------------------------------------------

// replicateOneWithVerification drives ReplicateObject for a single needed copy
// against target, with the stream copy stubbed to succeed and the target's
// read-back serving readBack.
func replicateOneWithVerification(t *testing.T, target string, readBack []byte, hash string) (*Replicator, ReplicationOutcome, *MockPlacement, *mockMetadataStore) {
	t.Helper()
	r, ops, pl, ms := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})

	srcBe := backendtest.NewMockObjectBackend(gomock.NewController(t))
	dstBe := backendtest.NewMockObjectBackend(gomock.NewController(t))
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, target: dstBe}).AnyTimes()
	ops.EXPECT().GetBackend(target).Return(dstBe, nil).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), gomock.Any(), gomock.Any(), "bucket/key1", gomock.Any()).
		Return(int64(0), nil)
	ops.EXPECT().GetWithTimeout(gomock.Any(), dstBe, "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(readBack)),
		Size: int64(len(readBack)),
	}, func() {}, nil)

	pl.EXPECT().SelectReplicaTarget(gomock.Any(), gomock.Any(), gomock.Any()).Return(target, nil)

	copies := []core.ObjectLocation{*sourceRow(hash)}
	out := r.ReplicateObject(context.Background(), "bucket/key1", copies, 1)
	return r, out, pl, ms
}

// TestReplicateObject_VerifiedCopyIsRecorded is the control: verification
// passing must leave the ordinary replication path untouched.
func TestReplicateObject_VerifiedCopyIsRecorded(t *testing.T) {
	t.Parallel()
	_, out, _, ms := replicateOneWithVerification(t, "b2", []byte(verifyOnBody), hashString(verifyOnBody))

	if out.Created != 1 {
		t.Errorf("created = %d, want 1", out.Created)
	}
	if out.VerifyMismatch != 0 || out.VerifyUnchecked != 0 {
		t.Errorf("mismatch=%d unchecked=%d, want 0/0", out.VerifyMismatch, out.VerifyUnchecked)
	}
	if ms.replicaRecorded != 1 {
		t.Errorf("RecordReplica calls = %d, want 1", ms.replicaRecorded)
	}
}

// TestReplicateObject_MismatchedCopyIsDiscardedNotRecorded is the reason the
// check runs before RecordReplica rather than after: a copy that disagrees with
// its source must never exist as a row, because a row is what makes it count
// toward the replication factor and stop the object being rebuilt.
func TestReplicateObject_MismatchedCopyIsDiscardedNotRecorded(t *testing.T) {
	t.Parallel()
	r, ops, pl, ms := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})

	srcBe := backendtest.NewMockObjectBackend(gomock.NewController(t))
	dstBe := backendtest.NewMockObjectBackend(gomock.NewController(t))
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), gomock.Any(), gomock.Any(), "bucket/key1", gomock.Any()).
		Return(int64(0), nil)
	ops.EXPECT().GetWithTimeout(gomock.Any(), dstBe, "bucket/key1", "").Return(&backend.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("wrong bytes"))),
		Size: 11,
	}, func() {}, nil)

	// Only one target exists, so the retry loop runs out rather than succeeding
	// elsewhere.
	pl.EXPECT().SelectReplicaTarget(gomock.Any(), gomock.Any(), gomock.Any()).Return("b2", nil)
	pl.EXPECT().SelectReplicaTarget(gomock.Any(), gomock.Any(), gomock.Any()).Return("", errors.New("no target")).AnyTimes()
	// The bytes that failed the check are removed from the target.
	pl.EXPECT().DeleteOrEnqueue(gomock.Any(), dstBe, "b2", "bucket/key1", "replication_orphan", gomock.Any())

	copies := []core.ObjectLocation{*sourceRow(hashString(verifyOnBody))}
	out := r.ReplicateObject(context.Background(), "bucket/key1", copies, 1)

	if out.Created != 0 {
		t.Errorf("created = %d, want 0", out.Created)
	}
	if out.VerifyMismatch != 1 {
		t.Errorf("VerifyMismatch = %d, want 1", out.VerifyMismatch)
	}
	if out.Failed() == 0 {
		t.Error("a discarded copy must count as a failed attempt")
	}
	if ms.replicaRecorded != 0 {
		t.Errorf("RecordReplica calls = %d, want 0 (the copy was rejected)", ms.replicaRecorded)
	}
}

// TestReplicateObject_UncheckedCopyIsStillRecorded pins the other half of the
// contract: a copy nothing could be proven about is a copy that still counts.
// Reporting it as unchecked is the honest outcome; refusing to record it would
// trade an unverified replica for no replica.
func TestReplicateObject_UncheckedCopyIsStillRecorded(t *testing.T) {
	t.Parallel()
	r, ops, pl, ms := verifyingReplicator(t, &config.IntegrityConfig{Enabled: true, VerifyOnReplicate: true})

	srcBe := backendtest.NewMockObjectBackend(gomock.NewController(t))
	dstBe := backendtest.NewMockObjectBackend(gomock.NewController(t))
	ops.EXPECT().Backends().Return(map[string]backend.ObjectBackend{"b1": srcBe, "b2": dstBe}).AnyTimes()
	ops.EXPECT().StreamCopy(gomock.Any(), gomock.Any(), gomock.Any(), "bucket/key1", gomock.Any()).
		Return(int64(0), nil)
	ops.EXPECT().GetBackend("b2").Return(dstBe, nil).AnyTimes()
	pl.EXPECT().SelectReplicaTarget(gomock.Any(), gomock.Any(), gomock.Any()).Return("b2", nil)
	// No read-back: the source carries no hash, so there is nothing to check.
	ops.EXPECT().GetWithTimeout(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	copies := []core.ObjectLocation{*sourceRow("")}
	out := r.ReplicateObject(context.Background(), "bucket/key1", copies, 1)

	if out.Created != 1 {
		t.Errorf("created = %d, want 1", out.Created)
	}
	if out.VerifyUnchecked != 1 {
		t.Errorf("VerifyUnchecked = %d, want 1", out.VerifyUnchecked)
	}
	if out.Failed() != 0 {
		t.Errorf("Failed() = %d, want 0 (unchecked is not failed)", out.Failed())
	}
	if ms.replicaRecorded != 1 {
		t.Errorf("RecordReplica calls = %d, want 1", ms.replicaRecorded)
	}
}
