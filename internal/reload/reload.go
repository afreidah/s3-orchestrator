// -------------------------------------------------------------------------------
// Reload Coordinator - SIGHUP-Driven Config Refresh
//
// Author: Alex Freidah
//
// Watches for SIGHUP, loads the replacement configuration, runs a Check
// pass across every Hook (any error aborts before mutation), then runs
// the Apply pass collecting per-hook outcomes. A monotonic generation
// counter advances on every successful Apply pass (full or partial) so
// operators can correlate reload events with the version each component
// is running. The most recent Result is held atomically and
// exposed via LastResult() for admin status surfaces.
// -------------------------------------------------------------------------------

package reload

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/samber/do/v2"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// applyTimeout caps the Apply phase so a hook that hangs on an outbound
// call (quota sync, quota metrics refresh) cannot stall the SIGHUP
// goroutine. The Check phase is in-memory and not timeboxed.
const applyTimeout = 10 * time.Second

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Deps groups everything the coordinator mutates or reads on reload.
// CfgPtr is the atomic config the rest of the process reads; the coordinator
// swaps the new one in after a successful Apply pass. Hooks is nil in
// production, where the default set is used, and set by tests injecting fakes.
type Deps struct {
	ConfigPath   string      // the on-disk YAML SIGHUP reloads
	Injector     do.Injector // consulted by hooks for optional subsystems
	CfgPtr       *syncutil.AtomicConfig[config.Config]
	LogLevel     *slog.LevelVar         // updated by the log_level hook
	CertReloader *httputil.CertReloader // rotates the TLS certificate; nil when TLS is off
	Hooks        []Hook
}

// Coordinator owns the SIGHUP goroutine, the hook sequence, and the
// last-result snapshot. Construct it with New, then call Watch to start
// the signal listener. Shutdown stops the goroutine. LastResult is
// concurrent-safe.
type Coordinator struct {
	deps  Deps
	hooks []Hook
	log   *slog.Logger

	generation atomic.Int64
	lastResult atomic.Pointer[Result]

	hupChan chan os.Signal
	hupDone chan struct{}

	mu      sync.Mutex
	stopped bool
}

// New returns a Coordinator with the given deps. Watch must be called
// to start observing SIGHUP. Deps is passed by pointer because it
// embeds a slog.LevelVar pointer and the runtime needs to mutate the
// underlying state through it.
func New(deps *Deps) *Coordinator {
	hooks := deps.Hooks
	if hooks == nil {
		hooks = defaultHooks(deps.Injector, deps.CertReloader, deps.LogLevel)
	}
	return &Coordinator{
		deps:  *deps,
		hooks: hooks,
		log:   slog.Default().With(logfmt.Component("reload")),
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Watch installs a SIGHUP handler and spawns the reload goroutine. The
// goroutine runs until Shutdown is called.
func (c *Coordinator) Watch() {
	c.hupChan = make(chan os.Signal, 1)
	c.hupDone = make(chan struct{})
	signal.Notify(c.hupChan, syscall.SIGHUP)

	go func() {
		defer close(c.hupDone)
		for range c.hupChan {
			c.Reload()
		}
	}()
}

// Shutdown stops the SIGHUP goroutine and waits for it to exit. Safe
// to call multiple times.
func (c *Coordinator) Shutdown() {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.mu.Unlock()

	signal.Stop(c.hupChan)
	close(c.hupChan)
	<-c.hupDone
}

// LastResult returns the most recent reload result, or nil if no
// reload has been attempted yet.
func (c *Coordinator) LastResult() *Result {
	return c.lastResult.Load()
}

// Generation returns the monotonic generation counter. Starts at 0;
// advances on every successful Apply pass (full or partial).
//
// The admin API reports the same number through LastResult, which carries the
// rest of the pass with it. This accessor answers the narrower question without
// a result to unpack, and is nil-safe before the first reload, where
// LastResult is not.
func (c *Coordinator) Generation() int64 {
	return c.generation.Load()
}

// Reload performs one full reload pass. Exposed so tests and admin
// surfaces can trigger a reload without sending a real signal.
// Returns the result of the pass; the same value is stored on the
// coordinator and reachable via LastResult.
func (c *Coordinator) Reload() *Result {
	started := time.Now()
	ctx := context.Background()
	currentGen := c.generation.Load()

	c.log.InfoContext(ctx, "SIGHUP received, reloading configuration",
		"path", c.deps.ConfigPath, "current_generation", currentGen)

	newCfg, err := config.LoadConfig(c.deps.ConfigPath)
	if err != nil {
		return c.finalize(ctx, &Result{
			Generation: currentGen,
			Status:     ReloadLoadFailed,
			LoadError:  err.Error(),
			StartedAt:  started,
		})
	}

	result := &Result{
		Generation:      currentGen,
		StartedAt:       started,
		RequiresRestart: config.NonReloadableFieldsChanged(c.deps.CfgPtr.Load(), newCfg),
	}

	currentCfg := c.deps.CfgPtr.Load()

	for _, h := range c.hooks {
		if err := h.Check(currentCfg, newCfg); err != nil {
			result.Status = ReloadValidationFailed
			result.Outcomes = append(result.Outcomes, HookOutcome{
				Name:   h.Name(),
				Status: HookFailed,
				Error:  err.Error(),
			})
			return c.finalize(ctx, result)
		}
	}

	applyCtx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()

	failed := 0
	for _, h := range c.hooks {
		status, err := h.Apply(applyCtx, currentCfg, newCfg)
		outcome := HookOutcome{Name: h.Name(), Status: status}
		if err != nil {
			outcome.Status = HookFailed
			outcome.Error = err.Error()
			failed++
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}

	// Mutate the live atomic config and advance the generation only
	// once Apply has run end-to-end. Generation advances even on
	// PartialApplied so operators can tell that state moved.
	c.deps.CfgPtr.Store(newCfg)
	newGen := c.generation.Add(1)
	result.Generation = newGen

	if failed > 0 {
		result.Status = ReloadPartialApplied
	} else {
		result.Status = ReloadFullSuccess
	}
	return c.finalize(ctx, result)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// finalize stamps EndedAt, logs the outcome at the appropriate level,
// stores the result, and returns it.
func (c *Coordinator) finalize(ctx context.Context, r *Result) *Result {
	r.EndedAt = time.Now()
	c.lastResult.Store(r)

	for _, field := range r.RequiresRestart {
		c.log.WarnContext(ctx, "config field changed but requires restart to take effect",
			"field", field, "generation", r.Generation)
	}

	switch r.Status {
	case ReloadFullSuccess:
		c.log.InfoContext(ctx, "configuration reload complete",
			"generation", r.Generation,
			"duration", r.EndedAt.Sub(r.StartedAt),
		)
	case ReloadPartialApplied:
		c.log.WarnContext(ctx, "configuration reload partially applied",
			"generation", r.Generation,
			"failed_hooks", failedHookNames(r),
			"duration", r.EndedAt.Sub(r.StartedAt),
		)
	case ReloadValidationFailed:
		c.log.ErrorContext(ctx, "configuration reload validation failed, keeping current config",
			"generation", r.Generation,
			"failed_hook", firstFailedHookName(r),
		)
		event.Publish(event.ConfigReloadFailed, "", map[string]any{
			"generation":   r.Generation,
			"status":       string(r.Status),
			"failed_hooks": failedHookNames(r),
		})
	case ReloadLoadFailed:
		c.log.ErrorContext(ctx, "config reload aborted, keeping current config",
			"generation", r.Generation,
			"error", r.LoadError,
		)
		event.Publish(event.ConfigReloadFailed, "", map[string]any{
			"generation": r.Generation,
			"status":     string(r.Status),
			"error":      r.LoadError,
		})
	}
	return r
}

// failedHookNames returns the names of hooks whose outcome is Failed.
func failedHookNames(r *Result) []string {
	var names []string
	for _, o := range r.Outcomes {
		if o.Status == HookFailed {
			names = append(names, o.Name)
		}
	}
	return names
}

// firstFailedHookName returns the name of the first failed hook in the
// outcomes slice. Used in the validation-failure log line where there
// is exactly one failure.
func firstFailedHookName(r *Result) string {
	for _, o := range r.Outcomes {
		if o.Status == HookFailed {
			return o.Name
		}
	}
	return ""
}
