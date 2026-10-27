// -------------------------------------------------------------------------------
// Shared State - Tests
//
// Author: Alex Freidah
//
// Drives Value, Topic and Roster across instances sharing one in-memory store,
// and on a single instance with no store at all.
// -------------------------------------------------------------------------------

package sharedstate_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/instanceid"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate"
	"github.com/afreidah/s3-orchestrator/internal/sharedstate/sharedstatetest"
)

// report is the payload the tests share.
type report struct {
	ID string `json:"id"`
	N  int    `json:"n"`
}

// errDown stands in for an unreachable Redis.
var errDown = errors.New("redis down")

// -------------------------------------------------------------------------
// VALUE
// -------------------------------------------------------------------------

// TestValue_SharedBetweenInstances writes on one instance and reads on
// another, and reads nothing for a name never written.
func TestValue_SharedBetweenInstances(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	a := sharedstate.NewValue[report](sharedstate.New(mem, "a"), "snap", time.Minute)
	b := sharedstate.NewValue[report](sharedstate.New(mem, "b"), "snap", time.Minute)

	if err := a.Put(t.Context(), &report{ID: "a", N: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := b.Get(t.Context()); err != nil || got == nil || *got != (report{ID: "a", N: 1}) {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if ttl := mem.TTL("snap"); ttl != time.Minute {
		t.Errorf("TTL = %v, want 1m", ttl)
	}
	other := sharedstate.NewValue[report](sharedstate.New(mem, "b"), "other", time.Minute)
	if got, err := other.Get(t.Context()); got != nil || err != nil {
		t.Errorf("Get(unwritten) = %+v, %v; want nil, nil", got, err)
	}
}

// TestValue_SingleInstanceAndFailures stores and reads nothing without a
// store, and surfaces a store error and an undecodable value.
func TestValue_SingleInstanceAndFailures(t *testing.T) {
	t.Parallel()
	alone := sharedstate.NewValue[report](nil, "snap", time.Minute)
	if err := alone.Put(t.Context(), &report{N: 1}); err != nil {
		t.Errorf("Put alone: %v", err)
	}
	if got, err := alone.Get(t.Context()); got != nil || err != nil {
		t.Errorf("Get alone = %+v, %v; want nil, nil", got, err)
	}

	mem := sharedstatetest.NewMemory()
	if err := mem.PutShared(t.Context(), "snap", []byte("not json"), 0); err != nil {
		t.Fatal(err)
	}
	v := sharedstate.NewValue[report](sharedstate.New(mem, "a"), "snap", time.Minute)
	if _, err := v.Get(t.Context()); err == nil {
		t.Error("Get of an undecodable value succeeded")
	}
	mem.Fail(errDown)
	if err := v.Put(t.Context(), &report{}); !errors.Is(err, errDown) {
		t.Errorf("Put err = %v, want %v", err, errDown)
	}
}

// -------------------------------------------------------------------------
// TOPIC
// -------------------------------------------------------------------------

// watch runs a watcher on topic until the test ends and returns what it
// recorded once the subscription is established.
func watch(t *testing.T, topic sharedstate.Topic[report]) func() (applied []report, resyncs int) {
	t.Helper()
	var mu sync.Mutex
	var applied []report
	resyncs := 0
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		topic.Watch(ctx, func(_ context.Context, r report) {
			mu.Lock()
			defer mu.Unlock()
			applied = append(applied, r)
		}, func(context.Context) {
			mu.Lock()
			defer mu.Unlock()
			if resyncs++; resyncs == 1 {
				close(ready)
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-ready
	return func() ([]report, int) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(applied), resyncs
	}
}

// TestTopic_AppliesMessagesAndResyncs resyncs as the subscription starts,
// applies a published message, and resyncs for one an older version sent.
func TestTopic_AppliesMessagesAndResyncs(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	seen := watch(t, sharedstate.NewTopic[report](sharedstate.New(mem, "b"), "cache"))

	if err := sharedstate.NewTopic[report](sharedstate.New(mem, "a"), "cache").Publish(t.Context(), report{ID: "x"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := mem.PublishShared(t.Context(), "cache", []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if applied, resyncs := seen(); !slices.Equal(applied, []report{{ID: "x"}}) || resyncs != 2 {
		t.Errorf("applied %+v with %d resyncs, want the message and 2", applied, resyncs)
	}
}

// TestTopic_SingleInstanceAndFailures publishes to no one and waits out the
// context without a store, and surfaces a store error.
func TestTopic_SingleInstanceAndFailures(t *testing.T) {
	t.Parallel()
	alone := sharedstate.NewTopic[report](nil, "cache")
	if err := alone.Publish(t.Context(), report{}); err != nil {
		t.Errorf("Publish alone: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	alone.Watch(ctx, func(context.Context, report) { t.Error("apply called alone") },
		func(context.Context) { t.Error("resync called alone") })

	mem := sharedstatetest.NewMemory()
	mem.Fail(errDown)
	if err := sharedstate.NewTopic[report](sharedstate.New(mem, "a"), "cache").Publish(t.Context(), report{}); !errors.Is(err, errDown) {
		t.Errorf("Publish err = %v, want %v", err, errDown)
	}
}

// -------------------------------------------------------------------------
// ROSTER
// -------------------------------------------------------------------------

// newRoster builds instance id's roster over store, reporting n.
func newRoster(store sharedstate.Store, id string, n int) *sharedstate.Roster[report] {
	return sharedstate.NewRoster(sharedstate.New(store, instanceid.ID(id)), sharedstate.RosterConfig[report]{
		Name:     "instance",
		Interval: time.Millisecond,
		Report:   func(context.Context) report { return report{ID: id, N: n} },
	})
}

// TestRoster_ListsEveryInstance runs instance b's heartbeat and lists from a,
// which reports its own first and b's after, with three heartbeats of TTL.
func TestRoster_ListsEveryInstance(t *testing.T) {
	t.Parallel()
	mem := sharedstatetest.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = newRoster(mem, "b", 2).Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	a := newRoster(mem, "a", 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := a.List(t.Context())
		if err == nil && slices.Equal(got, []report{{ID: "a", N: 1}, {ID: "b", N: 2}}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("List = %+v, %v; want a then b", got, err)
		}
		time.Sleep(time.Millisecond)
	}
	if ttl := mem.TTL("instance:b"); ttl != 3*time.Millisecond {
		t.Errorf("report TTL = %v, want three heartbeats", ttl)
	}
}

// TestRoster_SingleInstanceAndFailures lists this instance alone without a
// store, and with the error when the store fails or a report does not decode.
func TestRoster_SingleInstanceAndFailures(t *testing.T) {
	t.Parallel()
	alone := newRoster(nil, "a", 1)
	if got, err := alone.List(t.Context()); err != nil || !slices.Equal(got, []report{{ID: "a", N: 1}}) {
		t.Errorf("List alone = %+v, %v", got, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := alone.Run(ctx); err != nil {
		t.Errorf("Run alone: %v", err)
	}

	mem := sharedstatetest.NewMemory()
	if err := mem.PutShared(t.Context(), "instance:z", []byte("not json"), 0); err != nil {
		t.Fatal(err)
	}
	r := newRoster(mem, "a", 1)
	if got, err := r.List(t.Context()); err == nil || len(got) != 1 {
		t.Errorf("List with an undecodable report = %+v, %v; want a and an error", got, err)
	}
	mem.Fail(errDown)
	if got, err := r.List(t.Context()); !errors.Is(err, errDown) || len(got) != 1 {
		t.Errorf("List on a failing store = %+v, %v; want a and %v", got, err, errDown)
	}
}
