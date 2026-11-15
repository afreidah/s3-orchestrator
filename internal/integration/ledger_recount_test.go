// -------------------------------------------------------------------------------
// Integration Tests - Ledger Recount
//
// Author: Alex Freidah
//
// Runs a seeded mix of writes, overwrites, deletes and multipart uploads against
// one orchestrator while faults hit its backends and its store, lets every
// worker finish what was left, and checks the end state against a recount: each
// key at the factor with every copy holding a body some operation wrote, quota
// equal to the ledger, each bucket holding exactly what the ledger names, and
// nothing left queued. The same checks follow a factor change and a drain. A
// failure logs the seed that replays it.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/internalkey"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// The workload's shape: the keys it writes, the operations each half runs, one
// operation in recountFaultEvery carrying a fault, the operations a backend
// outage lasts, the settle rounds a phase gets, and the largest body written.
const (
	recountKeys       = 40
	recountOps        = 100
	recountFaultEvery = 5
	recountOutageOps  = 15
	recountMaxRounds  = 15
	recountMaxBody    = 4096
	recountSeedEnv    = "LEDGER_RECOUNT_SEED"
)

// recountRun is one run of the workload: the harness and the client that drive
// it, the seeded source every choice comes from, what the test knows each key
// may hold and what was done to it, the faults in force, and the factor the
// store's writes queue keys against.
type recountRun struct {
	t       *testing.T
	h       *harness
	client  *s3.Client
	seed    uint64
	rng     *rand.Rand
	seq     int
	model   map[string][][]byte
	history map[string][]string
	down    string
	drained string
	fault   string
	fired   map[string]int
	factor  atomic.Int64
}

// recountFault is one failure the workload arms around an operation. arm
// installs it for the key the operation touches and returns what clears it.
type recountFault struct {
	name string
	arm  func(r *recountRun, key string) func()
}

// putFaults are the failures a write can meet: on one backend or all of them,
// before or after the bytes land, and in the commits that record the copies.
var putFaults = []recountFault{
	{"one copy refused", func(r *recountRun, key string) func() {
		return r.onBackends([]string{r.pickBackend()}, copiesOf(http.MethodPut, key), proxyRefuse)
	}},
	{"one copy's reply lost", func(r *recountRun, key string) func() {
		return r.onBackends([]string{r.pickBackend()}, copiesOf(http.MethodPut, key), proxyLoseReply)
	}},
	{"every copy refused", func(r *recountRun, key string) func() {
		return r.onBackends(r.h.order, copiesOf(http.MethodPut, key), proxyRefuse)
	}},
	{"record fails", func(r *recountRun, key string) func() { return r.onStore(opRecordObject, key) }},
	{"companion commit fails", func(r *recountRun, key string) func() { return r.onStore(opCommitCompanionCopy, key) }},
}

// deleteFaults are the failures a delete can meet: in the commit that removes
// the copies, and on the backends whose bytes it then deletes.
var deleteFaults = []recountFault{
	{"delete fails", func(r *recountRun, key string) func() { return r.onStore(opDeleteObject, key) }},
	{"backend delete refused", func(r *recountRun, key string) func() {
		return r.onBackends(r.h.order, copiesOf(http.MethodDelete, key), proxyRefuse)
	}},
}

// TestLedgerRecount_MixedWorkloadWithFaults fails when any path leaves a key off
// the factor, a copy with the wrong bytes, quota apart from the ledger, bytes
// no copy names, or work queued that the workers never finish.
func TestLedgerRecount_MixedWorkloadWithFaults(t *testing.T) {
	r := newRecountRun(t)

	r.run(recountOps)
	r.outage(recountOutageOps)
	r.run(recountOps)
	r.settle(2)
	r.check("after the workload", 2)

	r.settle(3)
	r.check("after raising the factor to 3", 3)
	r.settle(2)
	r.check("after lowering the factor to 2", 2)

	r.drain()
	r.settle(2)
	r.check("after draining "+r.drained, 2)

	t.Logf("faults fired: %v", r.fired)
}

// newRecountRun builds a three-backend fleet that places two copies per write,
// seeded from recountSeedEnv when it is set so a failure can be replayed.
func newRecountRun(t *testing.T) *recountRun {
	t.Helper()
	seed := uint64(time.Now().UnixNano())
	if v := os.Getenv(recountSeedEnv); v != "" {
		parsed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q: %v", recountSeedEnv, v, err)
		}
		seed = parsed
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("replay with %s=%d", recountSeedEnv, seed)
		}
	})

	h := newHarness(t, harnessSpec{
		Backends: []harnessBackend{
			{Name: "lr-minio-1", Quota: 256 << 20},
			{Name: "lr-minio-2", Quota: 256 << 20},
			{Name: "lr-minio-3", Quota: 256 << 20},
		},
		Routing:        config.RoutingSpread,
		CopiesPerWrite: 2,
	})
	// One attempt per request: a retry would turn most armed faults into a
	// success the test never sees fail.
	client := s3.New(h.client.Options(), func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	r := &recountRun{
		t: t, h: h, client: client, seed: seed, rng: rand.New(rand.NewPCG(seed, seed)),
		model: map[string][][]byte{}, history: map[string][]string{}, fired: map[string]int{},
	}
	r.factor.Store(2)
	h.store.UseReplicationFactor(func() int { return int(r.factor.Load()) })
	return r
}

// -------------------------------------------------------------------------
// WORKLOAD
// -------------------------------------------------------------------------

// run performs n operations, each picked at random.
func (r *recountRun) run(n int) {
	for range n {
		r.step()
	}
}

// outage performs n operations while one backend refuses every request, then
// brings it back.
func (r *recountRun) outage(n int) {
	r.down = r.pickBackend()
	r.h.proxies[r.down].SetRule(func(*http.Request) proxyAction { return proxyRefuse })
	r.run(n)
	r.h.proxies[r.down].SetRule(nil)
	r.down = ""
}

// step performs one operation on a random key: mostly writes, then deletes,
// multipart uploads completed and abandoned, and reads checked against the model.
func (r *recountRun) step() {
	key := fmt.Sprintf("lr/%03d", r.rng.IntN(recountKeys))
	switch n := r.rng.IntN(100); {
	case n < 50:
		r.faulted(putFaults, key, func() { r.put(key) })
	case n < 62:
		r.faulted(deleteFaults, key, func() { r.delete(key) })
	case n < 68:
		r.bulkDelete()
	case n < 76:
		r.multipart(key)
	case n < 80:
		r.abandonUpload(key)
	default:
		r.read(key)
	}
	r.waitForTails()
}

// faulted runs op, now and then with one of faults armed for key. No fault is
// armed during an outage, whose rule an armed one would replace.
func (r *recountRun) faulted(faults []recountFault, key string, op func()) {
	if r.down != "" || r.rng.IntN(recountFaultEvery) != 0 {
		op()
		return
	}
	f := faults[r.rng.IntN(len(faults))]
	r.fired[f.name]++
	disarm := f.arm(r, key)
	r.fault = f.name
	op()
	r.waitForTails()
	r.fault = ""
	disarm()
}

// put writes a fresh body to key.
func (r *recountRun) put(key string) {
	body := r.body(key)
	_, err := r.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key),
		Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	})
	r.result("PUT", key, body, err)
}

// delete removes key.
func (r *recountRun) delete(key string) {
	_, err := r.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key),
	})
	r.result("DELETE", key, nil, err)
}

// bulkDelete removes a few random keys in one DeleteObjects request, taking
// each key's outcome from the response.
func (r *recountRun) bulkDelete() {
	var keys []string
	for range 2 + r.rng.IntN(4) {
		if key := fmt.Sprintf("lr/%03d", r.rng.IntN(recountKeys)); !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	ids := make([]types.ObjectIdentifier, len(keys))
	for i, key := range keys {
		ids[i] = types.ObjectIdentifier{Key: aws.String(key)}
	}
	out, err := r.client.DeleteObjects(context.Background(), &s3.DeleteObjectsInput{
		Bucket: aws.String(virtualBucket), Delete: &types.Delete{Objects: ids},
	})
	failed := map[string]error{}
	if out != nil {
		for _, e := range out.Errors {
			failed[aws.ToString(e.Key)] = fmt.Errorf("%s: %s", aws.ToString(e.Code), aws.ToString(e.Message))
		}
	}
	for _, key := range keys {
		r.result("DeleteObjects", key, nil, cmp.Or(err, failed[key]))
	}
}

// multipart writes a fresh body to key as a one-part multipart upload.
func (r *recountRun) multipart(key string) {
	body := r.body(key)
	ctx := context.Background()
	uploadID, etag, err := r.openUpload(key, body)
	if err == nil {
		_, err = r.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(virtualBucket), Key: aws.String(key), UploadId: uploadID,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: etag, PartNumber: aws.Int32(1)}}},
		})
	}
	r.result("multipart upload", key, body, err)
}

// abandonUpload opens a multipart upload under key with one part and never
// completes it, leaving it for the stale-upload sweep.
func (r *recountRun) abandonUpload(key string) {
	body := r.body(key)
	_, _, err := r.openUpload(key, body)
	r.note(key, "abandoned upload "+label(body), err)
	if err != nil && !r.excused() {
		r.keyErrorf(key, "abandoned upload of %s failed with no fault armed: %v", key, err)
	}
}

// openUpload creates a multipart upload under key and uploads body as its first
// part, returning the upload id and the part's ETag.
func (r *recountRun) openUpload(key string, body []byte) (*string, *string, error) {
	ctx := context.Background()
	created, err := r.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key),
	})
	if err != nil {
		return nil, nil, err
	}
	part, err := r.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key), UploadId: created.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return created.UploadId, nil, err
	}
	return created.UploadId, part.ETag, nil
}

// read GETs key and requires a body some operation on it wrote, or a 404 where
// the key may hold nothing. A failed read is excused only during an outage.
func (r *recountRun) read(key string) {
	out, err := r.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket), Key: aws.String(key),
	})
	var got []byte
	if _, missing := errors.AsType[*types.NoSuchKey](err); missing {
		err = nil
	} else if err == nil {
		got, err = io.ReadAll(out.Body)
		_ = out.Body.Close()
	}
	r.note(key, "GET -> "+label(got), err)
	if err != nil {
		if !r.excused() {
			r.keyErrorf(key, "GET %s failed with no fault armed: %v", key, err)
		}
		return
	}
	if !r.allows(key, got) {
		r.keyErrorf(key, "GET %s returned %s, which no operation on it wrote", key, describe(got))
	}
}

// waitForTails blocks until every copy still uploading after its write was
// answered has settled, so a fault is never cleared under a copy it was armed for.
func (r *recountRun) waitForTails() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if left := r.h.stack.Detached.Wait(ctx); left > 0 {
		r.t.Fatalf("%d copies still uploading a minute after their writes were answered", left)
	}
}

// -------------------------------------------------------------------------
// FAULTS
// -------------------------------------------------------------------------

// onBackends makes the named backends answer matching requests with action,
// returning what puts them back to passing every request.
func (r *recountRun) onBackends(names []string, match func(*http.Request) bool, action proxyAction) func() {
	rule := func(req *http.Request) proxyAction {
		if match(req) {
			return action
		}
		return proxyPass
	}
	for _, name := range names {
		r.h.proxies[name].SetRule(rule)
	}
	return func() {
		for _, name := range names {
			r.h.proxies[name].SetRule(nil)
		}
	}
}

// onStore fails the next call to op on key, returning what clears the fault
// whether or not it fired.
func (r *recountRun) onStore(op, key string) func() {
	r.h.faults.SetFault(failOnce(op, internalKey(key), errSimulatedCommitFailure))
	return func() { r.h.faults.SetFault(nil) }
}

// copiesOf matches requests of method for any copy of key, the key as the
// client wrote it.
func copiesOf(method, key string) func(*http.Request) bool {
	prefix := internalkey.CopyPrefix(internalKey(key))
	return func(req *http.Request) bool {
		return req.Method == method && strings.Contains(req.URL.Path, prefix)
	}
}

// pickBackend returns a random backend that has not been drained.
func (r *recountRun) pickBackend() string {
	active := r.active()
	return active[r.rng.IntN(len(active))]
}

// active returns the backends a copy may be on.
func (r *recountRun) active() []string {
	return slices.DeleteFunc(slices.Clone(r.h.order), func(name string) bool { return name == r.drained })
}

// excused reports whether a failed request is expected, because a fault is
// armed or a backend is down.
func (r *recountRun) excused() bool {
	return r.fault != "" || r.down != ""
}

// -------------------------------------------------------------------------
// MODEL
// -------------------------------------------------------------------------

// body returns a fresh payload for key, unique to this write so a copy left by
// an older one cannot pass for it.
func (r *recountRun) body(key string) []byte {
	r.seq++
	b := fmt.Appendf(nil, "%s#%d\n", key, r.seq)
	for range r.rng.IntN(recountMaxBody) {
		b = append(b, byte('a'+r.rng.IntN(26)))
	}
	return b
}

// result records what an operation left key holding: body alone when it
// succeeded, and body beside the earlier outcomes when it failed partway.
func (r *recountRun) result(op, key string, body []byte, err error) {
	r.note(key, op+" "+label(body), err)
	if err == nil {
		r.model[key] = [][]byte{body}
		return
	}
	if !r.excused() {
		r.keyErrorf(key, "%s %s failed with no fault armed: %v", op, key, err)
	}
	r.model[key] = append(r.outcomes(key), body)
}

// note adds one event to key's history, with the fault or outage in force and
// how the request ended.
func (r *recountRun) note(key, event string, err error) {
	if r.fault != "" {
		event += " [" + r.fault + "]"
	}
	if r.down != "" {
		event += " [" + r.down + " down]"
	}
	if err != nil {
		event += " failed: " + err.Error()
	}
	r.history[key] = append(r.history[key], event)
}

// keyErrorf fails the test with a message about key followed by everything done
// to key so far, so a failure reads on its own.
func (r *recountRun) keyErrorf(key, format string, args ...any) {
	r.t.Helper()
	r.t.Errorf("%s\n    history of %s:\n      %s", fmt.Sprintf(format, args...), key, strings.Join(r.history[key], "\n      "))
}

// label names a body in a history by its write number, or as no object.
func label(body []byte) string {
	if body == nil {
		return "(none)"
	}
	header, _, _ := bytes.Cut(body, []byte("\n"))
	_, n, _ := bytes.Cut(header, []byte("#"))
	return "#" + string(n)
}

// outcomes returns the bodies key may hold, nil standing for no object. A key
// never written holds nothing.
func (r *recountRun) outcomes(key string) [][]byte {
	if o, ok := r.model[key]; ok {
		return o
	}
	return [][]byte{nil}
}

// allows reports whether key may hold body.
func (r *recountRun) allows(key string, body []byte) bool {
	return slices.ContainsFunc(r.outcomes(key), func(o []byte) bool { return bytes.Equal(o, body) })
}

// describe names a body in a failure message by the header every write starts
// with, or as no object.
func describe(body []byte) string {
	if body == nil {
		return "no object"
	}
	header, _, _ := bytes.Cut(body, []byte("\n"))
	return fmt.Sprintf("%q (%d bytes)", header, len(body))
}

// -------------------------------------------------------------------------
// SETTLING
// -------------------------------------------------------------------------

// settle runs the workers that finish leftover work, as their ticks would given
// time, until a round changes nothing and nothing is queued. The store's writes
// queue keys against factor from here on, as a config reload would set it. The
// first round runs with one backend refusing every request and one promote
// failing.
func (r *recountRun) settle(factor int) {
	r.t.Helper()
	r.factor.Store(int64(factor))
	flaky := r.pickBackend()
	r.h.proxies[flaky].SetRule(func(*http.Request) proxyAction { return proxyRefuse })
	r.h.faults.SetFault(failOnce(opPromotePending, "", errSimulatedCommitFailure))
	_ = r.round(factor)
	r.h.faults.SetFault(nil)
	r.h.proxies[flaky].SetRule(nil)

	for range recountMaxRounds {
		before := r.fingerprint()
		if err := r.round(factor); err != nil {
			r.t.Errorf("settling at factor %d: %v", factor, err)
		}
		if r.fingerprint() == before && r.leftover() == 0 {
			return
		}
	}
	r.t.Errorf("still changing or queued after %d settle rounds at factor %d: %s", recountMaxRounds, factor, r.fingerprint())
}

// round runs each worker once. Intents are aged past the reaper's minimum and
// cleanup and replication rows made due, which is all the waiting their ticks
// would do.
func (r *recountRun) round(factor int) error {
	ctx := context.Background()
	r.exec("UPDATE pending_objects SET created_at = NOW() - INTERVAL '1 hour'")
	r.exec("UPDATE cleanup_queue SET next_retry = NOW(), claimed_at = NULL")
	r.exec("UPDATE replication_queue SET next_retry = NOW()")
	cfg := config.ReplicationConfig{
		Factor: factor, WorkerInterval: time.Minute, BatchSize: 500, Concurrency: 1,
		UnhealthyThreshold: 10 * time.Minute,
	}
	_, reapErr := r.h.workers.PendingReaper.ProcessPendingQueue(ctx)
	_, replicateErr := r.h.workers.Replicator.Replicate(ctx, cfg, nil)
	_, cleanErr := r.h.workers.OverReplicationCleaner.Clean(ctx, cfg, nil)
	_, cleanupErr := r.h.workers.CleanupWorker.ProcessCleanupQueue(ctx)
	r.h.stack.Multipart.CleanupStaleMultipartUploads(ctx, 0)
	return cmp.Or(reapErr, replicateErr, cleanErr, cleanupErr)
}

// drain drains a random backend to completion.
func (r *recountRun) drain() {
	r.t.Helper()
	name := r.pickBackend()
	if err := r.h.stack.States.StartDrain(context.Background(), name); err != nil {
		r.t.Fatalf("StartDrain(%s): %v", name, err)
	}
	r.h.drainToCompletion(name)
	r.drained = name
}

// fingerprint sums up the ledger and its queues, so a round that changed
// nothing leaves it the same.
func (r *recountRun) fingerprint() string {
	var s string
	r.scan(&s, `SELECT concat_ws(' ',
		(SELECT count(*) FROM object_locations), (SELECT COALESCE(sum(size_bytes), 0) FROM object_locations),
		(SELECT count(*) FROM pending_objects), (SELECT count(*) FROM cleanup_queue),
		(SELECT count(*) FROM cleanup_dlq), (SELECT count(*) FROM multipart_uploads),
		(SELECT count(*) FROM replication_queue),
		(SELECT COALESCE(sum(bytes_used), 0) FROM backend_quota_stripes))`)
	return s
}

// leftover counts the work still queued for some worker to finish.
func (r *recountRun) leftover() int {
	var n int
	r.scan(&n, `SELECT (SELECT count(*) FROM pending_objects) + (SELECT count(*) FROM cleanup_queue)
		+ (SELECT count(*) FROM multipart_uploads) + (SELECT count(*) FROM replication_queue)`)
	return n
}

// -------------------------------------------------------------------------
// CHECKS
// -------------------------------------------------------------------------

// ledgerCopy is one row of the ledger.
type ledgerCopy struct {
	key, backend, path string
	size               int64
}

// check recounts the end state and fails on every way it disagrees with the
// ledger or the model. Each key's outcome then becomes the one it holds, so
// the next phase starts from what is there.
func (r *recountRun) check(stage string, factor int) {
	r.t.Helper()
	copies := r.ledger()
	byKey := map[string][]ledgerCopy{}
	keys := map[string]bool{}
	for _, c := range copies {
		byKey[c.key] = append(byKey[c.key], c)
		keys[c.key] = true
	}
	for key := range r.model {
		keys[key] = true
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		r.checkKey(stage, key, byKey[key], factor)
	}
	r.checkQuota(stage)
	r.checkBuckets(stage, copies)
	r.checkQueues(stage, factor)
}

// checkKey requires key to have no copies where it may hold nothing, or
// exactly factor copies on distinct active backends, each holding the same
// body, one some operation on key wrote.
func (r *recountRun) checkKey(stage, key string, copies []ledgerCopy, factor int) {
	r.t.Helper()
	if _, written := r.model[key]; !written {
		r.keyErrorf(key, "%s: ledger holds %s, which the workload never wrote", stage, key)
		return
	}
	var held []byte
	switch {
	case len(copies) == 0 && !r.allows(key, nil):
		r.keyErrorf(key, "%s: %s has no copies, but every operation on it left a body", stage, key)
	case len(copies) == 0:
	case len(copies) != factor:
		r.keyErrorf(key, "%s: %s has %d copies, want %d", stage, key, len(copies), factor)
		fallthrough
	default:
		held = r.heldBody(stage, key, copies)
		if !r.allows(key, held) {
			r.keyErrorf(key, "%s: %s holds %s, which no operation on it wrote", stage, key, describe(held))
		}
	}
	r.model[key] = [][]byte{held}
	r.note(key, stage+": holds "+label(held), nil)
}

// heldBody reads every copy of key and returns the body the first holds,
// requiring each copy to sit on its own active backend and hold the same body.
func (r *recountRun) heldBody(stage, key string, copies []ledgerCopy) []byte {
	r.t.Helper()
	var held []byte
	for i, c := range copies {
		if !slices.Contains(r.active(), c.backend) {
			r.keyErrorf(key, "%s: %s has a copy on %s, which is drained", stage, key, c.backend)
		}
		if i > 0 && c.backend == copies[i-1].backend {
			r.keyErrorf(key, "%s: %s has two copies on %s", stage, key, c.backend)
		}
		got := r.stored(stage, c)
		if i == 0 {
			held = got
			continue
		}
		if !bytes.Equal(got, held) {
			r.keyErrorf(key, "%s: %s copies disagree: %s on %s, %s on %s",
				stage, key, describe(held), copies[0].backend, describe(got), c.backend)
		}
	}
	return held
}

// stored reads one copy's bytes straight off its backend and requires the
// ledger's size for it to match.
func (r *recountRun) stored(stage string, c ledgerCopy) []byte {
	r.t.Helper()
	res, err := r.h.backends[c.backend].GetObject(context.Background(), c.path, "")
	if err != nil {
		r.t.Errorf("%s: ledger names %s on %s, which the backend cannot read: %v", stage, c.path, c.backend, err)
		return nil
	}
	defer func() { _ = res.Body.Close() }()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Errorf("%s: reading %s on %s: %v", stage, c.path, c.backend, err)
	}
	if int64(len(got)) != c.size {
		r.t.Errorf("%s: ledger records %d bytes for %s on %s, the backend holds %d", stage, c.size, c.path, c.backend, len(got))
	}
	return got
}

// checkQuota requires every backend's stripes to sum to the bytes its ledger
// rows record.
func (r *recountRun) checkQuota(stage string) {
	r.t.Helper()
	rows, err := r.h.db.Query(`SELECT q.backend_name, COALESCE(s.total, 0), COALESCE(l.total, 0)
		FROM backend_quotas q
		LEFT JOIN (SELECT backend_name, SUM(bytes_used) AS total FROM backend_quota_stripes GROUP BY 1) s USING (backend_name)
		LEFT JOIN (SELECT backend_name, SUM(size_bytes) AS total FROM object_locations GROUP BY 1) l USING (backend_name)
		ORDER BY 1`)
	if err != nil {
		r.t.Fatalf("%s: recount quota: %v", stage, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		var charged, recorded int64
		if err := rows.Scan(&name, &charged, &recorded); err != nil {
			r.t.Fatalf("%s: recount quota: %v", stage, err)
		}
		if charged != recorded {
			r.t.Errorf("%s: %s is charged %d bytes, its copies record %d", stage, name, charged, recorded)
		}
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("%s: recount quota: %v", stage, err)
	}
}

// checkBuckets requires each bucket to hold exactly the paths the ledger names
// on that backend: no bytes left behind, and no copy without its bytes.
func (r *recountRun) checkBuckets(stage string, copies []ledgerCopy) {
	r.t.Helper()
	for _, name := range r.h.order {
		named := map[string]bool{}
		for _, c := range copies {
			if c.backend == name {
				named[c.path] = true
			}
		}
		held := r.listBucket(stage, name)
		for _, path := range absentFrom(held, named) {
			r.keyErrorf(keyOfPath(path), "%s: %s holds %s, which no copy in the ledger names", stage, name, path)
		}
		for _, path := range absentFrom(named, held) {
			r.keyErrorf(keyOfPath(path), "%s: ledger names %s on %s, which the backend does not hold", stage, path, name)
		}
	}
}

// listBucket returns every path one backend's bucket holds.
func (r *recountRun) listBucket(stage, name string) map[string]bool {
	r.t.Helper()
	held := map[string]bool{}
	err := r.h.backends[name].ListObjects(context.Background(), "", func(page []s3be.ListedObject) error {
		for _, o := range page {
			held[o.Key] = true
		}
		return nil
	})
	if err != nil {
		r.t.Fatalf("%s: list %s: %v", stage, name, err)
	}
	return held
}

// keyOfPath returns the key a backend path stores a copy of, the key as the
// client wrote it. A path holds the key, then "!" and the writing intent's id.
func keyOfPath(path string) string {
	if i := strings.LastIndex(path, "!"); i >= 0 {
		path = path[:i]
	}
	return strings.TrimPrefix(path, virtualBucket+"/")
}

// absentFrom returns the members of a that b lacks, sorted so failures read the
// same on every run.
func absentFrom(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// checkQueues requires every queue a worker drains to be empty, and a rebuild
// of the replication queue to find no key the writes left off the factor
// without queueing it.
func (r *recountRun) checkQueues(stage string, factor int) {
	r.t.Helper()
	for _, table := range []string{"pending_objects", "cleanup_queue", "cleanup_dlq", "multipart_uploads", "multipart_parts", "replication_queue"} {
		var n int
		r.scan(&n, "SELECT count(*) FROM "+table)
		if n != 0 {
			r.t.Errorf("%s: %s holds %d rows", stage, table, n)
		}
	}
	queued, err := r.h.store.RebuildReplicationQueue(context.Background(), factor, nil)
	if err != nil {
		r.t.Fatalf("%s: rebuild replication queue: %v", stage, err)
	}
	if queued > 0 {
		r.t.Errorf("%s: a rebuild queues %d keys off the factor that no write queued", stage, queued)
	}
}

// ledger returns every copy row, ordered by key and backend.
func (r *recountRun) ledger() []ledgerCopy {
	r.t.Helper()
	rows, err := r.h.db.Query(`SELECT object_key, backend_name, storage_key, size_bytes
		FROM object_locations ORDER BY object_key, backend_name`)
	if err != nil {
		r.t.Fatalf("read ledger: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ledgerCopy
	for rows.Next() {
		var objectKey, storageKey string
		var c ledgerCopy
		if err := rows.Scan(&objectKey, &c.backend, &storageKey, &c.size); err != nil {
			r.t.Fatalf("read ledger: %v", err)
		}
		c.key = strings.TrimPrefix(objectKey, virtualBucket+"/")
		c.path = core.StoragePath(objectKey, storageKey)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read ledger: %v", err)
	}
	return out
}

// exec runs one statement against the harness database.
func (r *recountRun) exec(query string) {
	r.t.Helper()
	if _, err := r.h.db.Exec(query); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
}

// scan reads one value from the harness database into dst.
func (r *recountRun) scan(dst any, query string) {
	r.t.Helper()
	if err := r.h.db.QueryRow(query).Scan(dst); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
}
