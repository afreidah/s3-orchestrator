// -------------------------------------------------------------------------------
// Roster - Every Instance's Own Report, Listed From Any of Them
//
// Author: Alex Freidah
//
// State that really does differ between instances - the log level each runs
// at, the outcome of each one's last config reload, the workers each has
// ticked - published by every instance under its own name on a heartbeat. A
// report expires a few heartbeats after its instance stops refreshing it, so
// the list is the set of instances running now.
// -------------------------------------------------------------------------------

package sharedstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// rosterTTLBeats is how many heartbeats a report outlives the last one that
// refreshed it, so one missed write does not drop a live instance.
const rosterTTLBeats = 3

// RosterConfig describes a roster: the name its reports are filed under, how
// often this instance republishes its own, and how to build it.
type RosterConfig[T any] struct {
	Name     string
	Interval time.Duration
	Report   func(ctx context.Context) T
}

// Roster is the set of reports of type T, one per running instance.
type Roster[T any] struct {
	shared *Shared
	cfg    RosterConfig[T]
}

// NewRoster builds the roster cfg describes over s.
func NewRoster[T any](s *Shared, cfg RosterConfig[T]) *Roster[T] {
	return &Roster[T]{shared: s, cfg: cfg}
}

// Run publishes this instance's report now and every interval after, until ctx
// is done. A failed publish is retried on the next beat; the Redis backend
// reports its own outage. On a single instance it only waits for ctx, so a
// supervisor does not restart it.
func (r *Roster[T]) Run(ctx context.Context) error {
	store := r.shared.active()
	if store == nil {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		if data, err := json.Marshal(r.cfg.Report(ctx)); err == nil {
			_ = store.PutShared(ctx, r.key(), data, rosterTTLBeats*r.cfg.Interval)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// List returns every running instance's report: this instance's built now,
// first, then the others' as last published, ordered by instance. A report
// that cannot be read is left out and its error returned with the rest, and
// so is a store that cannot be read at all, so a caller always has this
// instance's own.
func (r *Roster[T]) List(ctx context.Context) ([]T, error) {
	reports := []T{r.cfg.Report(ctx)}
	store := r.shared.active()
	if store == nil {
		return reports, nil
	}
	stored, err := store.ListShared(ctx, r.cfg.Name+":")
	if err != nil {
		return reports, err
	}
	delete(stored, r.key())
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(stored)) {
		var rep T
		if err := json.Unmarshal(stored[name], &rep); err != nil {
			errs = append(errs, fmt.Errorf("decode %s: %w", name, err))
			continue
		}
		reports = append(reports, rep)
	}
	return reports, errors.Join(errs...)
}

// key is the name this instance's report is filed under.
func (r *Roster[T]) key() string {
	return r.cfg.Name + ":" + r.shared.Self().String()
}
