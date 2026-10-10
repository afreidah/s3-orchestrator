// -------------------------------------------------------------------------------
// Worker Gauges - Published by the Lock Holder, Served Everywhere
//
// Author: Alex Freidah
//
// A lock-gated worker computes its fleet-wide gauges on whichever instance holds
// its lock that tick. The collector applies them there and publishes them to
// shared state under the worker's own key; every instance loads every worker's
// key on each flush tick, so all of them serve the figures the latest holder
// computed. Without shared state there is a single instance, and publishing
// only applies them locally.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// workerGaugesTTL bounds how long a worker's published gauges outlive the
// last instance that refreshed them.
const workerGaugesTTL = 10 * time.Minute

// workerGaugesKey names one worker's published gauges in shared state.
func workerGaugesKey(source string) string {
	return "worker_gauges:" + source
}

// PublishWorkerGauges applies a worker's gauges on this instance and shares them
// with the others. A failed publish is logged; the values still stand here.
func (mc *Collector) PublishWorkerGauges(ctx context.Context, source string, g telemetry.WorkerGauges) {
	g.Apply()
	if mc.shared == nil {
		return
	}
	data, err := json.Marshal(g)
	if err != nil {
		mc.log.ErrorContext(ctx, "failed to encode worker gauges", "source", source, logfmt.Err(err))
		return
	}
	if err := mc.shared.PutShared(ctx, workerGaugesKey(source), data, workerGaugesTTL); err != nil {
		mc.log.WarnContext(ctx, "failed to publish worker gauges", "source", source, logfmt.Err(err))
	}
}

// LoadWorkerGauges applies every worker's last published gauges. A worker that
// has published nothing yet, or whose key expired, leaves its gauges as they
// are. A no-op without shared state.
func (mc *Collector) LoadWorkerGauges(ctx context.Context) error {
	if mc.shared == nil {
		return nil
	}
	var errs []error
	for _, source := range telemetry.GaugeSources {
		data, err := mc.shared.GetShared(ctx, workerGaugesKey(source))
		if err != nil {
			errs = append(errs, fmt.Errorf("load %s gauges: %w", source, err))
			continue
		}
		if data == nil {
			continue
		}
		var g telemetry.WorkerGauges
		if err := json.Unmarshal(data, &g); err != nil {
			errs = append(errs, fmt.Errorf("decode %s gauges: %w", source, err))
			continue
		}
		g.Apply()
	}
	return errors.Join(errs...)
}
