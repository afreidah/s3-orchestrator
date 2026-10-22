// -------------------------------------------------------------------------------
// Audit - Per-Backend Usage Tally
//
// Author: Alex Freidah
//
// A context-scoped tally of the usage charged to each backend. The usage
// tracker adds every charge to the tally on its context, so a batch that opens
// one reports exactly what its work cost, in the same figures the usage
// metrics count.
// -------------------------------------------------------------------------------

package audit

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

// BackendUsage is what one backend was charged: the requests made of it and
// the bytes it sent and received.
type BackendUsage struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// plus returns the sum of two charges.
func (u BackendUsage) plus(o BackendUsage) BackendUsage {
	return BackendUsage{
		APIRequests:  u.APIRequests + o.APIRequests,
		EgressBytes:  u.EgressBytes + o.EgressBytes,
		IngressBytes: u.IngressBytes + o.IngressBytes,
	}
}

// Usage is the usage charged to each backend, keyed by backend name.
type Usage map[string]BackendUsage

// Plus returns the sum of two usages without modifying either.
func (u Usage) Plus(o Usage) Usage {
	if len(u) == 0 && len(o) == 0 {
		return nil
	}
	out := maps.Clone(u)
	if out == nil {
		out = make(Usage, len(o))
	}
	for name, c := range o {
		out[name] = out[name].plus(c)
	}
	return out
}

// Attr renders the usage as a "usage" group holding one group per backend, in
// name order. An empty usage renders as an empty group, which slog leaves out.
func (u Usage) Attr() slog.Attr {
	groups := make([]any, 0, len(u))
	for _, name := range slices.Sorted(maps.Keys(u)) {
		c := u[name]
		groups = append(groups, slog.Group(name,
			slog.Int64("api_requests", c.APIRequests),
			slog.Int64("egress_bytes", c.EgressBytes),
			slog.Int64("ingress_bytes", c.IngressBytes),
		))
	}
	return slog.Group("usage", groups...)
}

// Tally accumulates the usage charged under one context. A tally opened inside
// another also adds to the outer one, so an enclosing cycle still counts the
// work of the batches it runs. Safe for concurrent use.
type Tally struct {
	mu     sync.Mutex
	usage  Usage
	parent *Tally
}

// WithTally opens a tally on the context and returns both.
func WithTally(ctx context.Context) (context.Context, *Tally) {
	t := &Tally{parent: TallyOf(ctx)}
	return context.WithValue(ctx, tallyKey, t), t
}

// TallyOf returns the innermost tally on the context, or nil when none is open.
func TallyOf(ctx context.Context) *Tally {
	t, _ := ctx.Value(tallyKey).(*Tally)
	return t
}

// Add charges c to the backend on this tally and every tally enclosing it.
func (t *Tally) Add(backend string, c BackendUsage) {
	for ; t != nil; t = t.parent {
		t.mu.Lock()
		if t.usage == nil {
			t.usage = make(Usage)
		}
		t.usage[backend] = t.usage[backend].plus(c)
		t.mu.Unlock()
	}
}

// Usage returns a copy of what the tally holds.
func (t *Tally) Usage() Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return maps.Clone(t.usage)
}
