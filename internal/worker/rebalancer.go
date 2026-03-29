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

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/store"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/workerpool"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Rebalancer moves objects between backends to optimize space distribution.
// Embeds *backendCore for access to shared infrastructure.
type Rebalancer struct {
	ops Ops
	cfg syncutil.AtomicConfig[config.RebalanceConfig]
}

// NewRebalancer creates a Rebalancer that shares the given core infrastructure.
func NewRebalancer(ops Ops) *Rebalancer {
	return &Rebalancer{ops: ops}
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
	start := time.Now()
	ctx = audit.WithRequestID(ctx, audit.NewID())
	ctx, span := telemetry.StartSpan(ctx, "Rebalance",
		telemetry.AttrOperation.String("rebalance"),
	)
	defer span.End()

	audit.Log(ctx, "rebalance.start",
		slog.String("strategy", cfg.Strategy),
		slog.Int("batch_size", cfg.BatchSize),
		slog.Float64("threshold", cfg.Threshold),
	)

	// --- Get current quota stats ---
	stats, err := r.ops.Store().GetQuotaStats(ctx)
	if err != nil {
		telemetry.RebalanceRunsTotal.WithLabelValues(cfg.Strategy, "error").Inc()
		return 0, fmt.Errorf("failed to get quota stats: %w", err)
	}

	// --- Check threshold ---
	if !ExceedsThreshold(stats, r.ops.BackendOrder(), cfg.Threshold) {
		slog.InfoContext(ctx, "Rebalance skipping, within threshold",
			"threshold", cfg.Threshold, "strategy", cfg.Strategy)
		telemetry.RebalanceSkipped.WithLabelValues("threshold").Inc()
		return 0, nil
	}

	// --- Compute plan ---
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
		slog.InfoContext(ctx, "Rebalance skipping, empty plan", "strategy", cfg.Strategy)
		telemetry.RebalanceSkipped.WithLabelValues("empty_plan").Inc()
		return 0, nil
	}

	// --- Execute moves ---
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
}

// -------------------------------------------------------------------------
// THRESHOLD CHECK
// -------------------------------------------------------------------------

// exceedsThreshold returns true if the utilization spread across backends
// exceeds the configured threshold.
func ExceedsThreshold(stats map[string]store.QuotaStat, order []string, threshold float64) bool {
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

// planPackTight consolidates objects onto the most-utilized backends, pulling
// from the least-utilized. Sorts by percent full descending and only moves
// objects from a less-full source to a more-full destination. Skips moves
// that would not increase the destination's packing ratio.
func (r *Rebalancer) PlanPackTight(ctx context.Context, stats map[string]store.QuotaStat, batchSize int) ([]RebalanceMove, error) {
	type backendUtil struct {
		Name  string
		Limit int64
	}

	// --- Build sorted backend list (most full first) ---
	simUsed := make(map[string]int64)
	var backends []backendUtil

	for _, name := range r.ops.BackendOrder() {
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
		return cmp.Compare(rb, ra) // descending
	})

	var plan []RebalanceMove
	remaining := batchSize

	// Cache object lists per source to avoid re-querying when the same
	// source is visited across multiple destination iterations.
	objectCache := make(map[string][]store.ObjectLocation)

	// --- Pack into most-full destinations, pulling from least-full sources ---
	for di := 0; di < len(backends) && remaining > 0; di++ {
		if ctx.Err() != nil {
			return plan, ctx.Err()
		}
		dest := backends[di]
		destFree := dest.Limit - simUsed[dest.Name]
		if destFree <= 0 {
			continue
		}

		for si := len(backends) - 1; si > di && remaining > 0 && destFree > 0; si-- {
			src := backends[si]

			// Only pull from backends that are less utilized
			srcRatio := float64(simUsed[src.Name]) / float64(src.Limit)
			destRatio := float64(simUsed[dest.Name]) / float64(dest.Limit)
			if srcRatio >= destRatio {
				continue
			}

			objects, ok := objectCache[src.Name]
			if !ok {
				var err error
				objects, err = r.ops.Store().ListObjectsByBackend(ctx, src.Name, remaining)
				if err != nil {
					return nil, fmt.Errorf("failed to list objects on %s: %w", src.Name, err)
				}
				objectCache[src.Name] = objects
			}

			for oi := range objects {
				if remaining <= 0 || destFree <= 0 {
					break
				}
				if objects[oi].SizeBytes > destFree {
					continue
				}

				// Re-check ratios after prior simulated moves
				srcRatio = float64(simUsed[src.Name]) / float64(src.Limit)
				destRatio = float64(simUsed[dest.Name]) / float64(dest.Limit)
				if srcRatio >= destRatio {
					break // Source is now as full or fuller, stop pulling
				}

				plan = append(plan, RebalanceMove{
					ObjectKey:   objects[oi].ObjectKey,
					FromBackend: src.Name,
					ToBackend:   dest.Name,
					SizeBytes:   objects[oi].SizeBytes,
				})

				destFree -= objects[oi].SizeBytes
				simUsed[dest.Name] += objects[oi].SizeBytes
				simUsed[src.Name] -= objects[oi].SizeBytes
				remaining--
			}
		}
	}

	return plan, nil
}

// -------------------------------------------------------------------------
// SPREAD EVEN STRATEGY
// -------------------------------------------------------------------------

// backendBalance tracks a backend's excess or deficit relative to the target.
type backendBalance struct {
	Name    string
	Balance int64 // positive = over-target (source), negative = under-target (dest)
}

// planSpreadEven equalizes utilization ratios across backends. Moves objects
// from over-utilized backends to under-utilized ones.
func (r *Rebalancer) PlanSpreadEven(ctx context.Context, stats map[string]store.QuotaStat, batchSize int) ([]RebalanceMove, error) {
	var totalUsed, totalLimit int64
	for _, name := range r.ops.BackendOrder() {
		stat, ok := stats[name]
		if !ok {
			continue
		}
		totalUsed += stat.BytesUsed
		totalLimit += stat.BytesLimit
	}

	if totalLimit == 0 {
		return nil, nil
	}

	targetRatio := float64(totalUsed) / float64(totalLimit)

	// Compute excess/deficit for each backend
	var sources, destinations []backendBalance
	simUsed := make(map[string]int64)

	for _, name := range r.ops.BackendOrder() {
		stat, ok := stats[name]
		if !ok {
			continue
		}
		simUsed[name] = stat.BytesUsed
		targetBytes := int64(targetRatio * float64(stat.BytesLimit))
		excess := stat.BytesUsed - targetBytes

		if excess > 0 {
			sources = append(sources, backendBalance{Name: name, Balance: excess})
		} else if excess < 0 {
			destinations = append(destinations, backendBalance{Name: name, Balance: excess})
		}
	}

	// Sort: most over-target sources first, most under-target destinations first
	slices.SortFunc(sources, func(a, b backendBalance) int {
		return cmp.Compare(b.Balance, a.Balance) // descending
	})
	slices.SortFunc(destinations, func(a, b backendBalance) int {
		return cmp.Compare(a.Balance, b.Balance) // ascending (most negative first)
	})

	var plan []RebalanceMove
	remaining := batchSize

	for si := range sources {
		if remaining <= 0 || ctx.Err() != nil {
			break
		}

		src := &sources[si]
		objects, err := r.ops.Store().ListObjectsByBackend(ctx, src.Name, remaining)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects on %s: %w", src.Name, err)
		}

		for oi := range objects {
			if remaining <= 0 || src.Balance <= 0 {
				break
			}

			// Skip if this object is larger than what the source needs to shed —
			// moving it would overshoot and make the source under-target
			if objects[oi].SizeBytes > src.Balance {
				continue
			}

			// Find best destination that can fit this object without overshooting
			bestDest := -1
			for di := range destinations {
				deficit := -destinations[di].Balance
				destStat := stats[destinations[di].Name]
				destFree := destStat.BytesLimit - simUsed[destinations[di].Name]

				if deficit >= objects[oi].SizeBytes && objects[oi].SizeBytes <= destFree {
					bestDest = di
					break
				}
			}

			if bestDest < 0 {
				continue
			}

			dest := &destinations[bestDest]
			plan = append(plan, RebalanceMove{
				ObjectKey:   objects[oi].ObjectKey,
				FromBackend: src.Name,
				ToBackend:   dest.Name,
				SizeBytes:   objects[oi].SizeBytes,
			})

			src.Balance -= objects[oi].SizeBytes
			dest.Balance += objects[oi].SizeBytes
			simUsed[src.Name] -= objects[oi].SizeBytes
			simUsed[dest.Name] += objects[oi].SizeBytes
			remaining--
		}
	}

	return plan, nil
}

// -------------------------------------------------------------------------
// MOVE EXECUTION
// -------------------------------------------------------------------------

// executeMoves runs the planned object moves with bounded concurrency.
// Skips individual moves that fail and continues with the rest.
func (r *Rebalancer) ExecuteMoves(ctx context.Context, plan []RebalanceMove, strategy string, concurrency int) int {
	var moved atomic.Int32
	workerpool.Run(ctx, concurrency, plan, func(ctx context.Context, mv RebalanceMove) {
		if !r.ops.AcquireAdmission(ctx) {
			telemetry.WorkerAdmissionRejectionsTotal.WithLabelValues("rebalancer").Inc()
			return
		}
		defer r.ops.ReleaseAdmission()
		if r.ExecuteOneMove(ctx, mv, strategy) {
			moved.Add(1)
		}
	})
	return int(moved.Load())
}

// executeOneMove performs a single object move: read from source, write to
// destination, swap the DB location, and delete the source copy. Returns
// true on success.
func (r *Rebalancer) ExecuteOneMove(ctx context.Context, move RebalanceMove, strategy string) bool {
	srcBackend, ok := r.ops.Backends()[move.FromBackend]
	if !ok {
		slog.ErrorContext(ctx, "Rebalance: source backend not found", "backend", move.FromBackend)
		return false
	}

	destBackend, ok := r.ops.Backends()[move.ToBackend]
	if !ok {
		slog.ErrorContext(ctx, "Rebalance: destination backend not found", "backend", move.ToBackend)
		return false
	}

	// --- Stream source to destination ---
	if err := r.ops.StreamCopy(ctx, srcBackend, destBackend, move.ObjectKey); err != nil {
		slog.WarnContext(ctx, "Rebalance: stream copy failed",
			"key", move.ObjectKey, "from", move.FromBackend, "to", move.ToBackend, "error", err)
		telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "error").Inc()
		return false
	}

	// --- Atomic DB update (compare-and-swap) ---
	movedSize, err := r.ops.Store().MoveObjectLocation(ctx, move.ObjectKey, move.FromBackend, move.ToBackend)
	if err != nil {
		slog.ErrorContext(ctx, "Rebalance: failed to update object location",
			"key", move.ObjectKey, "error", err)
		// Clean up orphan on destination
		r.ops.DeleteOrEnqueue(ctx, destBackend, move.ToBackend, move.ObjectKey, "rebalance_orphan", move.SizeBytes)
		r.ops.Usage().Record(move.ToBackend, 1, 0, 0)
		telemetry.RebalanceObjectsMoved.WithLabelValues(strategy, "error").Inc()
		return false
	}

	if movedSize == 0 {
		// Object was deleted or already moved by another process
		slog.InfoContext(ctx, "Rebalance: object already moved or deleted, cleaning up",
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
