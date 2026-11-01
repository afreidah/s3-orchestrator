// -------------------------------------------------------------------------------
// Event Recorder - Capturing Published Events in Tests
//
// Author: Alex Freidah
//
// Installs an emitter that keeps every event published until the test ends,
// safe for code that publishes from several goroutines, and removes it in the
// test's cleanup so it cannot leak into a later test.
// -------------------------------------------------------------------------------

package eventtest

import (
	"slices"
	"sync"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/observe/event"
)

// Record captures every event published until the test ends and returns a
// function reporting what has been published so far.
func Record(t testing.TB) func() []event.Event {
	t.Helper()
	var mu sync.Mutex
	var published []event.Event
	event.SetEmitter(func(ev event.Event) {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, ev)
	})
	t.Cleanup(func() { event.SetEmitter(nil) })
	return func() []event.Event {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(published)
	}
}

// OfType returns the recorded events of one type, in publish order.
func OfType(events []event.Event, eventType string) []event.Event {
	return slices.DeleteFunc(slices.Clone(events), func(ev event.Event) bool { return ev.Type != eventType })
}
