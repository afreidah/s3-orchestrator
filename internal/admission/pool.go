// -------------------------------------------------------------------------------
// Admission - Concurrency Slots Divided Between Request Flows
//
// Author: Alex Freidah
//
// A pool of slots shared by request flows. A request takes a free slot at once
// while nobody is waiting. Otherwise it waits in its request flow's queue, and
// each slot that frees goes to the waiting request flow holding the fewest, so
// one with many requests in flight cannot keep the others waiting behind it.
// The queues hold at most as many requests as the pool has slots. Once they
// are full, the newest request of the request flow with the most waiting is
// refused, so one cannot fill them either.
//
// Background work is a request flow of its own, outside the client queues and
// their metrics. It holds at most half the pool and waits as long as its
// context allows, so it keeps a share however busy clients are and leaves them
// the rest.
// -------------------------------------------------------------------------------

package admission

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// Why a request was refused a slot.
var (
	ErrQueueFull   = errors.New("admission queue full")
	ErrWaitExpired = errors.New("admission wait expired")
)

// Gauge counts the client requests waiting for a slot. prometheus.Gauge
// satisfies it.
type Gauge interface {
	Inc()
	Dec()
}

// Observer times each client request's wait in seconds, whether or not a slot
// came. prometheus.Observer satisfies it.
type Observer interface {
	Observe(float64)
}

// Metrics is what a pool reports its client queues through.
type Metrics struct {
	Queued Gauge
	Waited Observer
}

// Pool hands out capacity slots by request flow. queued counts the client
// requests waiting and waiting lists the client request flows with any; seq
// numbers every request that waits, so equal request flows are served oldest
// first. spare is the last request flow forgotten, kept to start the next one
// without an allocation. A nil *Pool admits everything at once.
type Pool struct {
	name         string
	capacity     int
	metrics      Metrics
	mu           sync.Mutex
	inFlight     int
	queued       int
	seq          uint64
	requestFlows map[string]*requestFlow
	waiting      []*requestFlow
	spare        *requestFlow
	background   requestFlow
}

// requestFlow is one request flow's slots in use, the most it may hold, and
// its requests waiting, oldest first.
type requestFlow struct {
	name     string
	limit    int
	inFlight int
	queue    []*waiter
}

// waiter is one request waiting for a slot. done and err are set under the
// pool's lock before ready is closed, and a nil err means it was given one.
type waiter struct {
	requestFlow *requestFlow
	seq         uint64
	ready       chan struct{}
	done        bool
	err         error
}

// Slot is a slot taken from a pool, given back by Release. The zero Slot holds
// nothing.
type Slot struct {
	pool        *Pool
	requestFlow *requestFlow
}

// New builds a pool of capacity slots reporting its client queues through
// metrics. name is what callers label the pool's other metrics by.
func New(name string, capacity int, metrics Metrics) *Pool {
	return &Pool{
		name:         name,
		capacity:     capacity,
		metrics:      metrics,
		requestFlows: make(map[string]*requestFlow),
		background:   requestFlow{limit: max(capacity/2, 1)},
	}
}

// Name is the name the pool was built with, which callers label its metrics
// by.
func (p *Pool) Name() string {
	return p.name
}

// Acquire takes a slot in the request flow called name, waiting up to wait
// when the pool is full or others are waiting, or as long as ctx allows when
// wait is zero. A refusal is ErrQueueFull, ErrWaitExpired, or ctx's error.
func (p *Pool) Acquire(ctx context.Context, name string, wait time.Duration) (Slot, error) {
	if p == nil {
		return Slot{}, nil
	}
	p.mu.Lock()
	return p.acquire(ctx, p.requestFlowOf(name), wait)
}

// AcquireBackground takes a slot for background work, waiting as long as ctx
// allows. It is refused only with ctx's error.
func (p *Pool) AcquireBackground(ctx context.Context) (Slot, error) {
	if p == nil {
		return Slot{}, nil
	}
	p.mu.Lock()
	return p.acquire(ctx, &p.background, 0)
}

// Release gives the slot back, to the waiting request flow owed it next.
// Releasing the zero Slot does nothing.
func (s Slot) Release() {
	if s.pool == nil {
		return
	}
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	s.pool.release(s.requestFlow)
}

// acquire takes a slot for rf at once when one is free, no client is waiting
// and rf holds under its limit, and otherwise waits in rf's queue. The caller
// holds mu, which acquire releases.
func (p *Pool) acquire(ctx context.Context, rf *requestFlow, wait time.Duration) (Slot, error) {
	if p.inFlight < p.capacity && p.queued == 0 && len(rf.queue) == 0 && rf.inFlight < rf.limit {
		p.admit(rf)
		p.mu.Unlock()
		return Slot{p, rf}, nil
	}
	w, err := p.enqueue(rf)
	p.mu.Unlock()
	if err != nil {
		return Slot{}, err
	}
	if err := p.await(ctx, w, wait); err != nil {
		return Slot{}, err
	}
	return Slot{p, rf}, nil
}

// enqueue puts a request of rf at the back of its queue. A client request that
// finds the queues full pushes out the newest request of the longest, and is
// refused itself when its own request flow is as long as any.
func (p *Pool) enqueue(rf *requestFlow) (*waiter, error) {
	if p.isClient(rf) && p.queued >= p.capacity {
		longest := p.longest()
		if len(rf.queue) >= len(longest.queue) {
			p.forget(rf)
			return nil, ErrQueueFull
		}
		newest := longest.queue[len(longest.queue)-1]
		p.remove(newest)
		newest.finish(ErrQueueFull)
	}
	p.seq++
	w := &waiter{requestFlow: rf, seq: p.seq, ready: make(chan struct{})}
	rf.queue = append(rf.queue, w)
	if p.isClient(rf) {
		p.queued++
		p.metrics.Queued.Inc()
		if len(rf.queue) == 1 {
			p.waiting = append(p.waiting, rf)
		}
	}
	return w, nil
}

// await waits for w to be given a slot or refused, for up to wait, or as long
// as ctx allows when wait is zero. A client request's wait is timed whatever
// ends it.
func (p *Pool) await(ctx context.Context, w *waiter, wait time.Duration) error {
	var expired <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expired = timer.C
	}
	if p.isClient(w.requestFlow) {
		start := time.Now()
		defer func() { p.metrics.Waited.Observe(time.Since(start).Seconds()) }()
	}
	select {
	case <-w.ready:
		return w.err
	case <-expired:
		return p.abandon(w, ErrWaitExpired)
	case <-ctx.Done():
		return p.abandon(w, ctx.Err())
	}
}

// abandon takes w out of its queue once its wait ends for cause. A slot that
// reached it first is kept when only the wait ran out, and passed on when ctx
// ended.
func (p *Pool) abandon(w *waiter, cause error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case !w.done:
		p.remove(w)
		return cause
	case w.err != nil:
		return w.err
	case errors.Is(cause, ErrWaitExpired):
		return nil
	default:
		p.release(w.requestFlow)
		return cause
	}
}

// release gives back one of rf's slots and hands the free slots on. The caller
// holds mu.
func (p *Pool) release(rf *requestFlow) {
	rf.inFlight--
	p.inFlight--
	p.forget(rf)
	p.dispatch()
}

// dispatch hands each free slot to the request flow owed it, admitting it
// before its request leaves the queue so it is not forgotten in between.
func (p *Pool) dispatch() {
	for p.inFlight < p.capacity {
		rf := p.owed()
		if rf == nil {
			return
		}
		w := rf.queue[0]
		p.admit(rf)
		p.remove(w)
		w.finish(nil)
	}
}

// owed is the waiting request flow the next free slot goes to: the one holding
// the fewest, oldest request first among equals. Background work is owed one
// only while it holds under its limit.
func (p *Pool) owed() *requestFlow {
	var next *requestFlow
	for _, rf := range p.waiting {
		if next == nil || ahead(rf, next) {
			next = rf
		}
	}
	bg := &p.background
	if len(bg.queue) > 0 && bg.inFlight < bg.limit && (next == nil || ahead(bg, next)) {
		next = bg
	}
	return next
}

// ahead reports whether a is owed a slot before b: it holds fewer, or as many
// and its oldest request has waited longer.
func ahead(a, b *requestFlow) bool {
	if a.inFlight != b.inFlight {
		return a.inFlight < b.inFlight
	}
	return a.queue[0].seq < b.queue[0].seq
}

// longest is the client request flow with the most requests waiting. The
// caller has checked some are.
func (p *Pool) longest() *requestFlow {
	longest := p.waiting[0]
	for _, rf := range p.waiting[1:] {
		if len(rf.queue) > len(longest.queue) {
			longest = rf
		}
	}
	return longest
}

// remove takes w out of its request flow's queue, and a client request flow
// off the waiting list once nobody in it is left waiting.
func (p *Pool) remove(w *waiter) {
	rf := w.requestFlow
	i := slices.Index(rf.queue, w)
	rf.queue = slices.Delete(rf.queue, i, i+1)
	if !p.isClient(rf) {
		return
	}
	p.queued--
	p.metrics.Queued.Dec()
	if len(rf.queue) == 0 {
		j := slices.Index(p.waiting, rf)
		p.waiting = slices.Delete(p.waiting, j, j+1)
		p.forget(rf)
	}
}

// admit counts a slot taken by rf.
func (p *Pool) admit(rf *requestFlow) {
	rf.inFlight++
	p.inFlight++
}

// isClient reports whether rf is a client request flow rather than background
// work, which is kept out of the client queues and their metrics.
func (p *Pool) isClient(rf *requestFlow) bool {
	return rf != &p.background
}

// requestFlowOf returns the client request flow called name, starting it from
// the spare, or a new one, when none is in use.
func (p *Pool) requestFlowOf(name string) *requestFlow {
	if rf, ok := p.requestFlows[name]; ok {
		return rf
	}
	rf := p.spare
	if rf != nil {
		p.spare = nil
		rf.name = name
	} else {
		rf = &requestFlow{name: name, limit: p.capacity}
	}
	p.requestFlows[name] = rf
	return rf
}

// forget drops a client request flow with nothing in flight and nobody
// waiting, so the pool only keeps the ones in use, and keeps it as the spare.
func (p *Pool) forget(rf *requestFlow) {
	if p.isClient(rf) && rf.inFlight == 0 && len(rf.queue) == 0 {
		delete(p.requestFlows, rf.name)
		p.spare = rf
	}
}

// finish tells w it was given a slot, when err is nil, or refused with err.
// The caller holds the pool's lock.
func (w *waiter) finish(err error) {
	w.done = true
	w.err = err
	close(w.ready)
}
