// -------------------------------------------------------------------------------
// Redis Usage Hash Tests
//
// Author: Alex Freidah
//
// Usage counters live in one hash per backend and period, and a flush stages a
// hash by renaming it under a flush ID before the store applies it. These
// tests run the backend against an in-memory Redis to pin the key layout, the
// stage-apply-delete cycle, and the fallback contract: a failed command keeps
// the charge in the local counters rather than dropping it.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

const (
	testPrefix   = "test"
	stagedPrefix = "test:usage-staged:"
)

// errRedisDown is the Redis failure the fake injects.
var errRedisDown = errors.New("redis down")

// fakeRedis is an in-memory Redis holding hashes only, enough for the usage
// paths. Each error field, when set, fails that command.
type fakeRedis struct {
	RedisClient

	mu        sync.Mutex
	hashes    map[string]map[string]string
	ttls      map[string]time.Duration
	execErr   error
	hgetErr   error
	renameErr error
	scanErr   error
	delErr    error
}

// newFakeRedis returns an empty fake.
func newFakeRedis() *fakeRedis {
	return &fakeRedis{hashes: map[string]map[string]string{}, ttls: map[string]time.Duration{}}
}

// HGetAll returns a copy of the hash, or an empty map when it does not exist,
// as Redis does.
func (f *fakeRedis) HGetAll(_ context.Context, key string) *redis.MapStringStringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hgetErr != nil {
		return redis.NewMapStringStringResult(nil, f.hgetErr)
	}
	return redis.NewMapStringStringResult(maps.Clone(f.hashes[key]), nil)
}

// Rename moves a hash to a new key, failing as Redis does when the source is
// missing.
func (f *fakeRedis) Rename(_ context.Context, key, newkey string) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renameErr != nil {
		return redis.NewStatusResult("", f.renameErr)
	}
	h, ok := f.hashes[key]
	if !ok {
		return redis.NewStatusResult("", errors.New("ERR no such key"))
	}
	delete(f.hashes, key)
	f.hashes[newkey] = h
	return redis.NewStatusResult("OK", nil)
}

// Scan returns one matching key per call so the caller's cursor loop is
// exercised.
func (f *fakeRedis) Scan(_ context.Context, cursor uint64, match string, _ int64) *redis.ScanCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scanErr != nil {
		return redis.NewScanCmdResult(nil, 0, f.scanErr)
	}
	var keys []string
	for k := range f.hashes {
		if ok, _ := path.Match(match, k); ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if cursor >= uint64(len(keys)) {
		return redis.NewScanCmdResult(nil, 0, nil)
	}
	next := cursor + 1
	if next == uint64(len(keys)) {
		next = 0
	}
	return redis.NewScanCmdResult(keys[cursor:cursor+1], next, nil)
}

// Del removes the named hashes.
func (f *fakeRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return redis.NewIntResult(0, f.delErr)
	}
	var n int64
	for _, k := range keys {
		if _, ok := f.hashes[k]; ok {
			delete(f.hashes, k)
			n++
		}
	}
	return redis.NewIntResult(n, nil)
}

// TxPipeline returns a transaction that applies its queued commands on Exec.
func (f *fakeRedis) TxPipeline() redis.Pipeliner {
	return &fakeTx{owner: f}
}

// seed sets one counter of workloadA in oldBucket directly, for state a test
// needs before it starts.
func (f *fakeRedis) seed(key, name string, n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hashes[key] == nil {
		f.hashes[key] = map[string]string{}
	}
	f.hashes[key][seededField(name)] = strconv.FormatInt(n, 10)
}

// has reports whether a key exists.
func (f *fakeRedis) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.hashes[key]
	return ok
}

// hincr is one queued HIncrBy.
type hincr struct {
	key, field string
	n          int64
}

// fakeTx queues HIncrBy and Expire and applies them together on Exec, or none
// of them when the fake's execErr is set.
type fakeTx struct {
	redis.Pipeliner

	owner   *fakeRedis
	incrs   []hincr
	expires []string
}

// HIncrBy queues one field increment.
func (t *fakeTx) HIncrBy(ctx context.Context, key, field string, n int64) *redis.IntCmd {
	t.incrs = append(t.incrs, hincr{key: key, field: field, n: n})
	return redis.NewIntCmd(ctx)
}

// Expire queues a TTL on the key.
func (t *fakeTx) Expire(ctx context.Context, key string, _ time.Duration) *redis.BoolCmd {
	t.expires = append(t.expires, key)
	return redis.NewBoolCmd(ctx)
}

// Exec applies every queued command, or fails with none applied.
func (t *fakeTx) Exec(_ context.Context) ([]redis.Cmder, error) {
	f := t.owner
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execErr != nil {
		return nil, f.execErr
	}
	for _, c := range t.incrs {
		if f.hashes[c.key] == nil {
			f.hashes[c.key] = map[string]string{}
		}
		cur, _ := strconv.ParseInt(f.hashes[c.key][c.field], 10, 64)
		f.hashes[c.key][c.field] = strconv.FormatInt(cur+c.n, 10)
	}
	for _, k := range t.expires {
		f.ttls[k] = keyTTL
	}
	return nil, nil
}

// newHashBackend builds a Redis counter backend over the client with a local
// fallback and a breaker that opens on the first failure.
func newHashBackend(t *testing.T, client RedisClient, backends ...string) *RedisCounterBackend {
	t.Helper()
	if len(backends) == 0 {
		backends = []string{backendB1}
	}
	return &RedisCounterBackend{
		client:    client,
		prefix:    testPrefix,
		local:     NewLocalCounterBackend(backends),
		backends:  backends,
		cb:        newTestCB(),
		log:       slog.Default(),
		stopProbe: make(chan struct{}),
		probeDone: make(chan struct{}),
	}
}

// liveKey is the live hash for a backend.
func liveKey(backend string) string {
	return testPrefix + ":usage:" + backend
}

// seededField is the hash field seed writes a counter under.
func seededField(name string) string {
	return fieldPrefix(oldBucket, workloadA) + name
}

// byBackend indexes batches by backend name.
func byBackend(batches []core.UsageBatch) map[string]core.UsageBatch {
	out := make(map[string]core.UsageBatch, len(batches))
	for _, b := range batches {
		out[b.Backend] = b
	}
	return out
}

// TestCharge_WritesOneHashPerBackend pins the key layout: totals and pools are
// fields of one hash under their bucket and workload, and the hash gets the
// key TTL.
func TestCharge_WritesOneHashPerBackend(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)

	r.Charge(backendB1, oldBucket, Charge{Workload: workloadA, APIRequests: 2, IngressBytes: 512, Pools: poolsOf(poolClassA, "class_b"), PoolN: 2})

	h := fr.hashes[liveKey(backendB1)]
	want := map[string]string{
		"1577836800:w:scrub:" + fieldAPIRequests:  "2",
		"1577836800:w:scrub:" + fieldIngressBytes: "512",
		"1577836800:w:scrub:pool:class_a":         "2",
		"1577836800:w:scrub:pool:class_b":         "2",
	}
	if !maps.Equal(h, want) {
		t.Errorf("hash = %v, want %v; zero fields are not written", h, want)
	}
	if fr.ttls[liveKey(backendB1)] != keyTTL {
		t.Errorf("TTL = %v, want %v", fr.ttls[liveKey(backendB1)], keyTTL)
	}

	r.Charge(backendB1, oldBucket, Charge{})
	if got := loaded(r, backendB1); got.APIRequests != 2 || got.Pools[poolClassA] != 2 {
		t.Errorf("Load = %+v, want 2 requests and 2 against class_a", got)
	}
}

// TestCharge_DropsAnUnknownBackend asserts a charge for a backend not named at
// construction reaches neither Redis nor the local counters.
func TestCharge_DropsAnUnknownBackend(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)

	r.Charge("unknown", oldBucket, Charge{APIRequests: 1})

	if fr.has(liveKey("unknown")) {
		t.Error("a charge for an unknown backend reached Redis")
	}
	if got := loaded(r.local, "unknown"); !got.IsZero() {
		t.Errorf("local counters = %+v, want zero", got)
	}
}

// TestTakeShared_EscapesGlobCharsInThePrefix asserts the SCAN pattern matches
// the prefix literally, so a prefix holding glob characters neither matches
// another prefix's staged hashes nor misses its own.
func TestTakeShared_EscapesGlobCharsInThePrefix(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	r.prefix = "te*st[1]"
	fr.seed(r.stagedKey(backendB1, "own"), fieldAPIRequests, 1)
	fr.seed("teXst1:usage-staged:b1:other", fieldAPIRequests, 2)

	batches := r.TakeShared(context.Background())

	if len(batches) != 1 || batches[0].ID != "own" {
		t.Errorf("TakeShared = %+v, want only this prefix's staged batch", batches)
	}
}

// TestCharge_FailureKeepsTheChargeLocally asserts a failed transaction moves
// the charge to the local counters, and Load still counts it.
func TestCharge_FailureKeepsTheChargeLocally(t *testing.T) {
	fr := newFakeRedis()
	fr.execErr = errRedisDown
	r := newHashBackend(t, fr)

	r.Charge(backendB1, oldBucket, Charge{APIRequests: 3})

	if !r.inFallback() {
		t.Error("a failure on an open breaker should enter fallback")
	}
	if got := loaded(r.local, backendB1).APIRequests; got != 3 {
		t.Errorf("local requests = %d, want 3", got)
	}
	if got := loaded(r, backendB1).APIRequests; got != 3 {
		t.Errorf("Load = %d, want the local 3", got)
	}
}

// TestLoad_AddsLocalToTheHash asserts Load is the shared hash plus what this
// instance holds locally, and falls back to local alone on a read error.
func TestLoad_AddsLocalToTheHash(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	r.Charge(backendB1, oldBucket, Charge{APIRequests: 5, Pools: poolsOf(poolClassA), PoolN: 5})
	r.local.Charge(backendB1, oldBucket, Charge{APIRequests: 2, Pools: poolsOf(poolClassA), PoolN: 2})

	if got := loaded(r, backendB1); got.APIRequests != 7 || got.Pools[poolClassA] != 7 {
		t.Errorf("Load = %+v, want 7 requests and 7 against class_a", got)
	}

	fr.hgetErr = errRedisDown
	if got := loaded(r, backendB1).APIRequests; got != 2 {
		t.Errorf("Load on a read error = %d, want the local 2", got)
	}
}

// TestFallback_ChargesAndLoadsLocally asserts a backend in fallback never
// touches Redis for a charge or a load.
func TestFallback_ChargesAndLoadsLocally(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	r.setFallback(true)
	t.Cleanup(func() { r.setFallback(false) })

	r.Charge(backendB1, oldBucket, Charge{APIRequests: 4})

	if fr.has(liveKey(backendB1)) {
		t.Error("a charge in fallback reached Redis")
	}
	if got := loaded(r, backendB1).APIRequests; got != 4 {
		t.Errorf("Load = %d, want the local 4", got)
	}
}

// TestTakeShared_StagesAndReturnsBatches asserts each live hash is renamed
// under a flush ID and returned as a batch whose key parses back, including a
// backend whose name holds a colon.
func TestTakeShared_StagesAndReturnsBatches(t *testing.T) {
	const colonBackend = "eu:west"
	fr := newFakeRedis()
	r := newHashBackend(t, fr, backendB1, colonBackend, "idle")
	r.Charge(backendB1, oldBucket, Charge{APIRequests: 2, Pools: poolsOf(poolClassA), PoolN: 2})
	r.Charge(colonBackend, oldBucket, Charge{EgressBytes: 100})

	batches := byBackend(r.TakeShared(context.Background()))

	if len(r.Backends()) != 3 {
		t.Errorf("Backends() = %v, want the three configured", r.Backends())
	}
	if len(batches) != 2 {
		t.Fatalf("TakeShared returned %v, want b1 and eu:west", batches)
	}
	if b := batches[backendB1]; b.ID == "" || totalOf(b).APIRequests != 2 || totalOf(b).Pools[poolClassA] != 2 {
		t.Errorf("b1 batch = %+v, want an ID and the charge", b)
	}
	if b := batches[colonBackend]; b.ID == "" || totalOf(b).EgressBytes != 100 {
		t.Errorf("eu:west batch = %+v, want an ID and 100 egress", b)
	}
	if fr.has(liveKey(backendB1)) || !fr.has(r.stagedKey(backendB1, batches[backendB1].ID)) {
		t.Error("the live hash should have moved to its staged key")
	}
}

// TestSettle_StagedBatch asserts an applied staged batch is deleted and an
// unapplied one is returned again, with the same ID, by the next take.
func TestSettle_StagedBatch(t *testing.T) {
	ctx := context.Background()
	fr := newFakeRedis()
	r := newHashBackend(t, fr, backendB1, "b2")
	r.Charge(backendB1, oldBucket, Charge{APIRequests: 1})
	r.Charge("b2", oldBucket, Charge{APIRequests: 2})

	first := byBackend(r.TakeShared(ctx))
	applied, kept := first[backendB1], first["b2"]
	r.Settle(ctx, &applied, true)
	r.Settle(ctx, &kept, false)

	if fr.has(r.stagedKey(applied.Backend, applied.ID)) {
		t.Error("an applied staged hash should be deleted")
	}
	again := r.TakeShared(ctx)
	if len(again) != 1 || again[0].ID != kept.ID || totalOf(again[0]).APIRequests != 2 {
		t.Errorf("second take = %+v, want only the kept b2 batch %q", again, kept.ID)
	}
}

// TestSettle_InFallbackKeepsTheStagedHash asserts a staged hash is not
// deleted while Redis is in fallback; a later flush finds it again.
func TestSettle_InFallbackKeepsTheStagedHash(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	key := r.stagedKey(backendB1, "abc")
	fr.seed(key, fieldAPIRequests, 1)
	r.setFallback(true)
	t.Cleanup(func() { r.setFallback(false) })

	r.Settle(context.Background(), &core.UsageBatch{ID: "abc", Backend: backendB1}, true)

	if !fr.has(key) {
		t.Error("the staged hash was deleted in fallback")
	}
}

// TestSettle_LocalBatchReturnsToLocal asserts a batch with no ID is the local
// backend's to settle.
func TestSettle_LocalBatchReturnsToLocal(t *testing.T) {
	r := newHashBackend(t, newFakeRedis())
	b := core.UsageBatch{Backend: backendB1, Buckets: core.BucketUsage{oldBucket: {workloadA: {UsageStat: core.UsageStat{APIRequests: 6}}}}}

	r.Settle(context.Background(), &b, false)

	batches := r.Take(context.Background())
	if len(batches) != 1 || batches[0].Buckets[oldBucket][workloadA].APIRequests != 6 {
		t.Errorf("Take = %+v, want the local batch of 6", batches)
	}
}

// TestLoad_SkipsBucketsBeforeSince asserts Load sums only the hash's buckets
// from since onward, while a flush still takes every bucket.
func TestLoad_SkipsBucketsBeforeSince(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	since := oldBucket.Add(time.Hour)
	r.Charge(backendB1, oldBucket, Charge{APIRequests: 5})
	r.Charge(backendB1, since, Charge{APIRequests: 2})

	var u core.Usage
	r.Load(backendB1, since, &u)
	if u.APIRequests != 2 {
		t.Errorf("Load since %v = %d requests, want 2", since, u.APIRequests)
	}
	batches := r.TakeShared(context.Background())
	if len(batches) != 1 || len(batches[0].Buckets) != 2 || totalOf(batches[0]).APIRequests != 7 {
		t.Errorf("TakeShared = %+v, want both buckets, 7 requests", batches)
	}
}

// TestTakeShared_SkipsWhatCannotBeRead asserts a malformed staged key, an
// empty hash, and an unparseable field are skipped rather than failing the
// take.
func TestTakeShared_SkipsWhatCannotBeRead(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	fr.seed(stagedPrefix+"garbage", fieldAPIRequests, 1)
	fr.hashes[r.stagedKey("empty", "e1")] = map[string]string{}
	good := r.stagedKey(backendB1, "g1")
	fr.seed(good, fieldAPIRequests, 3)
	fr.hashes[good][seededField(fieldEgressBytes)] = "NaN"
	fr.hashes[good][fieldIngressBytes] = "5"

	batches := r.TakeShared(context.Background())

	if len(batches) != 1 {
		t.Fatalf("TakeShared = %+v, want only g1", batches)
	}
	if u := totalOf(batches[0]); batches[0].ID != "g1" || u.APIRequests != 3 || u.EgressBytes != 0 || u.IngressBytes != 0 {
		t.Errorf("TakeShared = %+v, want g1 with 3 requests", batches)
	}
}

// TestTakeShared_StageErrorStillReadsStaged asserts a failed rename stops the
// staging pass but still returns hashes already staged.
func TestTakeShared_StageErrorStillReadsStaged(t *testing.T) {
	fr := newFakeRedis()
	r := newHashBackend(t, fr)
	fr.seed(r.stagedKey(backendB1, "left"), fieldAPIRequests, 4)
	fr.renameErr = errRedisDown

	batches := r.TakeShared(context.Background())

	if len(batches) != 1 || batches[0].ID != "left" {
		t.Errorf("TakeShared = %+v, want the leftover staged batch", batches)
	}
}

// TestTakeShared_ReadErrors asserts a failed scan or hash read returns no
// batch rather than a partial one.
func TestTakeShared_ReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*fakeRedis)
	}{
		{name: "scan", inject: func(f *fakeRedis) { f.scanErr = errRedisDown }},
		{name: "hgetall", inject: func(f *fakeRedis) { f.hgetErr = errRedisDown }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRedis()
			r := newHashBackend(t, fr)
			fr.seed(r.stagedKey(backendB1, "s1"), fieldAPIRequests, 1)
			tc.inject(fr)

			if got := r.TakeShared(context.Background()); len(got) != 0 {
				t.Errorf("TakeShared = %+v, want nothing", got)
			}
		})
	}
}

// TestParseStagedKey covers the key shapes a scan can return: a well-formed
// key, a backend name holding colons, and malformed keys that must not parse.
func TestParseStagedKey(t *testing.T) {
	r := &RedisCounterBackend{prefix: testPrefix}
	for _, tc := range []struct {
		key  string
		want core.UsageBatch
		ok   bool
	}{
		{key: stagedPrefix + "b1:ID1", want: core.UsageBatch{ID: "ID1", Backend: backendB1}, ok: true},
		{key: stagedPrefix + "eu:west:1:ID2", want: core.UsageBatch{ID: "ID2", Backend: "eu:west:1"}, ok: true},
		{key: "other:usage-staged:b1:ID1"},
		{key: stagedPrefix + "b1"},
		{key: stagedPrefix + "b1:"},
		{key: stagedPrefix + ":ID1"},
	} {
		got, ok := r.parseStagedKey(tc.key)
		if ok != tc.ok || got.ID != tc.want.ID || got.Backend != tc.want.Backend {
			t.Errorf("parseStagedKey(%q) = %+v, %v; want %+v, %v", tc.key, got, ok, tc.want, tc.ok)
		}
	}
}
