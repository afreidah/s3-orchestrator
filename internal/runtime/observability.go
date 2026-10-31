// -------------------------------------------------------------------------------
// Runtime - Observability Bootstrap
//
// Author: Alex Freidah
//
// Daemon-startup wiring for slog, the in-memory log buffer, OpenTelemetry
// tracing, and the audit-event Prometheus metric. The Bootstrap struct
// owns shutdown of the tracer so the runtime's ordered teardown can call
// it after every other component has flushed.
// -------------------------------------------------------------------------------

package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	goruntime "runtime"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/di"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// Observability bundles the state produced by daemon-startup logging and
// tracing initialization. The in-memory LogBuffer is exposed so the UI
// log pane can subscribe to it; ShutdownTracer is called during ordered
// teardown.
type Observability struct {
	LogBuffer      *telemetry.LogBuffer
	ShutdownTracer func(ctx context.Context) error
}

// startObservability configures the default slog handler chain (error
// attribute rendering, trace context injection, in-memory ring buffer),
// initializes the OpenTelemetry tracer, and sets the build_info metric.
// The provided logLevel is wired into the slog handler so reload-time
// log level changes take effect on subsequent log calls.
func startObservability(cfg *config.Config, stdout io.Writer, logLevel *slog.LevelVar) (*Observability, error) {
	logLevel.Set(config.ParseLogLevel(cfg.Server.LogLevel))

	logBuffer := telemetry.NewLogBuffer()
	slog.SetDefault(slog.New(telemetry.NewLogHandler(stdout, logLevel, logBuffer)))

	shutdownTracer, err := telemetry.InitTracer(context.Background(), cfg.Telemetry.Tracing)
	if err != nil {
		return nil, fmt.Errorf("init tracer: %w", err)
	}

	telemetry.BuildInfo.WithLabelValues(telemetry.Version, goruntime.Version()).Set(1)
	di.WireAuditMetrics()

	return &Observability{
		LogBuffer:      logBuffer,
		ShutdownTracer: shutdownTracer,
	}, nil
}
