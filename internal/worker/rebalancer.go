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
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Rebalancer moves objects between backends to optimize space distribution.
type Rebalancer struct {
	log   *slog.Logger
	ops   Ops
	store RebalancerStore
	cfg   syncutil.AtomicConfig[config.RebalanceConfig]
}

// NewRebalancer creates a Rebalancer with fleet operations and a narrow store.
func NewRebalancer(ops Ops, store RebalancerStore) *Rebalancer {
	return &Rebalancer{ops: ops, store: store, log: slog.Default().With(logfmt.Component("rebalancer"))}
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

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Rebalance moves objects between backends to optimize space distribution.
// Returns the number of objects successfully moved.
func (r *Rebalancer) Rebalance(ctx context.Context, cfg config.RebalanceConfig) (int, error) {
	return runOpsCycle(ctx, "Rebalance", "rebalance", func(ctx context.Context) (int, error) {
		start := time.Now()
		audit.Log(ctx, "rebalance.start",
			slog.String("strategy", cfg.Strategy),
			slog.Int("batch_size", cfg.BatchSize),
			slog.Float64("threshold", cfg.Threshold),
		)

		stats, err := r.store.GetQuotaStats(ctx)
		if err != nil {
			telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, "error").Inc()
			return 0, fmt.Errorf("failed to get quota stats: %w", err)
		}

		if !ExceedsThreshold(stats, r.ops.BackendOrder(), cfg.Threshold) {
			r.log.InfoContext(ctx, "rebalance skipping, within threshold",
				"threshold", cfg.Threshold, "strategy", cfg.Strategy)
			telemetry.RebalanceSkipped.WithLabelValues("threshold").Inc()
			return 0, nil
		}

		var plan []RebalanceMove
		switch cfg.Strategy {
		case "pack":
			plan, err = r.PlanPackTight(ctx, stats, cfg.BatchSize)
		case "spread":
			plan, err = r.PlanSpreadEven(ctx, stats, cfg.BatchSize)
		default:
			return 0, fmt.Errorf("unknown rebalance strategy: %s", cfg.Strategy)
		}
		if err != nil {
			telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, "error").Inc()
			return 0, fmt.Errorf("failed to plan rebalance: %w", err)
		}

		telemetry.RebalancePending.Set(float64(len(plan)))

		if len(plan) == 0 {
			r.log.InfoContext(ctx, "rebalance skipping, empty plan", "strategy", cfg.Strategy)
			telemetry.RebalanceSkipped.WithLabelValues("empty_plan").Inc()
			return 0, nil
		}

		moved := r.ExecuteMoves(ctx, plan, cfg.Strategy, cfg.Concurrency)

		telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, "success").Inc()
		telemetry.RebalanceDuration.WithLabelValues(cfg.Strategy).Observe(time.Since(start).Seconds())

		audit.Log(ctx, "rebalance.complete",
			slog.String("strategy", cfg.Strategy),
			slog.Int("objects_moved", moved),
			slog.Int("planned", len(plan)),
			slog.Duration("duration", time.Since(start)),
		)
		return moved, nil
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

	var plan []RebalanceMove
	remaining := batchSize
	objectCache := make(map[string][]core.ObjectLocation)

	for di := 0; di < len(backends) && remaining > 0; di++ {
		if ctx.Err() != nil {
			return plan, ctx.Err()
		}
		moves, err := r.packMovesIntoDestination(ctx, di, backends, simUsed, objectCache, &remaining)
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
	simUsed map[string]int64,
	objectCache map[string][]core.ObjectLocation,
	remaining *int,
) ([]RebalanceMove, error) {
	dest := backends[di]
	destFree := dest.Limit - simUsed[dest.Name]
	if destFree <= 0 {
		return nil, nil
	}

	var plan []RebalanceMove
	for si := len(backends) - 1; si > di && *remaining > 0 && destFree > 0; si-- {
		src := backends[si]
		if !srcLessUtilized(src, dest, simUsed) {
			continue
		}

		objects, err := r.cachedSourceObjects(ctx, src.Name, *remaining, objectCache)
		if err != nil {
			return nil, err
		}
		copyMap := r.fetchCopyMap(ctx, objects)

		moves := r.packMovesFromSource(src, dest, objects, copyMap, simUsed, &destFree, remaining)
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
	copyMap map[string][]string,
	simUsed map[string]int64,
	destFree *int64,
	remaining *int,
) []RebalanceMove {
	var moves []RebalanceMove
	for oi := range objects {
		if *remaining <= 0 || *destFree <= 0 {
			break
		}
		if objects[oi].SizeBytes > *destFree {
			continue
		}
		if !srcLessUtilized(src, dest, simUsed) {
			break
		}
		if slices.Contains(copyMap[objects[oi].ObjectKey], dest.Name) {
			continue
		}

		moves = append(moves, RebalanceMove{
			ObjectKey:   objects[oi].ObjectKey,
			FromBackend: src.Name,
			ToBackend:   dest.Name,
			SizeBytes:   objects[oi].SizeBytes,
		})
		*destFree -= objects[oi].SizeBytes
		simUsed[dest.Name] += objects[oi].SizeBytes
		simUsed[src.Name] -= objects[oi].SizeBytes
		*remaining--
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

// cachedSourceObjects returns ListObjectsByBackend results for a source,
// caching the slice across destination iterations to avoid the same source
// being re-queried by the outer pack loop.
func (r *Rebalancer) cachedSourceObjects(
	ctx context.Context,
	src string,
	limit int,
	cache map[string][]core.ObjectLocation,
) ([]core.ObjectLocation, error) {
	if objs, ok := cache[src]; ok {
		return objs, nil
	}
	objs, err := r.store.ListObjectsByBackend(ctx, src, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects on %s: %w", src, err)
	}
	cache[src] = objs
	return objs, nil
}

// fetchCopyMap batches GetObjectBackendsForKeys for every candidate key.
// Replaces a per-object GetAllObjectLocations call that the inner loop
// would otherwise issue.
func (r *Rebalancer) fetchCopyMap(ctx context.Context, objects []core.ObjectLocation) map[string][]string {
	keys := make([]string, len(objects))
	for i := range objects {
		keys[i] = objects[i].ObjectKey
	}
	copyMap, _ := r.store.GetObjectBackendsForKeys(ctx, keys)
	return copyMap
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

	var plan []RebalanceMove
	remaining := batchSize

	for si := range sources {
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		moves, err := r.spreadMovesFromSource(ctx, &sources[si], destinations, stats, simUsed, &remaining)
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
	simUsed map[string]int64,
	remaining *int,
) ([]RebalanceMove, error) {
	objects, err := r.store.ListObjectsByBackend(ctx, src.Name, *remaining)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects on %s: %w", src.Name, err)
	}
	copyMap := r.fetchCopyMap(ctx, objects)

	var moves []RebalanceMove
	for oi := range objects {
		if *remaining <= 0 || src.Balance <= 0 {
			break
		}
		if objects[oi].SizeBytes > src.Balance {
			continue
		}
		bestDest := findSpreadDestination(&objects[oi], destinations, copyMap, stats, simUsed)
		if bestDest < 0 {
			continue
		}

		dest := &destinations[bestDest]
		moves = append(moves, RebalanceMove{
			ObjectKey:   objects[oi].ObjectKey,
			FromBackend: src.Name,
			ToBackend:   dest.Name,
			SizeBytes:   objects[oi].SizeBytes,
		})
		src.Balance -= objects[oi].SizeBytes
		dest.Balance += objects[oi].SizeBytes
		simUsed[src.Name] -= objects[oi].SizeBytes
		simUsed[dest.Name] += objects[oi].SizeBytes
		*remaining--
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
	copyMap map[string][]string,
	stats map[string]core.QuotaStat,
	simUsed map[string]int64,
) int {
	copySet := make(map[string]bool, len(copyMap[obj.ObjectKey]))
	for _, b := range copyMap[obj.ObjectKey] {
		copySet[b] = true
	}
	for di := range destinations {
		if copySet[destinations[di].Name] {
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
// the count of successful moves.
func (r *Rebalancer) ExecuteMoves(ctx context.Context, plan []RebalanceMove, strategy string, concurrency int) int {
	var moved atomic.Int32
	workerpool.Run(ctx, concurrency, plan, func(ctx context.Context, mv RebalanceMove) {
		defer telemetry.RebalancePending.Dec()
		WithAdmission(ctx, r.ops, WorkerNameRebalancer, func() {
			if r.ExecuteOneMove(ctx, mv, strategy) {
				moved.Add(1)
			}
		})
	})
	return int(moved.Load())
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

	// --- Stream source to destination ---
	if err := r.ops.StreamCopy(ctx, srcBackend, destBackend, move.ObjectKey); err != nil {
		r.log.WarnContext(ctx, "stream copy failed",
			"key", move.ObjectKey, "from", move.FromBackend, "to", move.ToBackend, "error", err)
		telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "error").Inc()
		return false
	}

	// --- Atomic DB update (compare-and-swap) ---
	movedSize, err := r.store.MoveObjectLocation(ctx, move.ObjectKey, move.FromBackend, move.ToBackend)
	if err != nil {
		r.log.ErrorContext(ctx, "failed to update object location",
			"key", move.ObjectKey, "error", err)
		// Clean up orphan on destination
		r.ops.DeleteOrEnqueue(ctx, destBackend, move.ToBackend, move.ObjectKey, "rebalance_orphan", move.SizeBytes)
		r.ops.Usage().Record(move.ToBackend, 1, 0, 0)
		telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "error").Inc()
		return false
	}

	if movedSize == 0 {
		// Object was deleted or already moved by another process
		r.log.InfoContext(ctx, "object already moved or deleted, cleaning up",
			"key", move.ObjectKey)
		r.ops.DeleteOrEnqueue(ctx, destBackend, move.ToBackend, move.ObjectKey, "rebalance_stale_orphan", move.SizeBytes)
		r.ops.Usage().Record(move.ToBackend, 1, 0, 0)
		return false
	}

	// --- Delete from source ---
	r.ops.DeleteOrEnqueue(ctx, srcBackend, move.FromBackend, move.ObjectKey, "rebalance_source_delete", movedSize)

	r.ops.Usage().Record(move.FromBackend, 2, movedSize, 0) // Get + Delete, egress
	r.ops.Usage().Record(move.ToBackend, 1, 0, movedSize)   // Put, ingress

	audit.Log(ctx, "rebalance.move",
		slog.String("key", move.ObjectKey),
		slog.String("from_backend", move.FromBackend),
		slog.String("to_backend", move.ToBackend),
		slog.Int64("size", movedSize),
	)

	telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "success").Inc()
	telemetry.RebalanceBytesMoved.WithLabelValues(strategy).Add(float64(movedSize))
	return true
}
