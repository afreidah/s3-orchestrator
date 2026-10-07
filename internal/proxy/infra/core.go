// -------------------------------------------------------------------------------
// Backend Runtime - Shared Storage Infrastructure
//
// Author: Alex Freidah
//
// *BackendRuntime is the infrastructure every proxy subpackage and worker
// shares: the backend map and its drain and health filters, usage limits,
// admission, per-call backend timeouts, write-error classification, and the
// metrics collector. Consumers reach it through their own consumer-declared
// interfaces. Its methods are split across files by concern: registry.go,
// usage.go, admission.go, timeout.go and classify.go.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"log/slog"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/counter"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/proxy/accounting"
	"github.com/afreidah/s3-orchestrator/internal/proxy/metrics"
)

// DrainChecker reports whether a named backend is currently being drained.
// *BackendRuntime consumes this so drain ownership can live in the drain subpackage
// while *BackendRuntime filters write eligibility.
type DrainChecker interface {
	IsDraining(name string) bool
}

// Config bundles every input *BackendRuntime needs at construction. Exposed so
// callers (root proxy package, tests) can build a *BackendRuntime directly.
type Config struct {
	Backends         map[string]backend.ObjectBackend
	Order            []string
	BackendTimeout   time.Duration
	Usage            *counter.UsageTracker
	Quota            *counter.QuotaTracker
	RoutingStrategy  config.RoutingStrategy
	MaxObjectSizes   map[string]int64
	MetricsCollector *metrics.Collector
	AdmissionSem     chan struct{}
	Log              *slog.Logger
}

// BackendRuntime holds the shared backend infrastructure. It deliberately holds
// no store: each collaborator takes the store roles it needs directly, which is
// what lets every worker reuse the runtime without dragging persistence along.
// For which methods belong here versus on a collaborator, see
// docs/style-guide.md "Where new methods live".
//
// drainMgr is wired after construction by SetDrainChecker, since the drain
// manager is built after the runtime. A nil admissionSem means unbounded
// admission, and a zero backendTimeout disables the per-call timeout.
type BackendRuntime struct {
	backends         map[string]backend.ObjectBackend
	order            []string
	drainMgr         DrainChecker
	usage            *counter.UsageTracker
	maxObjectSizes   map[string]int64
	quota            *counter.QuotaTracker
	backendTimeout   time.Duration
	admissionSem     chan struct{}
	routingStrategy  config.RoutingStrategy
	metricsCollector *metrics.Collector
	log              *slog.Logger
	recorder         *accounting.Recorder
}

// New constructs a *BackendRuntime from cfg. The accounting Recorder is built
// here so every consumer shares one instance that observes the same usage
// tracker and the later-wired metrics collector through RecordOperation.
func New(cfg *Config) *BackendRuntime {
	c := &BackendRuntime{
		backends:         cfg.Backends,
		order:            cfg.Order,
		usage:            cfg.Usage,
		maxObjectSizes:   cfg.MaxObjectSizes,
		quota:            cfg.Quota,
		backendTimeout:   cfg.BackendTimeout,
		admissionSem:     cfg.AdmissionSem,
		routingStrategy:  cfg.RoutingStrategy,
		metricsCollector: cfg.MetricsCollector,
		log:              cfg.Log,
	}
	c.recorder = accounting.New(cfg.Usage, c.RecordOperation)
	return c
}

// -------------------------------------------------------------------------
// ACCOUNTING + LOGGING + METRICS
// -------------------------------------------------------------------------

// Acct returns the shared accounting.Recorder. Consumers should call
// Acct().APICall / Egress / Ingress / Operation instead of reaching
// through Usage() and RecordOperation directly so the per-backend
// accounting rules stay centralised.
func (c *BackendRuntime) Acct() *accounting.Recorder {
	return c.recorder
}

// SetMetricsCollector installs the metrics collector after BackendRuntime
// construction. The collector depends on the usage tracker which is
// owned by *BackendRuntime, so the collector is built after *BackendRuntime and wired
// back in.
func (c *BackendRuntime) SetMetricsCollector(m *metrics.Collector) {
	c.metricsCollector = m
}

// MetricsCollector returns the wired metrics collector (nil if unset).
func (c *BackendRuntime) MetricsCollector() *metrics.Collector {
	return c.metricsCollector
}

// Log returns the component-scoped logger; falls back to slog.Default()
// when *BackendRuntime was constructed without one.
func (c *BackendRuntime) Log() *slog.Logger {
	if c.log == nil {
		return slog.Default()
	}
	return c.log
}

// RoutingStrategy returns the configured routing strategy.
func (c *BackendRuntime) RoutingStrategy() config.RoutingStrategy {
	return c.routingStrategy
}

// Quota returns the byte-reservation tracker every write path consults before
// it writes and credits after it commits. A deployment always has one: it is
// what answers whether a backend has room, which no other component knows.
func (c *BackendRuntime) Quota() *counter.QuotaTracker {
	return c.quota
}

// RecordOperation delegates to the metrics collector.
func (c *BackendRuntime) RecordOperation(operation, backend string, start time.Time, err error) {
	c.metricsCollector.RecordOperation(operation, backend, start, err)
}

// UpdateQuotaMetrics refreshes Prometheus gauges from the metadata store.
func (c *BackendRuntime) UpdateQuotaMetrics(ctx context.Context) error {
	return c.metricsCollector.UpdateQuotaMetrics(ctx)
}

// UpdateFleetMetrics delegates to the metrics collector.
func (c *BackendRuntime) UpdateFleetMetrics(ctx context.Context) error {
	return c.metricsCollector.UpdateFleetMetrics(ctx)
}

// LoadFleetMetrics delegates to the metrics collector.
func (c *BackendRuntime) LoadFleetMetrics(ctx context.Context) error {
	return c.metricsCollector.LoadFleetMetrics(ctx)
}

// LoadWorkerGauges delegates to the metrics collector.
func (c *BackendRuntime) LoadWorkerGauges(ctx context.Context) error {
	return c.metricsCollector.LoadWorkerGauges(ctx)
}

// PublishWorkerGauges delegates to the metrics collector, or applies the
// gauges locally when none is installed.
func (c *BackendRuntime) PublishWorkerGauges(ctx context.Context, source string, g telemetry.WorkerGauges) {
	if c.metricsCollector == nil {
		g.Apply()
		return
	}
	c.metricsCollector.PublishWorkerGauges(ctx, source, g)
}

// RefreshUsageBaselines delegates to the metrics collector.
func (c *BackendRuntime) RefreshUsageBaselines(ctx context.Context) error {
	return c.metricsCollector.RefreshUsageBaselines(ctx)
}
