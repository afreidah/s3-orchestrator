// -------------------------------------------------------------------------------
// Detached Upload Tracker Tests
//
// Author: Alex Freidah
//
// Covers the three things the registry is for: refusing a write once the fleet
// is carrying as many unfinished tails as it will, reporting how many are
// running, and letting a shutdown wait for them without waiting forever.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestDetachedUploads_AdmitsUpToTheLimit verifies the ceiling is a ceiling: the
// slot after the last one is refused rather than queued, because a write that
// waited for one would put the backlog on its client.
func TestDetachedUploads_AdmitsUpToTheLimit(t *testing.T) {
	t.Parallel()
	d := NewDetachedUploads(2)

	first, ok := d.Begin()
	if !ok {
		t.Fatal("first tail was refused")
	}
	if _, ok := d.Begin(); !ok {
		t.Fatal("second tail was refused below the limit")
	}
	if _, ok := d.Begin(); ok {
		t.Error("a third tail was admitted past a limit of 2")
	}
	if got := d.Depth(); got != 2 {
		t.Errorf("depth = %d, want 2", got)
	}

	// A released slot is reusable, so a burst that clears does not leave the
	// fan-out permanently off.
	first()
	if _, ok := d.Begin(); !ok {
		t.Error("a released slot was not handed to the next write")
	}
}

// TestDetachedUploads_ReleaseIsIdempotent verifies a slot handed back twice is
// only returned once. The release rides on a commit path with several exits,
// and a double release would inflate the ceiling for every later write.
func TestDetachedUploads_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	d := NewDetachedUploads(1)

	release, ok := d.Begin()
	if !ok {
		t.Fatal("the only slot was refused")
	}
	release()
	release()

	if got := d.Depth(); got != 0 {
		t.Fatalf("depth = %d after a double release, want 0", got)
	}
	if _, ok := d.Begin(); !ok {
		t.Fatal("the slot did not come back")
	}
	if _, ok := d.Begin(); ok {
		t.Error("the double release raised the ceiling")
	}
}

// TestDetachedUploads_ZeroLimitAdmitsNothing verifies a registry with no room
// turns the fan-out off rather than running work it cannot account for.
func TestDetachedUploads_ZeroLimitAdmitsNothing(t *testing.T) {
	t.Parallel()
	if _, ok := NewDetachedUploads(0).Begin(); ok {
		t.Error("a limit of zero admitted a tail")
	}
}

// TestDetachedUploads_WaitReturnsWhenTheLastTailFinishes verifies the shutdown
// wait ends as soon as the work does, rather than sitting out its deadline.
func TestDetachedUploads_WaitReturnsWhenTheLastTailFinishes(t *testing.T) {
	t.Parallel()
	d := NewDetachedUploads(4)

	releases := make([]func(), 0, 3)
	for range 3 {
		release, ok := d.Begin()
		if !ok {
			t.Fatal("a tail was refused below the limit")
		}
		releases = append(releases, release)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, release := range releases {
			time.Sleep(time.Millisecond)
			release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if remaining := d.Wait(ctx); remaining != 0 {
		t.Errorf("Wait returned with %d tails still running, want 0", remaining)
	}
	wg.Wait()
}

// TestDetachedUploads_WaitGivesUpOnTheDeadline verifies the drain is bounded.
// A tail that outlasts shutdown is left as a kill would leave it, for the
// reaper to resolve, rather than holding the process open.
func TestDetachedUploads_WaitGivesUpOnTheDeadline(t *testing.T) {
	t.Parallel()
	d := NewDetachedUploads(2)
	release, ok := d.Begin()
	if !ok {
		t.Fatal("the first tail was refused")
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	remaining := d.Wait(ctx)
	if remaining != 1 {
		t.Errorf("Wait reported %d tails still running, want 1", remaining)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Wait took %s to give up on a 20ms deadline", elapsed)
	}
}

// TestDetachedUploads_WaitOnAnEmptyRegistryReturnsImmediately verifies the
// common shutdown - nothing in flight - costs nothing.
func TestDetachedUploads_WaitOnAnEmptyRegistryReturnsImmediately(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if remaining := NewDetachedUploads(4).Wait(ctx); remaining != 0 {
		t.Errorf("Wait on an empty registry returned %d", remaining)
	}
}

// TestDetachedUploads_ConcurrentBeginsRespectTheLimit verifies the ceiling
// holds when writes arrive together, which is the only way it is ever
// approached.
func TestDetachedUploads_ConcurrentBeginsRespectTheLimit(t *testing.T) {
	t.Parallel()
	const limit = 8
	d := NewDetachedUploads(limit)

	var (
		mu       sync.Mutex
		admitted int
		wg       sync.WaitGroup
	)
	for range limit * 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := d.Begin(); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != limit {
		t.Errorf("admitted %d tails, want exactly the limit of %d", admitted, limit)
	}
	if got := d.Depth(); got != limit {
		t.Errorf("depth = %d, want %d", got, limit)
	}
}
