// -------------------------------------------------------------------------------
// Detached Upload Tracker
//
// Author: Alex Freidah
//
// Owns the work a write leaves running after its client has been answered: the
// copies still uploading and the goroutine that commits them. Nothing else
// holds that work, so without a registry a shutdown cannot wait for it and
// nothing bounds how much of it can pile up behind a slow backend.
//
// One slot per write rather than per upload. The slot is what shutdown waits
// on and what the ceiling counts, and both of those are properties of the
// write's tail as a whole - the payload it holds open, the goroutine reading
// it - rather than of the individual copies underneath.
// -------------------------------------------------------------------------------

package writepath

import (
	"context"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// DetachedDrainTimeout bounds how long shutdown waits for the tails still
// running. Sized like the HTTP drain it follows, since the two are halves of
// the same wait: requests first, then the work those requests left behind.
//
// Whatever has not finished by then is left as it would be if the process had
// been killed - the intents stay, and the reaper resolves them on a later tick.
// The drain shortens the common case; it is not what makes the write path safe.
const DetachedDrainTimeout = 30 * time.Second

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// DetachedUploads is the registry of writes whose copies outlived their
// response. The zero value is unusable; construct one with NewDetachedUploads.
//
// The limit is a ceiling for the case the gauge exists to catch - a backend
// that is slow rather than broken, quietly accumulating tails - and not a
// queue. A write that cannot get a slot places fewer copies rather than
// waiting, because waiting would put the backlog on the client, which is what
// answering on the first copy exists to avoid.
type DetachedUploads struct {
	mu     sync.Mutex
	cond   *sync.Cond
	limit  int
	active int
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewDetachedUploads builds a registry admitting at most limit tails at once. A
// limit of zero or less admits none, which turns every write back into one that
// places a single copy.
func NewDetachedUploads(limit int) *DetachedUploads {
	d := &DetachedUploads{limit: limit}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Begin takes a slot for one write's tail, reporting whether it got one. The
// caller releases it with the returned func once every copy has settled, which
// is what lets a drain know the tail is done.
//
// A refused slot is not an error: the write places one copy and the replicator
// makes the rest, which is what every write did before the fan-out existed.
func (d *DetachedUploads) Begin() (release func(), admitted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active >= d.limit {
		return nil, false
	}
	d.active++
	telemetry.DetachedUploadsDepth.Set(float64(d.active))
	return sync.OnceFunc(d.done), true
}

// Depth reports how many tails are running, which is the operator's signal that
// a backend is falling behind without failing.
func (d *DetachedUploads) Depth() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

// Wait blocks until every tail has finished or ctx is done, and reports how
// many were still running when it returned. A non-zero count means the deadline
// won and those copies are the reaper's now.
func (d *DetachedUploads) Wait(ctx context.Context) int {
	// The waiter is woken either by the last release or by the context, so a
	// deadline that expires while nothing is finishing still returns.
	stop := context.AfterFunc(ctx, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.cond.Broadcast()
	})
	defer stop()

	d.mu.Lock()
	defer d.mu.Unlock()
	for d.active > 0 && ctx.Err() == nil {
		d.cond.Wait()
	}
	return d.active
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// done releases a slot and wakes whoever is draining.
func (d *DetachedUploads) done() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active--
	telemetry.DetachedUploadsDepth.Set(float64(d.active))
	d.cond.Broadcast()
}
