// -------------------------------------------------------------------------------
// Shared-State Watch Service Tests
//
// Author: Alex Freidah
//
// Covers the services that apply what another instance announces: the
// provisioning rebuild and the object cache invalidation, each over an
// in-memory store shared by two instances.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	objcache "github.com/afreidah/s3-orchestrator/internal/cache"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate/sharedstatetest"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// runService runs svc until the test ends.
func runService(t *testing.T, svc lifecycle.Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestProvisioningWatcher_RebuildsOnSubscribeAndEveryAnnouncement rebuilds as
// the subscription is established and once per change another instance
// announces, and keeps watching when a rebuild fails.
func TestProvisioningWatcher_RebuildsOnSubscribeAndEveryAnnouncement(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	var mu sync.Mutex
	rebuilds := 0
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return rebuilds
	}
	runService(t, newProvisioningWatcher(sharedstate.New(mem, "b"), func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		rebuilds++
		if rebuilds == 2 {
			return errors.New("store unavailable")
		}
		return nil
	}))
	waitFor(t, "the subscription", func() bool { return count() == 1 })

	other := provisioningTopic(sharedstate.New(mem, "a"))
	for range 2 {
		if err := other.Publish(context.Background(), struct{}{}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	if got := count(); got != 3 {
		t.Errorf("rebuilt %d times, want 3: subscribe and two announcements", got)
	}
}

// TestCacheWatcher_AppliesInvalidationsAndClearsOnResync empties the cache as
// the subscription is established, then applies a prefix invalidation another
// instance announces.
func TestCacheWatcher_AppliesInvalidationsAndClearsOnResync(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	c, err := objcache.NewMemoryCache(objcache.MemoryConfig{MaxSize: 1 << 20, MaxObjectSize: 1 << 20, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.PutBytes("before", []byte("x"), objcache.EntryMeta{})

	runService(t, newCacheWatcher(sharedstate.New(mem, "b"), c))
	waitFor(t, "the resync to empty the cache", func() bool { return c.Stats().Entries == 0 })

	c.PutBytes("logs/a", []byte("x"), objcache.EntryMeta{})
	c.PutBytes("keep/b", []byte("x"), objcache.EntryMeta{})
	err = objcache.InvalidationTopic(sharedstate.New(mem, "a")).Publish(context.Background(), objcache.Invalidation{Prefix: "logs/"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, ok := c.Get("logs/a"); ok {
		t.Error("logs/a still cached after the announced invalidation")
	}
	if _, ok := c.Get("keep/b"); !ok {
		t.Error("keep/b dropped by an invalidation of another prefix")
	}
}
