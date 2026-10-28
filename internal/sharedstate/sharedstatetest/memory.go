// -------------------------------------------------------------------------------
// Memory - In-Process Shared State Store
//
// Author: Alex Freidah
//
// A sharedstate.Store held in a map, standing in for Redis in tests. Several
// sharedstate.Shared handles built over one Memory behave as instances of one
// deployment: a value one puts the others read, and a message one publishes
// reaches every watcher, delivered on the publisher's goroutine. Values do not
// expire; the TTL each was written with is recorded for assertions instead.
// -------------------------------------------------------------------------------

package sharedstatetest

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// Memory is an in-process sharedstate.Store. An error set with Fail fails
// every call but a watch, standing in for an unreachable Redis.
type Memory struct {
	mu       sync.Mutex
	values   map[string][]byte
	ttls     map[string]time.Duration
	watchers map[string]map[int]func(context.Context, []byte)
	nextID   int
	err      error
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{
		values:   make(map[string][]byte),
		ttls:     make(map[string]time.Duration),
		watchers: make(map[string]map[int]func(context.Context, []byte)),
	}
}

// -------------------------------------------------------------------------
// VALUES
// -------------------------------------------------------------------------

// PutShared stores value under name and records its TTL.
func (m *Memory) PutShared(_ context.Context, name string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.values[name] = value
	m.ttls[name] = ttl
	return nil
}

// GetShared returns the value under name, or nil when there is none.
func (m *Memory) GetShared(_ context.Context, name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.values[name], nil
}

// ListShared returns every value whose name starts with prefix.
func (m *Memory) ListShared(_ context.Context, prefix string) (map[string][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	out := maps.Clone(m.values)
	maps.DeleteFunc(out, func(name string, _ []byte) bool { return !strings.HasPrefix(name, prefix) })
	return out, nil
}

// Fail makes every later call return err, or succeed again when err is nil.
func (m *Memory) Fail(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// TTL returns the expiry the value under name was last written with.
func (m *Memory) TTL(name string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ttls[name]
}

// -------------------------------------------------------------------------
// MESSAGES
// -------------------------------------------------------------------------

// PublishShared delivers payload to every watcher of channel before it returns.
func (m *Memory) PublishShared(ctx context.Context, channel string, payload []byte) error {
	m.mu.Lock()
	if m.err != nil {
		m.mu.Unlock()
		return m.err
	}
	watchers := make([]func(context.Context, []byte), 0, len(m.watchers[channel]))
	for _, fn := range m.watchers[channel] {
		watchers = append(watchers, fn)
	}
	m.mu.Unlock()
	for _, fn := range watchers {
		fn(ctx, payload)
	}
	return nil
}

// WatchShared subscribes onMessage to channel, calls it once with a nil payload
// as a fresh subscription does, and blocks until ctx is done.
func (m *Memory) WatchShared(ctx context.Context, channel string, onMessage func(ctx context.Context, payload []byte)) {
	m.mu.Lock()
	id := m.nextID
	m.nextID++
	if m.watchers[channel] == nil {
		m.watchers[channel] = make(map[int]func(context.Context, []byte))
	}
	m.watchers[channel][id] = onMessage
	m.mu.Unlock()

	onMessage(ctx, nil)
	<-ctx.Done()

	m.mu.Lock()
	delete(m.watchers[channel], id)
	m.mu.Unlock()
}
