// -------------------------------------------------------------------------------
// Usage Limits - Compiled Per-Backend Request Budgets
//
// Author: Alex Freidah
//
// Turns the per-backend request budgets an operator writes in config into the
// form admission reads on every backend call: a set of named pools, and a
// lookup from operation to the pools that operation charges. Compilation
// happens once at startup and on reload, so the request path does a map lookup
// rather than matching operation names against pool definitions.
//
// Providers group operations into billing classes and disagree about the
// grouping, so the mapping is the operator's to declare. Pools are additive:
// an operation charges every pool it belongs to and is admitted only when all
// of them have headroom, which is what lets a sub-budget sit inside an
// aggregate one.
// -------------------------------------------------------------------------------

package core

import (
	"fmt"

	"github.com/afreidah/s3-orchestrator/internal/s3op"
)

// PoolAll is the name given to the pool that a bare api_request_limit
// desugars into. It covers every metered operation, which is what the single
// scalar always meant.
const PoolAll = "all"

// PoolSpec is one budget as written in config, before compilation.
// Operations may contain s3op.Wildcard to mean every metered operation.
type PoolSpec struct {
	Name       string
	Operations []string
	Limit      int64
}

// RequestPool is a compiled budget: the name it is counted under and the
// ceiling it enforces. A limit of 0 means unlimited, matching the convention
// the byte limits already use - the pool is still counted, it just never
// refuses.
type RequestPool struct {
	Name  string
	Limit int64
}

// -------------------------------------------------------------------------
// CONSTRUCTION
// -------------------------------------------------------------------------

// SingleRequestPool is the desugaring of a bare monthly request cap: one pool
// over every metered operation, which is what the single scalar always meant.
// Returns nil for a non-positive limit, so an unset cap compiles to no pools
// rather than to an unlimited one.
func SingleRequestPool(limit int64) []PoolSpec {
	if limit <= 0 {
		return nil
	}
	return []PoolSpec{{Name: PoolAll, Operations: []string{s3op.Wildcard}, Limit: limit}}
}

// UsageLimits holds the monthly usage limits for a single backend. Byte limits
// are scalar; request limits are pools with a per-operation lookup. Period is
// the monthly cycle the limits are counted over. Build one with
// NewUsageLimits; it is immutable afterwards and safe to share across
// goroutines.
type UsageLimits struct {
	EgressByteLimit  int64
	IngressByteLimit int64
	Period           UsagePeriod

	pools       []RequestPool
	byOperation map[s3op.Operation][]RequestPool
}

// NewUsageLimits compiles the configured pools for one backend. Operations in
// unmetered are removed from every pool, including the wildcard, but still
// count toward the backend's request total. A pool that explicitly names an
// unmetered operation is a config error.
func NewUsageLimits(egressLimit, ingressLimit int64, specs []PoolSpec, unmetered []s3op.Operation) (UsageLimits, error) {
	lim := UsageLimits{EgressByteLimit: egressLimit, IngressByteLimit: ingressLimit}
	if len(specs) == 0 {
		return lim, nil
	}

	free := make(map[s3op.Operation]bool, len(unmetered))
	for _, op := range unmetered {
		free[op] = true
	}

	lim.pools = make([]RequestPool, 0, len(specs))
	lim.byOperation = make(map[s3op.Operation][]RequestPool, len(s3op.All()))
	for _, spec := range specs {
		pool := RequestPool{Name: spec.Name, Limit: spec.Limit}
		ops, err := expand(spec, free)
		if err != nil {
			return UsageLimits{}, err
		}
		lim.pools = append(lim.pools, pool)
		for _, op := range ops {
			lim.byOperation[op] = append(lim.byOperation[op], pool)
		}
	}
	return lim, nil
}

// expand resolves one pool's operation list against the unmetered set,
// returning the operations it actually charges.
func expand(spec PoolSpec, free map[s3op.Operation]bool) ([]s3op.Operation, error) {
	seen := make(map[s3op.Operation]bool, len(spec.Operations))
	out := make([]s3op.Operation, 0, len(spec.Operations))
	for _, name := range spec.Operations {
		if name == s3op.Wildcard {
			for _, op := range s3op.All() {
				if !free[op] && !seen[op] {
					seen[op] = true
					out = append(out, op)
				}
			}
			continue
		}
		op := s3op.Operation(name)
		if free[op] {
			return nil, fmt.Errorf("pool %q charges %s, which is also listed as unmetered", spec.Name, name)
		}
		if !seen[op] {
			seen[op] = true
			out = append(out, op)
		}
	}
	return out, nil
}

// -------------------------------------------------------------------------
// ACCESSORS
// -------------------------------------------------------------------------

// Pools returns every compiled pool for the backend, in configured order.
// Used by the flush path and the usage surfaces, which report a pool whether
// or not any operation charged it this period.
func (u UsageLimits) Pools() []RequestPool { return u.pools }

// PoolsFor returns the pools one operation charges. Empty means the operation
// is unmetered, or that the backend has no request budgets at all.
func (u UsageLimits) PoolsFor(op s3op.Operation) []RequestPool { return u.byOperation[op] }

// Unlimited reports whether the backend has no enforceable limit in any
// dimension. Admission short-circuits on this, which keeps an unconfigured
// backend off the counter-read path entirely.
func (u UsageLimits) Unlimited() bool {
	if u.EgressByteLimit > 0 || u.IngressByteLimit > 0 {
		return false
	}
	for _, p := range u.pools {
		if p.Limit > 0 {
			return false
		}
	}
	return true
}

// MeterEgress and the other meter kinds name the dimension a UsageMeter
// measures. Egress and ingress count bytes; requests counts one request
// budget's calls.
const (
	MeterEgress   = "egress"
	MeterIngress  = "ingress"
	MeterRequests = "requests"
)

// UsageMeter is one limited dimension of a backend's usage: what it has used
// this period against its limit. Budget names the request budget of a requests
// meter and is empty for the byte meters.
type UsageMeter struct {
	Kind   string
	Budget string
	Used   int64
	Limit  int64
}

// Meters pairs a backend's usage with every limit it enforces: egress, ingress,
// then each limited request budget in configured order. A dimension with no
// limit has nothing to be measured against and is left out.
func (u UsageLimits) Meters(used Usage) []UsageMeter {
	var out []UsageMeter
	if u.EgressByteLimit > 0 {
		out = append(out, UsageMeter{Kind: MeterEgress, Used: used.EgressBytes, Limit: u.EgressByteLimit})
	}
	if u.IngressByteLimit > 0 {
		out = append(out, UsageMeter{Kind: MeterIngress, Used: used.IngressBytes, Limit: u.IngressByteLimit})
	}
	for _, p := range u.pools {
		if p.Limit > 0 {
			out = append(out, UsageMeter{Kind: MeterRequests, Budget: p.Name, Used: used.Pools[p.Name], Limit: p.Limit})
		}
	}
	return out
}

// -------------------------------------------------------------------------
// USAGE READINGS
// -------------------------------------------------------------------------

// UsageStat holds usage statistics for a single backend in a given period.
// APIRequests is the count of backend calls made, including ones no pool
// charges, so it stays an honest record of request volume rather than a
// figure derived from the budgets.
type UsageStat struct {
	APIRequests  int64
	EgressBytes  int64
	IngressBytes int64
}

// PoolUsage is the per-pool request count for one backend in one period,
// keyed by pool name. Pools are additive, so these do not sum to
// UsageStat.APIRequests and must never be presented as if they did.
type PoolUsage map[string]int64

// Usage is one backend's counters for one period: the request and byte totals
// and every request pool charged. It is the one shape the counters, the limit
// checks, the flush and the stored rows exchange.
type Usage struct {
	UsageStat
	Pools PoolUsage
}

// Reset zeroes u for reuse, keeping its pool map's storage so refilling it
// with the same pools does not allocate.
func (u *Usage) Reset() {
	u.UsageStat = UsageStat{}
	clear(u.Pools)
}

// AddPool adds n to one pool's count, creating the pool map on first use.
func (u *Usage) AddPool(name string, n int64) {
	if u.Pools == nil {
		u.Pools = make(PoolUsage)
	}
	u.Pools[name] += n
}

// Add adds o's totals and pools into u.
func (u *Usage) Add(o Usage) {
	u.APIRequests += o.APIRequests
	u.EgressBytes += o.EgressBytes
	u.IngressBytes += o.IngressBytes
	for name, n := range o.Pools {
		u.AddPool(name, n)
	}
}

// IsZero reports whether u carries nothing to flush.
func (u Usage) IsZero() bool {
	if u.UsageStat != (UsageStat{}) {
		return false
	}
	for _, n := range u.Pools {
		if n != 0 {
			return false
		}
	}
	return true
}

// WorkloadUsage is one backend's usage in one period, keyed by workload.
type WorkloadUsage map[string]Usage

// Total sums every workload's usage into one.
func (w WorkloadUsage) Total() Usage {
	var t Usage
	for _, u := range w {
		t.Add(u)
	}
	return t
}

// IsZero reports whether no workload carries anything to flush.
func (w WorkloadUsage) IsZero() bool {
	for _, u := range w {
		if !u.IsZero() {
			return false
		}
	}
	return true
}

// Totals sums each backend's workloads, for the readers that only need what a
// backend has used.
func Totals(usage map[string]WorkloadUsage) map[string]Usage {
	out := make(map[string]Usage, len(usage))
	for name, w := range usage {
		out[name] = w.Total()
	}
	return out
}

// UsageBatch is one backend's counters for one period, taken for a flush. ID
// is set for a batch staged in shared storage, which more than one flush can
// find, so the store applies a batch with an ID at most once. A batch held
// only in this process's memory has no ID.
type UsageBatch struct {
	ID        string
	Backend   string
	Period    string
	Workloads WorkloadUsage
}
