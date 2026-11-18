// -------------------------------------------------------------------------------
// PressureTracker - Per-Backend Call Times and the Pressure They Show
//
// Author: Alex Freidah
//
// Times the calls each backend answers and judges, on a tick, whether it is
// under pressure: too many calls slower than its own baseline, or too many
// ended by the orchestrator's backend timeout. The shares are kept over 10s,
// 60s and 300s windows the way Linux PSI keeps its averages, and the 60s window
// decides. The request path reads only the published judgement.
//
// The baseline is the geometric mean of the backend's call times over about an
// hour. It takes no calls in while the backend is under pressure, so a long
// slowdown never becomes its normal. Each instance judges from its own calls.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle"
	"github.com/afreidah/s3-orchestrator/internal/lifecycle/tickrunner"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// pressureWindows are the spans the shares are kept over, labelled as the
// gauges name them. decidingWindow indexes the one pressure is judged on.
var pressureWindows = [...]struct {
	span  time.Duration
	label string
}{
	{10 * time.Second, "10s"},
	{time.Minute, "60s"},
	{5 * time.Minute, "300s"},
}

// The deciding window, the span the baseline is averaged over, the shortest
// call time the baseline's logarithm is taken of, how often pressure is
// judged, and the name the service logs and reports its ticks under.
const (
	decidingWindow  = 1
	baselineWindow  = time.Hour
	minBaselineCall = time.Millisecond
	pressureTick    = 2 * time.Second
	pressureService = "backend_pressure"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// PressureShares is the share of a backend's calls in one window that were
// slow, and the share ended by the backend timeout.
type PressureShares struct {
	Slow     float64
	Timeouts float64
}

// PressureState is a backend's pressure as last judged: the reasons it holds,
// its shares in each window, the calls the deciding window holds, and the
// baseline its calls are judged slow against, zero until it has one.
type PressureState struct {
	Reasons  Pressure
	Windows  [len(pressureWindows)]PressureShares
	Calls    float64
	Baseline time.Duration
}

// Deciding returns the shares of the window pressure is judged on.
func (s PressureState) Deciding() PressureShares {
	return s.Windows[decidingWindow]
}

// callWindow is one window's exponentially decayed call counts.
type callWindow struct {
	calls, slow, timeouts float64
}

// backendCalls is one backend's decayed counts, its baseline as a decayed sum
// of log call times, and the reasons it was last judged to hold.
type backendCalls struct {
	mu       sync.Mutex
	last     time.Time
	windows  [len(pressureWindows)]callWindow
	logSum   float64
	logCount float64
	logLast  time.Time
	reasons  Pressure
}

// PressureTracker times every backend's calls and publishes the pressure each
// is under. The backend set is fixed at construction.
type PressureTracker struct {
	backends map[string]*backendCalls
	cfg      syncutil.Section[config.BackendPressureConfig]
	state    atomic.Pointer[map[string]PressureState]
	log      *slog.Logger
}

// NewPressureTracker builds a tracker over the named backends, judged by the
// policy cfg reads. Every backend starts under no pressure.
func NewPressureTracker(names []string, cfg syncutil.Section[config.BackendPressureConfig]) *PressureTracker {
	t := &PressureTracker{
		backends: make(map[string]*backendCalls, len(names)),
		cfg:      cfg,
		log:      logfmt.Logger(pressureService),
	}
	for _, name := range names {
		t.backends[name] = &backendCalls{}
	}
	t.state.Store(&map[string]PressureState{})
	return t
}

// NewPressureService judges the tracker's backends every tick, on every
// instance with no lock, since each instance judges from its own calls.
func NewPressureService(t *PressureTracker) lifecycle.Runner {
	return tickrunner.New(tickrunner.Config{
		Name:     pressureService,
		Interval: pressureTick,
		Work: func(ctx context.Context) error {
			t.Evaluate(ctx, time.Now())
			return nil
		},
	})
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Observe records one call to backend that took elapsed, with timedOut set
// when the backend timeout ended it.
func (t *PressureTracker) Observe(backend string, elapsed time.Duration, timedOut bool) {
	t.observeAt(backend, time.Now(), elapsed, timedOut)
}

// Pressure returns the reasons backend is under pressure, empty when it is
// not.
func (t *PressureTracker) Pressure(backend string) Pressure {
	return (*t.state.Load())[backend].Reasons
}

// State returns backend's pressure as last judged.
func (t *PressureTracker) State(backend string) PressureState {
	return (*t.state.Load())[backend]
}

// Evaluate judges every backend as of now, publishes the result for the
// request path, sets the gauges, and reports each backend that came under
// pressure or left it.
func (t *PressureTracker) Evaluate(ctx context.Context, now time.Time) {
	cfg := t.cfg.Config()
	next := make(map[string]PressureState, len(t.backends))
	for name, b := range t.backends {
		policy := t.policyFor(name)
		was, st := b.judge(now, &policy, cfg.IsEnabled())
		next[name] = st
		t.report(ctx, name, was, &st)
	}
	t.state.Store(&next)
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// policyFor returns the policy backend is judged by. Every backend shares the
// configured one.
func (t *PressureTracker) policyFor(string) config.PressurePolicy {
	return t.cfg.Config().PressurePolicy
}

// observeAt records one call at now. A call is slow once the backend has a
// baseline it exceeds by the policy's factor. Calls the timeout ended, and
// every call while the backend is under pressure, stay out of the baseline.
func (t *PressureTracker) observeAt(backend string, now time.Time, elapsed time.Duration, timedOut bool) {
	b := t.backends[backend]
	if b == nil {
		return
	}
	policy := t.policyFor(backend)
	secs := max(elapsed, minBaselineCall).Seconds()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.decay(now)
	base, ok := b.baseline(policy.MinCalls)
	slow := ok && secs > policy.SlowFactor*base.Seconds()
	for i := range b.windows {
		w := &b.windows[i]
		w.calls++
		w.slow += boolCount(slow)
		w.timeouts += boolCount(timedOut)
	}
	if b.reasons == 0 && !timedOut {
		keep := decayFactor(now.Sub(b.logLast), baselineWindow)
		b.logSum = b.logSum*keep + math.Log(secs)
		b.logCount = b.logCount*keep + 1
		b.logLast = now
	}
}

// judge decays the counts to now and decides the reasons the backend holds
// under policy. A reason is taken at its share and held until the share falls
// below half of it, and a window with fewer than MinCalls calls holds none.
// Returns the reasons it held before and its new state.
func (b *backendCalls) judge(now time.Time, policy *config.PressurePolicy, enabled bool) (Pressure, PressureState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decay(now)

	var st PressureState
	for i, w := range b.windows {
		st.Windows[i] = PressureShares{Slow: share(w.slow, w.calls), Timeouts: share(w.timeouts, w.calls)}
	}
	st.Calls = b.windows[decidingWindow].calls
	st.Baseline, _ = b.baseline(1)

	was := b.reasons
	if enabled && st.Calls >= float64(policy.MinCalls) {
		d := st.Deciding()
		st.Reasons = held(was, PressureSlow, d.Slow, policy.SlowShare) |
			held(was, PressureTimeouts, d.Timeouts, policy.TimeoutShare)
	}
	b.reasons = st.Reasons
	return was, st
}

// decay ages every window's counts to now.
func (b *backendCalls) decay(now time.Time) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	for i := range b.windows {
		keep := decayFactor(elapsed, pressureWindows[i].span)
		w := &b.windows[i]
		w.calls *= keep
		w.slow *= keep
		w.timeouts *= keep
	}
	b.last = now
}

// baseline returns the backend's typical call time, or false while fewer
// than minCalls calls stand behind it.
func (b *backendCalls) baseline(minCalls int) (time.Duration, bool) {
	if b.logCount < float64(minCalls) || b.logCount == 0 {
		return 0, false
	}
	return time.Duration(math.Exp(b.logSum/b.logCount) * float64(time.Second)), true
}

// report sets backend's gauges and logs and publishes a change between under
// and out of pressure.
func (t *PressureTracker) report(ctx context.Context, backend string, was Pressure, st *PressureState) {
	for i, w := range st.Windows {
		label := pressureWindows[i].label
		telemetry.BackendPressure.WithLabelValues(backend, "slow", label).Set(w.Slow)
		telemetry.BackendPressure.WithLabelValues(backend, "timeouts", label).Set(w.Timeouts)
	}
	for _, reason := range pressureNames {
		telemetry.BackendPressured.WithLabelValues(backend, reason.Name).Set(boolCount(st.Reasons.Has(reason.Bit)))
	}
	telemetry.BackendLatencyBaseline.WithLabelValues(backend).Set(st.Baseline.Seconds())

	if (was == 0) == (st.Reasons == 0) {
		return
	}
	d := st.Deciding()
	if st.Reasons != 0 {
		t.log.WarnContext(ctx, "backend under pressure; trying it after the others",
			"backend", backend, "reasons", st.Reasons.String(),
			"slow_share", d.Slow, "timeout_share", d.Timeouts, "baseline", st.Baseline)
	} else {
		t.log.InfoContext(ctx, "backend no longer under pressure", "backend", backend)
	}
	telemetry.BackendPressureEvent(backend, st.Reasons.Names(), d.Slow, d.Timeouts)
}

// held returns bit when share reaches threshold, or stays above half of it
// while prev already holds bit.
func held(prev, bit Pressure, share, threshold float64) Pressure {
	if share >= threshold || (prev.Has(bit) && share >= threshold/2) {
		return bit
	}
	return 0
}

// decayFactor is how much of a count survives elapsed in a window of span.
func decayFactor(elapsed, span time.Duration) float64 {
	return math.Exp(-elapsed.Seconds() / span.Seconds())
}

// share is part over whole, zero when there is no whole.
func share(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole
}

// boolCount counts a true as one.
func boolCount(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
