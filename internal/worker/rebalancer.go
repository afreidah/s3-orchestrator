// -------------------------------------------------------------------------------
// Rebalancer - Periodic Backend Object Distribution
//
// Author: Alex Freidah
//
// Moves objects between backends to optimize space distribution. Supports two
// strategies: "pack" consolidates free space by filling backends in order, and
// "spread" equalizes utilization ratios across all backends. Disabled by default
// to avoid unexpected API calls and egress charges.
// -------------------------------------------------------------------------------

package worker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/must"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RebalancerStore is the narrow persistence surface the rebalancer needs:
// per-backend listings + quota stats for the source/dest fill ratio.
// Declared locally so the worker does not pull in the full MetadataStore.
type RebalancerStore interface {
	core.ObjectStore
	core.QuotaStore
}

// Rebalancer moves objects between backends to optimize space distribution.
type Rebalancer struct {
	log       *slog.Logger
	ops       Ops
	placement Placement
	store     RebalancerStore
	cfg       syncutil.AtomicConfig[config.RebalanceConfig]
}

// NewRebalancer creates a Rebalancer with fleet operations, write-path
// placement, and a metadata store.
func NewRebalancer(ops Ops, placement Placement, store RebalancerStore) *Rebalancer {
	must.NotNil("ops", ops)
	must.NotNil("placement", placement)
	must.NotNil("store", store)
	return &Rebalancer{ops: ops, placement: placement, store: store, log: slog.Default().With(logfmt.Component("rebalancer"))}
}

// SetConfig atomically stores the rebalance configuration.
func (r *Rebalancer) SetConfig(cfg *config.RebalanceConfig) {
	r.cfg.Store(cfg)
}

// Config returns the current rebalance configuration.
func (r *Rebalancer) Config() *config.RebalanceConfig {
	return r.cfg.Load()
}

// RebalanceMove describes a single object move from one backend to another.
type RebalanceMove struct {
	ObjectKey   string
	FromBackend string
	ToBackend   string
	SizeBytes   int64
}

// progressLabel names the move for a streaming caller: the object and the
// backends it travels between, which is the whole of what a rebalance does.
func (m RebalanceMove) progressLabel() string {
	return fmt.Sprintf("%s  %s -> %s", m.ObjectKey, m.FromBackend, m.ToBackend)
}

// Why a cycle declined to move anything. Reported instead of a zero move count,
// which a caller cannot tell apart from a cycle that ran and found nothing.
const (
	SkipReasonWithinThreshold = "backend utilization is already within the rebalance threshold"
	SkipReasonEmptyPlan       = "the rebalance strategy planned no moves"
)

// RebalanceSummary is one cycle's outcome. SkipReason is set when the cycle
// never planned any moves; the embedded WorkSummary is zero in that case.
type RebalanceSummary struct {
	WorkSummary
	SkipReason string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Rebalance moves objects between backends to optimize space distribution.
// observer, when non-nil, receives a step per planned move so a streaming
// caller can report progress as the cycle runs.
func (r *Rebalancer) Rebalance(ctx context.Context, cfg config.RebalanceConfig, observer progress.Observer) (RebalanceSummary, error) {
	return runOpsCycle(ctx, "Rebalance", "rebalance", func(ctx context.Context) (RebalanceSummary, error) {
		start := time.Now()
		audit.Log(ctx, "rebalance.start",
			slog.String("strategy", cfg.Strategy),
			slog.Int("batch_size", cfg.BatchSize),
			slog.Float64("threshold", cfg.Threshold),
		)

		stats, err := r.store.GetQuotaStats(ctx)
		if err != nil {
			telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, OutcomeError).Inc()
			return RebalanceSummary{}, fmt.Errorf("failed to get quota stats: %w", err)
		}

		if !ExceedsThreshold(stats, r.ops.BackendOrder(), cfg.Threshold) {
			r.log.InfoContext(ctx, "rebalance skipping, within threshold",
				"threshold", cfg.Threshold, "strategy", cfg.Strategy)
			telemetry.RebalanceSkipped.WithLabelValues("threshold").Inc()
			return RebalanceSummary{SkipReason: SkipReasonWithinThreshold}, nil
		}

		var plan []RebalanceMove
		switch cfg.Strategy {
		case "pack":
			plan, err = r.PlanPackTight(ctx, stats, cfg.BatchSize)
		case "spread":
			plan, err = r.PlanSpreadEven(ctx, stats, cfg.BatchSize)
		default:
			return RebalanceSummary{}, fmt.Errorf("unknown rebalance strategy: %s", cfg.Strategy)
		}
		if err != nil {
			telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, OutcomeError).Inc()
			return RebalanceSummary{}, fmt.Errorf("failed to plan rebalance: %w", err)
		}

		telemetry.RebalancePending.Set(float64(len(plan)))

		if len(plan) == 0 {
			r.log.InfoContext(ctx, "rebalance skipping, empty plan", "strategy", cfg.Strategy)
			telemetry.RebalanceSkipped.WithLabelValues("empty_plan").Inc()
			return RebalanceSummary{SkipReason: SkipReasonEmptyPlan}, nil
		}

		sum := r.ExecuteMoves(ctx, plan, cfg.Strategy, cfg.Concurrency, observer)

		telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, sum.Outcome()).Inc()
		telemetry.RebalanceDuration.WithLabelValues(cfg.Strategy).Observe(time.Since(start).Seconds())

		audit.Log(ctx, "rebalance.complete",
			slog.String("strategy", cfg.Strategy),
			slog.Int("objects_moved", sum.Succeeded),
			slog.Int("planned", len(plan)),
			slog.Duration("duration", time.Since(start)),
		)
		return RebalanceSummary{WorkSummary: sum}, nil
	})
}

// -------------------------------------------------------------------------
// THRESHOLD CHECK
// -------------------------------------------------------------------------

// ExceedsThreshold reports whether the utilization spread across backends
// (max ratio minus min ratio) is at least the configured threshold.
func ExceedsThreshold(stats map[string]core.QuotaStat, order []string, threshold float64) bool {
	if len(order) < 2 {
		return false
	}

	var minRatio, maxRatio float64
	first := true

	for _, name := range order {
		stat, ok := stats[name]
		if !ok || stat.BytesLimit == 0 {
			continue
		}
		ratio := float64(stat.BytesUsed) / float64(stat.BytesLimit)
		if first {
			minRatio = ratio
			maxRatio = ratio
			first = false
		} else {
			if ratio < minRatio {
				minRatio = ratio
			}
			if ratio > maxRatio {
				maxRatio = ratio
			}
		}
	}

	return maxRatio-minRatio >= threshold
}

// -------------------------------------------------------------------------
// PACK TIGHT STRATEGY
// -------------------------------------------------------------------------

// backendUtil pairs a backend name with its byte capacity. Used during
// pack-tight planning to drive utilization-ordered traversal.
type backendUtil struct {
	Name  string
	Limit int64
}

// PlanPackTight consolidates objects onto the most-utilized backends by
// pulling from the least-utilized. Sorts by percent full descending and
// only moves an object from a less-full source to a more-full destination.
// Skips moves that would not increase the destination's packing ratio.
func (r *Rebalancer) PlanPackTight(ctx context.Context, stats map[string]core.QuotaStat, batchSize int) ([]RebalanceMove, error) {
	simUsed := make(map[string]int64)
	backends := sortedBackendsByUtilDesc(r.ops.BackendOrder(), stats, simUsed)
	state := newPlanState(r.ops.Usage(), simUsed, batchSize)

	var plan []RebalanceMove
	candidates := r.newCandidateCache(batchSize)

	for di := 0; di < len(backends) && state.remaining > 0; di++ {
		if ctx.Err() != nil {
			return plan, ctx.Err()
		}
		moves, err := r.packMovesIntoDestination(ctx, di, backends, state, candidates)
		if err != nil {
			return nil, err
		}
		plan = append(plan, moves...)
	}

	return plan, nil
}

// sortedBackendsByUtilDesc returns the configured backends with non-zero
// limits, sorted by utilization ratio descending. Populates simUsed with
// each backend's current bytes-used reading.
func sortedBackendsByUtilDesc(order []string, stats map[string]core.QuotaStat, simUsed map[string]int64) []backendUtil {
	var backends []backendUtil
	for _, name := range order {
		stat, ok := stats[name]
		if !ok || stat.BytesLimit == 0 {
			continue
		}
		simUsed[name] = stat.BytesUsed
		backends = append(backends, backendUtil{Name: name, Limit: stat.BytesLimit})
	}

	slices.SortFunc(backends, func(a, b backendUtil) int {
		ra := float64(simUsed[a.Name]) / float64(a.Limit)
		rb := float64(simUsed[b.Name]) / float64(b.Limit)
		return cmp.Compare(rb, ra)
	})
	return backends
}

// packMovesIntoDestination plans moves into backends[di] from less-full
// sources, walking sources from least-full upward. Mutates simUsed and the
// remaining pointer to reflect simulated moves.
func (r *Rebalancer) packMovesIntoDestination(
	ctx context.Context,
	di int,
	backends []backendUtil,
	state *planState,
	candidates *candidateCache,
) ([]RebalanceMove, error) {
	dest := backends[di]
	destFree := dest.Limit - state.simUsed[dest.Name]
	if destFree <= 0 {
		return nil, nil
	}

	var plan []RebalanceMove
	for si := len(backends) - 1; si > di && state.remaining > 0 && destFree > 0; si-- {
		src := backends[si]
		if !srcLessUtilized(src, dest, state.simUsed) {
			continue
		}

		sc, err := candidates.forBackend(ctx, src.Name)
		if err != nil {
			return nil, err
		}

		moves := r.packMovesFromSource(src, dest, sc.Objects, sc.Placement, state, &destFree)
		plan = append(plan, moves...)
	}
	return plan, nil
}

// packMovesFromSource walks one source's candidate objects and selects
// moves that fit dest, that dest does not already mirror, and that keep
// src strictly less utilized than dest after the simulated transfer.
func (r *Rebalancer) packMovesFromSource(
	src, dest backendUtil,
	objects []core.ObjectLocation,
	placement PlacementSet,
	state *planState,
	destFree *int64,
) []RebalanceMove {
	var moves []RebalanceMove
	for oi := range objects {
		if state.remaining <= 0 || *destFree <= 0 {
			break
		}
		if objects[oi].SizeBytes > *destFree {
			continue
		}
		if !srcLessUtilized(src, dest, state.simUsed) {
			break
		}
		if placement.Has(objects[oi].ObjectKey, dest.Name) {
			continue
		}
		if !state.allows(src.Name, dest.Name, objects[oi].SizeBytes) {
			telemetry.UsageLimitRejectionsTotal.WithLabelValues("rebalance", "transfer").Inc()
			continue
		}

		moves = append(moves, RebalanceMove{
			ObjectKey:   objects[oi].ObjectKey,
			FromBackend: src.Name,
			ToBackend:   dest.Name,
			SizeBytes:   objects[oi].SizeBytes,
		})
		state.accept(src.Name, dest.Name, objects[oi].SizeBytes)
		*destFree -= objects[oi].SizeBytes
	}
	return moves
}

// srcLessUtilized reports whether src's current ratio is strictly below
// dest's. Pack-tight only pulls when this is true.
func srcLessUtilized(src, dest backendUtil, simUsed map[string]int64) bool {
	srcRatio := float64(simUsed[src.Name]) / float64(src.Limit)
	destRatio := float64(simUsed[dest.Name]) / float64(dest.Limit)
	return srcRatio < destRatio
}

// fetchCopyMap batches GetObjectBackendsForKeys for every candidate key, so the
// inner loop does not issue a per-object lookup. Returns the lookup error so
// callers fail planning rather than continue with empty placement data, which
// plans transfers to destinations that already hold a copy.
func (r *Rebalancer) fetchCopyMap(ctx context.Context, objects []core.ObjectLocation) (map[string][]string, error) {
	keys := make([]string, len(objects))
	for i := range objects {
		keys[i] = objects[i].ObjectKey
	}
	copyMap, err := r.store.GetObjectBackendsForKeys(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("fetch copy map for rebalance planning: %w", err)
	}
	return copyMap, nil
}

// -------------------------------------------------------------------------
// SPREAD EVEN STRATEGY
// -------------------------------------------------------------------------

// backendBalance tracks a backend's excess or deficit relative to the target.
type backendBalance struct {
	Name    string
	Balance int64 // positive = over-target (source), negative = under-target (dest)
}

// PlanSpreadEven equalizes utilization ratios across backends by moving
// objects from over-utilized backends to under-utilized ones. Returns nil
// when no backend has a usable byte limit.
func (r *Rebalancer) PlanSpreadEven(ctx context.Context, stats map[string]core.QuotaStat, batchSize int) ([]RebalanceMove, error) {
	sources, destinations, simUsed, ok := computeSpreadBalances(r.ops.BackendOrder(), stats)
	if !ok {
		return nil, nil
	}
	state := newPlanState(r.ops.Usage(), simUsed, batchSize)

	var plan []RebalanceMove

	for si := range sources {
		if state.remaining <= 0 || ctx.Err() != nil {
			break
		}
		moves, err := r.spreadMovesFromSource(ctx, &sources[si], destinations, stats, state)
		if err != nil {
			return nil, err
		}
		plan = append(plan, moves...)
	}

	return plan, nil
}

// computeSpreadBalances computes each backend's excess or deficit relative
// to the fleet target ratio and partitions backends into sorted source and
// destination slices. Returns ok=false when totalLimit is zero (no quota
// data; nothing to plan).
func computeSpreadBalances(order []string, stats map[string]core.QuotaStat) ([]backendBalance, []backendBalance, map[string]int64, bool) {
	var totalUsed, totalLimit int64
	for _, name := range order {
		stat, ok := stats[name]
		if !ok {
			continue
		}
		totalUsed += stat.BytesUsed
		totalLimit += stat.BytesLimit
	}
	if totalLimit == 0 {
		return nil, nil, nil, false
	}

	targetRatio := float64(totalUsed) / float64(totalLimit)
	var sources, destinations []backendBalance
	simUsed := make(map[string]int64)

	for _, name := range order {
		stat, ok := stats[name]
		if !ok {
			continue
		}
		simUsed[name] = stat.BytesUsed
		targetBytes := int64(targetRatio * float64(stat.BytesLimit))
		excess := stat.BytesUsed - targetBytes

		switch {
		case excess > 0:
			sources = append(sources, backendBalance{Name: name, Balance: excess})
		case excess < 0:
			destinations = append(destinations, backendBalance{Name: name, Balance: excess})
		}
	}

	slices.SortFunc(sources, func(a, b backendBalance) int {
		return cmp.Compare(b.Balance, a.Balance)
	})
	slices.SortFunc(destinations, func(a, b backendBalance) int {
		return cmp.Compare(a.Balance, b.Balance)
	})
	return sources, destinations, simUsed, true
}

// spreadMovesFromSource selects spread-even moves out of one over-target
// source. For each candidate object it searches destinations for one that
// can absorb the size without overshooting target.
func (r *Rebalancer) spreadMovesFromSource(
	ctx context.Context,
	src *backendBalance,
	destinations []backendBalance,
	stats map[string]core.QuotaStat,
	state *planState,
) ([]RebalanceMove, error) {
	objects, err := r.store.ListObjectsByBackend(ctx, src.Name, state.remaining)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects on %s: %w", src.Name, err)
	}
	copyMap, err := r.fetchCopyMap(ctx, objects)
	if err != nil {
		return nil, err
	}
	placement := NewPlacementSet(copyMap)

	var moves []RebalanceMove
	for oi := range objects {
		if state.remaining <= 0 || src.Balance <= 0 {
			break
		}
		if objects[oi].SizeBytes > src.Balance {
			continue
		}
		bestDest := findSpreadDestination(&objects[oi], destinations, placement, stats, state.simUsed)
		if bestDest < 0 {
			continue
		}

		dest := &destinations[bestDest]
		if !state.allows(src.Name, dest.Name, objects[oi].SizeBytes) {
			telemetry.UsageLimitRejectionsTotal.WithLabelValues("rebalance", "transfer").Inc()
			continue
		}

		moves = append(moves, RebalanceMove{
			ObjectKey:   objects[oi].ObjectKey,
			FromBackend: src.Name,
			ToBackend:   dest.Name,
			SizeBytes:   objects[oi].SizeBytes,
		})
		src.Balance -= objects[oi].SizeBytes
		dest.Balance += objects[oi].SizeBytes
		state.accept(src.Name, dest.Name, objects[oi].SizeBytes)
	}
	return moves, nil
}

// findSpreadDestination returns the index of the first destination that
// can absorb obj.SizeBytes without overshooting its target deficit and
// that does not already hold a copy of the key. Returns -1 when no
// destination qualifies.
func findSpreadDestination(
	obj *core.ObjectLocation,
	destinations []backendBalance,
	placement PlacementSet,
	stats map[string]core.QuotaStat,
	simUsed map[string]int64,
) int {
	for di := range destinations {
		if placement.Has(obj.ObjectKey, destinations[di].Name) {
			continue
		}
		deficit := -destinations[di].Balance
		destStat := stats[destinations[di].Name]
		destFree := destStat.BytesLimit - simUsed[destinations[di].Name]
		if deficit >= obj.SizeBytes && obj.SizeBytes <= destFree {
			return di
		}
	}
	return -1
}

// -------------------------------------------------------------------------
// MOVE EXECUTION
// -------------------------------------------------------------------------

// ExecuteMoves runs the planned object moves with bounded concurrency.
// Skips individual moves that fail and continues with the rest, returning
// the count of successful moves. observer, when non-nil, receives a step per
// move labelled with the object and the backends it travels between.
func (r *Rebalancer) ExecuteMoves(ctx context.Context, plan []RebalanceMove, strategy string, concurrency int, observer progress.Observer) WorkSummary {
	runner := BatchRunner[RebalanceMove]{
		Name:        "rebalance",
		Log:         r.log,
		Concurrency: concurrency,
		Observer:    observer,
		Key:         RebalanceMove.progressLabel,
	}
	return runner.Run(ctx, plan, func(ctx context.Context, mv RebalanceMove) ItemResult {
		defer telemetry.RebalancePending.Dec()
		var res ItemResult // zero value (ItemSkipped) when admission blocks the move
		WithAdmission(ctx, r.ops, WorkerNameRebalancer, func() {
			if r.ExecuteOneMove(ctx, mv, strategy) {
				res = ItemResult{Outcome: ItemSucceeded}
			} else {
				res = ItemResult{Outcome: ItemFailed}
			}
		})
		return res
	})
}

// ExecuteOneMove performs a single object move: stream the bytes from
// source to destination, swap the DB location with compare-and-swap, and
// delete the source copy. Returns true when all steps succeed.
func (r *Rebalancer) ExecuteOneMove(ctx context.Context, move RebalanceMove, strategy string) bool {
	srcBackend, ok := r.ops.Backends()[move.FromBackend]
	if !ok {
		r.log.ErrorContext(ctx, "source backend not found", "backend", move.FromBackend)
		return false
	}

	destBackend, ok := r.ops.Backends()[move.ToBackend]
	if !ok {
		r.log.ErrorContext(ctx, "destination backend not found", "backend", move.ToBackend)
		return false
	}

	movedSize, err := r.placement.MoveObject(ctx, &writepath.MoveRequest{
		Key:         move.ObjectKey,
		SizeBytes:   move.SizeBytes,
		SrcBackend:  srcBackend,
		SrcName:     move.FromBackend,
		DestBackend: destBackend,
		DestName:    move.ToBackend,
		Reasons:     writepath.RebalanceMoveReasons,
	})
	if err != nil {
		if errors.Is(err, writepath.ErrMoveStale) {
			r.log.InfoContext(ctx, "object already moved or deleted, cleaned up",
				"key", move.ObjectKey)
			return false
		}
		r.log.WarnContext(ctx, "rebalance move failed",
			"key", move.ObjectKey, "from", move.FromBackend, "to", move.ToBackend, "error", err)
		telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "error").Inc()
		return false
	}

	audit.Log(ctx, "rebalance.move",
		slog.String("key", move.ObjectKey),
		slog.String("src_backend", move.FromBackend),
		slog.String("dst_backend", move.ToBackend),
		slog.Int64("size", movedSize),
	)

	telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "success").Inc()
	telemetry.RebalanceBytesMoved.WithLabelValues(strategy).Add(float64(movedSize))
	return true
}

// -------------------------------------------------------------------------
// USAGE BUDGET
// -------------------------------------------------------------------------

// planState is the running state of one rebalance cycle: the simulated stored
// bytes, the committed transfer, and how many moves the batch has left.
//
// Grouped because every accepted move mutates all three together. Threading
// them individually put the pack helpers past the parameter count a reader can
// hold, and left the mutations inline at each call site where dropping one -
// the source side of simUsed, say - would quietly skew every later decision in
// the same plan.
type planState struct {
	simUsed   map[string]int64
	budget    *usageBudget
	remaining int
}

func newPlanState(usage *counter.UsageTracker, simUsed map[string]int64, batchSize int) *planState {
	return &planState{simUsed: simUsed, budget: newUsageBudget(usage), remaining: batchSize}
}

// allows reports whether the plan may still move size bytes from src to dest.
func (p *planState) allows(src, dest string, size int64) bool {
	return p.remaining > 0 && p.budget.allows(src, dest, size)
}

// accept records one planned move against every counter it affects.
func (p *planState) accept(src, dest string, size int64) {
	p.budget.commit(src, dest, size)
	p.simUsed[dest] += size
	p.simUsed[src] -= size
	p.remaining--
}

// usageBudget tracks the transfer a plan has already committed so a batch of
// moves cannot collectively breach a limit that each move individually fits
// inside. It is the usage-side counterpart to simUsed, which does the same job
// for stored bytes.
//
// A move spends two different allowances on two different backends: reading the
// object is egress on the source, writing it is ingress on the destination. The
// two are checked separately because a backend can have headroom in one and
// none in the other.
type usageBudget struct {
	usage   *counter.UsageTracker
	egress  map[string]int64
	ingress map[string]int64
}

func newUsageBudget(usage *counter.UsageTracker) *usageBudget {
	return &usageBudget{
		usage:   usage,
		egress:  map[string]int64{},
		ingress: map[string]int64{},
	}
}

// allows reports whether moving size bytes from src to dest stays inside both
// backends' limits, counting what this plan has already committed.
//
// Draining is deliberately not consulted: a drain exists to move data off a
// draining backend, so excluding it as a source would stall the operation it
// is meant to perform. This is why the check is WithinLimits rather than
// EligibleForWrite, which excludes draining backends.
func (b *usageBudget) allows(src, dest string, size int64) bool {
	if b == nil || b.usage == nil {
		return true
	}
	if !b.usage.WithinLimits(src, 1, b.egress[src]+size, 0) {
		return false
	}
	return b.usage.WithinLimits(dest, 1, 0, b.ingress[dest]+size)
}

// commit records a planned move against both backends' allowances.
func (b *usageBudget) commit(src, dest string, size int64) {
	if b == nil {
		return
	}
	b.egress[src] += size
	b.ingress[dest] += size
}
