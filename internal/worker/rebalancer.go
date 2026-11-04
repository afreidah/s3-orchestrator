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

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/cycle"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/progress"
	"github.com/afreidah/s3-orchestrator/internal/proxy/writepath"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/batch"
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
// The embedded Section reads its rebalance config from the running
// configuration.
type Rebalancer struct {
	syncutil.Section[config.RebalanceConfig]
	d   RebalancerDeps
	log *slog.Logger
}

// RebalancerDeps groups the rebalancer's constructor dependencies: the fleet,
// the metadata store, and the rebalance section of the running configuration.
type RebalancerDeps struct {
	Fleet
	Store  RebalancerStore
	Config syncutil.Section[config.RebalanceConfig]
}

// NewRebalancer creates a Rebalancer from its dependencies, all of which are
// required.
//
//nolint:gocritic // hugeParam: the Deps are copied once, at construction.
func NewRebalancer(d RebalancerDeps) *Rebalancer {
	d.mustBeWired()
	must.NotNil("d.Store", d.Store)
	must.NotNil("d.Config", d.Config)
	return &Rebalancer{Section: d.Config, d: d, log: logfmt.Logger(string(audit.WorkloadRebalance))}
}

// rebalanceMove describes a single object move from one backend to another.
//
// Source is the copy the plan read; the move commits only while it is still
// the copy recorded on its backend. The destination path is minted when the
// move runs, not when it is planned, since other writes can take the key while
// a plan waits.
type rebalanceMove struct {
	Source    *core.ObjectLocation
	ToBackend string
}

// progressLabel names the move for a streaming caller: the object and the
// backends it travels between, which is the whole of what a rebalance does.
func (m rebalanceMove) progressLabel() string {
	return fmt.Sprintf("%s  %s -> %s", m.Source.ObjectKey, m.Source.BackendName, m.ToBackend)
}

// Why a cycle declined to move anything. Reported instead of a zero move count,
// which a caller cannot tell apart from a cycle that ran and found nothing.
const (
	SkipReasonWithinThreshold = "backend utilization is already within the rebalance threshold"
	SkipReasonEmptyPlan       = "the rebalance strategy planned no moves"
)

// RebalanceSummary is one cycle's outcome. SkipReason is set when the cycle
// never planned any moves; the embedded Summary is zero in that case.
type RebalanceSummary struct {
	batch.Summary
	SkipReason string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Rebalance moves objects between backends to optimize space distribution.
// observer, when non-nil, receives a step per planned move so a streaming
// caller can report progress as the cycle runs. A cycle that attempted a move
// publishes RebalanceCompleted, whether a tick or an operator ran it.
func (r *Rebalancer) Rebalance(ctx context.Context, cfg config.RebalanceConfig, observer progress.Observer) (RebalanceSummary, error) {
	pass := cycle.Pass{Workload: audit.WorkloadRebalance, Refresh: r.d.Metrics}
	return cycle.Run(ctx, pass, func(ctx context.Context) (RebalanceSummary, error) {
		audit.Log(ctx, "rebalance.start",
			slog.String("strategy", string(cfg.Strategy)),
			slog.Int("batch_size", cfg.BatchSize),
			slog.Float64("threshold", cfg.Threshold),
		)

		stats, err := r.d.Store.GetQuotaStats(ctx)
		if err != nil {
			return RebalanceSummary{}, fmt.Errorf("failed to get quota stats: %w", err)
		}

		if !exceedsThreshold(stats, r.d.Ops.BackendOrder(), cfg.Threshold) {
			r.log.InfoContext(ctx, "rebalance skipping, within threshold",
				"threshold", cfg.Threshold, "strategy", cfg.Strategy)
			telemetry.RebalanceSkipped.WithLabelValues("threshold").Inc()
			return RebalanceSummary{SkipReason: SkipReasonWithinThreshold}, nil
		}

		var plan []rebalanceMove
		switch cfg.Strategy {
		case config.RoutingPack:
			plan, err = r.planPackTight(ctx, stats, cfg.BatchSize)
		case config.RoutingSpread:
			plan, err = r.planSpreadEven(ctx, stats, cfg.BatchSize)
		default:
			return RebalanceSummary{}, fmt.Errorf("unknown rebalance strategy: %s", cfg.Strategy)
		}
		if err != nil {
			return RebalanceSummary{}, fmt.Errorf("failed to plan rebalance: %w", err)
		}

		r.publishPending(ctx, int64(len(plan)))

		if len(plan) == 0 {
			r.log.InfoContext(ctx, "rebalance skipping, empty plan", "strategy", cfg.Strategy)
			telemetry.RebalanceSkipped.WithLabelValues("empty_plan").Inc()
			return RebalanceSummary{SkipReason: SkipReasonEmptyPlan}, nil
		}

		sum := r.executeMoves(ctx, plan, cfg.Strategy, cfg.Concurrency, observer)
		r.publishPending(ctx, 0)
		if sum.Attempted > 0 {
			event.Publish(event.RebalanceCompleted, "", map[string]any{"moved": sum.Succeeded})
		}
		return RebalanceSummary{Summary: sum}, nil
	})
}

// publishPending shares how many moves the current pass has planned: the plan
// size when it starts, zero when it ends.
func (r *Rebalancer) publishPending(ctx context.Context, n int64) {
	r.d.Metrics.PublishWorkerGauges(ctx, telemetry.GaugeSourceRebalancer, telemetry.WorkerGauges{RebalancePending: &n})
}

// -------------------------------------------------------------------------
// THRESHOLD CHECK
// -------------------------------------------------------------------------

// exceedsThreshold reports whether the utilization spread across backends
// (max ratio minus min ratio) is at least the configured threshold.
func exceedsThreshold(stats map[string]core.QuotaStat, order []string, threshold float64) bool {
	if len(order) < 2 {
		return false
	}

	var ratios []float64
	for _, name := range order {
		if stat, ok := stats[name]; ok && stat.BytesLimit != 0 {
			ratios = append(ratios, float64(stat.BytesUsed)/float64(stat.BytesLimit))
		}
	}
	if len(ratios) == 0 {
		return threshold <= 0
	}
	return slices.Max(ratios)-slices.Min(ratios) >= threshold
}

// -------------------------------------------------------------------------
// PACK TIGHT STRATEGY
// -------------------------------------------------------------------------

// planPackTight consolidates objects onto the most-utilized backends by
// pulling from the least-utilized. Sorts by percent full descending and
// only moves an object from a less-full source to a more-full destination.
// Skips moves that would not increase the destination's packing ratio.
func (r *Rebalancer) planPackTight(ctx context.Context, stats map[string]core.QuotaStat, batchSize int) ([]rebalanceMove, error) {
	state := r.newPlanState(stats, batchSize)
	backends := state.limitedByUtilDesc(r.d.Ops.BackendOrder())

	var plan []rebalanceMove
	candidates := r.newCandidateCache(batchSize, state.holders)

	for di := 0; di < len(backends) && state.remaining > 0; di++ {
		if ctx.Err() != nil {
			return plan, ctx.Err()
		}
		moves, err := packMovesIntoDestination(ctx, di, backends, state, candidates)
		if err != nil {
			return nil, err
		}
		plan = append(plan, moves...)
	}

	return plan, nil
}

// packMovesIntoDestination plans moves into backends[di] from less-full
// sources, walking sources from least-full upward.
func packMovesIntoDestination(
	ctx context.Context,
	di int,
	backends []string,
	state *planState,
	candidates *candidateCache,
) ([]rebalanceMove, error) {
	dest := backends[di]
	if state.room(dest) <= 0 || !state.takes(dest) {
		return nil, nil
	}

	var plan []rebalanceMove
	for si := len(backends) - 1; si > di && state.remaining > 0 && state.room(dest) > 0; si-- {
		src := backends[si]
		if state.ratio(src) >= state.ratio(dest) {
			continue
		}

		objects, err := candidates.forBackend(ctx, src)
		if err != nil {
			return nil, err
		}
		plan = append(plan, packMovesFromSource(src, dest, objects, state)...)
	}
	return plan, nil
}

// packMovesFromSource walks one source's candidate objects and selects moves
// that fit dest, and that keep src strictly less utilized than dest after the
// simulated transfer.
func packMovesFromSource(src, dest string, objects []core.ObjectLocation, state *planState) []rebalanceMove {
	var moves []rebalanceMove
	for oi := range objects {
		if state.remaining <= 0 || state.room(dest) <= 0 {
			break
		}
		if objects[oi].SizeBytes > state.room(dest) {
			continue
		}
		if state.ratio(src) >= state.ratio(dest) {
			break
		}
		if move, ok := state.plan(&objects[oi], dest); ok {
			moves = append(moves, move)
		}
	}
	return moves
}

// -------------------------------------------------------------------------
// SPREAD EVEN STRATEGY
// -------------------------------------------------------------------------

// backendBalance tracks a backend's excess or deficit relative to the target.
type backendBalance struct {
	Name    string
	Balance int64 // positive = over-target (source), negative = under-target (dest)
}

// planSpreadEven equalizes utilization ratios across backends by moving
// objects from over-utilized backends to under-utilized ones. Returns nil
// when no backend has a usable byte limit.
func (r *Rebalancer) planSpreadEven(ctx context.Context, stats map[string]core.QuotaStat, batchSize int) ([]rebalanceMove, error) {
	sources, destinations, ok := computeSpreadBalances(r.d.Ops.BackendOrder(), stats)
	if !ok {
		return nil, nil
	}
	state := r.newPlanState(stats, batchSize)
	destinations = slices.DeleteFunc(destinations, func(d backendBalance) bool { return !state.takes(d.Name) })

	var plan []rebalanceMove
	candidates := r.newCandidateCache(batchSize, state.holders)

	for si := range sources {
		if state.remaining <= 0 || ctx.Err() != nil {
			break
		}
		moves, err := spreadMovesFromSource(ctx, &sources[si], destinations, state, candidates)
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
func computeSpreadBalances(order []string, stats map[string]core.QuotaStat) ([]backendBalance, []backendBalance, bool) {
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
		return nil, nil, false
	}

	targetRatio := float64(totalUsed) / float64(totalLimit)
	var sources, destinations []backendBalance

	for _, name := range order {
		stat, ok := stats[name]
		if !ok {
			continue
		}
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
	return sources, destinations, true
}

// spreadMovesFromSource selects spread-even moves out of one over-target
// source. For each candidate object it searches destinations for one that
// can absorb the size without overshooting target.
func spreadMovesFromSource(
	ctx context.Context,
	src *backendBalance,
	destinations []backendBalance,
	state *planState,
	candidates *candidateCache,
) ([]rebalanceMove, error) {
	objects, err := candidates.forBackend(ctx, src.Name)
	if err != nil {
		return nil, err
	}

	var moves []rebalanceMove
	for oi := range objects {
		if state.remaining <= 0 || src.Balance <= 0 {
			break
		}
		if objects[oi].SizeBytes > src.Balance {
			continue
		}
		bestDest := findSpreadDestination(&objects[oi], destinations, state)
		if bestDest < 0 {
			continue
		}

		dest := &destinations[bestDest]
		move, ok := state.plan(&objects[oi], dest.Name)
		if !ok {
			continue
		}
		moves = append(moves, move)
		src.Balance -= objects[oi].SizeBytes
		dest.Balance += objects[oi].SizeBytes
	}
	return moves, nil
}

// findSpreadDestination returns the index of the first destination the object
// may move to that can absorb its size without overshooting the target deficit.
// Returns -1 when none qualifies.
func findSpreadDestination(obj *core.ObjectLocation, destinations []backendBalance, state *planState) int {
	for di := range destinations {
		deficit := -destinations[di].Balance
		if state.movable(obj, destinations[di].Name) && deficit >= obj.SizeBytes && obj.SizeBytes <= state.room(destinations[di].Name) {
			return di
		}
	}
	return -1
}

// -------------------------------------------------------------------------
// MOVE EXECUTION
// -------------------------------------------------------------------------

// executeMoves runs the planned object moves with bounded concurrency.
// Skips individual moves that fail and continues with the rest, returning
// the tally. observer, when non-nil, receives a step per move labelled with
// the object and the backends it travels between.
func (r *Rebalancer) executeMoves(ctx context.Context, plan []rebalanceMove, strategy config.RoutingStrategy, concurrency int, observer progress.Observer) batch.Summary {
	runner := batch.Runner[rebalanceMove]{
		Concurrency: concurrency,
		Admission:   r.d.Ops,
		Observer:    observer,
		Key:         rebalanceMove.progressLabel,
	}
	return runner.Run(ctx, plan, func(ctx context.Context, mv rebalanceMove) batch.ItemResult {
		return r.executeOneMove(ctx, mv, strategy)
	})
}

// executeOneMove performs a single object move: stream the bytes from
// source to destination, place the copy there while removing the source row,
// and delete the source bytes. A move the source's change or the target
// declined counts as a skip, since nothing went wrong with the transfer.
func (r *Rebalancer) executeOneMove(ctx context.Context, move rebalanceMove, strategy config.RoutingStrategy) batch.ItemResult {
	label := string(strategy)
	key, from := move.Source.ObjectKey, move.Source.BackendName
	movedSize, err := r.d.Placement.MoveObject(ctx, &writepath.MoveRequest{
		Source:   move.Source,
		DestName: move.ToBackend,
		Profile:  writepath.RebalanceMove,
	})
	switch {
	case core.PlacementDeclined(err):
		r.log.InfoContext(ctx, "object changed or target refused it, its copy queued for cleanup",
			"key", key, "to", move.ToBackend, "reason", err)
	case err != nil:
		r.log.WarnContext(ctx, "rebalance move failed",
			"key", key, "from", from, "to", move.ToBackend, "error", err)
		telemetry.RebalanceObjectsMoved.WithLabelValues(label, "error").Inc()
	default:
		audit.Log(ctx, "rebalance.move",
			slog.String("key", key),
			slog.String("src_backend", from),
			slog.String("dst_backend", move.ToBackend),
			slog.Int64("size", movedSize),
		)
		telemetry.RebalanceObjectsMoved.WithLabelValues(label, "success").Inc()
		telemetry.RebalanceBytesMoved.WithLabelValues(label).Add(float64(movedSize))
	}
	return moveResult(err)
}

// -------------------------------------------------------------------------
// PLAN STATE AND USAGE BUDGET
// -------------------------------------------------------------------------

// planState is the running state of one rebalance cycle: the simulated stored
// bytes, the committed transfer, where keys live, which copies are already
// leaving, and how many moves the batch has left. plan is the one way to change
// them, so every move sees the moves before it. stats and targets are fixed for
// the cycle.
type planState struct {
	stats     map[string]core.QuotaStat
	simUsed   map[string]int64
	budget    *usageBudget
	targets   map[string]bool
	holders   placementSet
	leaving   placementSet
	remaining int
}

// newPlanState starts a cycle from the fleet as it stands, taking the write
// targets once so every move in the plan is judged against the same set.
func (r *Rebalancer) newPlanState(stats map[string]core.QuotaStat, batchSize int) *planState {
	simUsed := make(map[string]int64, len(stats))
	for name := range stats {
		simUsed[name] = stats[name].BytesUsed
	}
	targets := make(map[string]bool)
	for _, name := range r.d.Ops.WriteTargets() {
		targets[name] = true
	}
	return &planState{
		stats:     stats,
		simUsed:   simUsed,
		budget:    newUsageBudget(r.d.Ops.Usage()),
		targets:   targets,
		holders:   placementSet{},
		leaving:   placementSet{},
		remaining: batchSize,
	}
}

// takes reports whether dest may receive moves, which is whether it is a
// write target: a move gives it new bytes the same way a client write does.
func (p *planState) takes(dest string) bool {
	return p.targets[dest]
}

// room is the bytes dest can still take. It leaves out dest's unrecorded bytes,
// which its claims count against a move.
func (p *planState) room(dest string) int64 {
	stat := p.stats[dest]
	return stat.BytesLimit - stat.Unrecorded() - p.simUsed[dest]
}

// ratio is how full name is, counting the moves planned so far.
func (p *planState) ratio(name string) float64 {
	return float64(p.simUsed[name]) / float64(p.stats[name].BytesLimit)
}

// limitedByUtilDesc returns the backends in order that have a byte limit, most
// utilized first.
func (p *planState) limitedByUtilDesc(order []string) []string {
	backends := slices.DeleteFunc(slices.Clone(order), func(name string) bool { return p.stats[name].BytesLimit == 0 })
	slices.SortFunc(backends, func(a, b string) int { return cmp.Compare(p.ratio(b), p.ratio(a)) })
	return backends
}

// movable reports whether obj can still be planned onto dest: no other move
// already takes this copy, and dest neither holds the key nor has a move
// bringing it there. A moved key stays recorded on its source, since moves run
// concurrently and the source may still hold it when a later move runs.
func (p *planState) movable(obj *core.ObjectLocation, dest string) bool {
	return !p.leaving.Has(obj.ObjectKey, obj.BackendName) && !p.holders.Has(obj.ObjectKey, dest)
}

// plan accepts a move of obj to dest, recording it against everything it
// affects, or declines one the object or the usage budget rules out.
func (p *planState) plan(obj *core.ObjectLocation, dest string) (rebalanceMove, bool) {
	if !p.movable(obj, dest) {
		return rebalanceMove{}, false
	}
	src, size := obj.BackendName, obj.SizeBytes
	if !p.budget.allows(src, dest, size) {
		telemetry.UsageLimitRejectionsTotal.WithLabelValues("rebalance", "transfer").Inc()
		return rebalanceMove{}, false
	}
	p.budget.commit(src, dest, size)
	p.simUsed[dest] += size
	p.simUsed[src] -= size
	p.holders.add(obj.ObjectKey, dest)
	p.leaving.add(obj.ObjectKey, src)
	p.remaining--
	return rebalanceMove{Source: obj, ToBackend: dest}, true
}

// usageBudget tracks the transfer a plan has already committed so a batch of
// moves cannot collectively breach a limit that each move individually fits
// inside. A move spends egress on the source and ingress on the destination,
// tracked separately since a backend can have headroom in one and not the
// other.
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
// backends' limits, counting what this plan has already committed. It uses
// WithinLimits rather than EligibleForWrite, which would exclude a draining
// source and stall the drain.
func (b *usageBudget) allows(src, dest string, size int64) bool {
	if b == nil || b.usage == nil {
		return true
	}
	if !b.usage.WithinLimits(src, s3op.GetObject, b.egress[src]+size, 0) {
		return false
	}
	return b.usage.WithinLimits(dest, s3op.PutObject, 0, b.ingress[dest]+size)
}

// commit records a planned move against both backends' allowances.
func (b *usageBudget) commit(src, dest string, size int64) {
	if b == nil {
		return
	}
	b.egress[src] += size
	b.ingress[dest] += size
}
