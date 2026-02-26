// -------------------------------------------------------------------------------
// Rebalancer Tests - Move Execution Concurrency
//
// Author: Alex Freidah
//
// Unit tests for the parallel move execution in the rebalancer. Verifies that
// concurrent moves complete correctly, partial failures are handled, and
// sequential fallback (concurrency=1) works identically to the old behavior.
// -------------------------------------------------------------------------------

package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// delayedGetBackend wraps mockBackend and adds a delay to GetObject
// to simulate real backend latency for concurrency testing.
type delayedGetBackend struct {
	mu      sync.Mutex
	objects map[string]mockObject
	putErr  error
	getErr  error
	headErr error
	delErr  error
	delay   time.Duration
}

func newDelayedGetBackend(delay time.Duration) *delayedGetBackend {
	return &delayedGetBackend{
		objects: make(map[string]mockObject),
		delay:   delay,
	}
}

var _ ObjectBackend = (*delayedGetBackend)(nil)

func (m *delayedGetBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, contentType string) (string, error) {
	m.mu.Lock()
	err := m.putErr
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	etag := fmt.Sprintf(`"%x"`, len(data))
	m.mu.Lock()
	m.objects[key] = mockObject{data: data, contentType: contentType, etag: etag}
	m.mu.Unlock()
	return etag, nil
}

func (m *delayedGetBackend) GetObject(_ context.Context, key string, _ string) (*GetObjectResult, error) {
	time.Sleep(m.delay)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	cp := make([]byte, len(obj.data))
	copy(cp, obj.data)
	return &GetObjectResult{
		Body:        io.NopCloser(bytes.NewReader(cp)),
		Size:        int64(len(cp)),
		ContentType: obj.contentType,
		ETag:        obj.etag,
	}, nil
}

func (m *delayedGetBackend) HeadObject(_ context.Context, key string) (int64, string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.headErr != nil {
		return 0, "", "", m.headErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return 0, "", "", fmt.Errorf("object %q not found", key)
	}
	return int64(len(obj.data)), obj.contentType, obj.etag, nil
}

func (m *delayedGetBackend) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.objects, key)
	return nil
}

func (m *delayedGetBackend) seedObject(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = mockObject{data: data, contentType: "application/octet-stream"}
}

func (m *delayedGetBackend) hasObject(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

func TestExecuteMoves_Concurrent(t *testing.T) {
	src := newDelayedGetBackend(50 * time.Millisecond)
	dest := newDelayedGetBackend(0)

	for i := range 5 {
		src.seedObject(fmt.Sprintf("key%d", i), []byte("data"))
	}

	store := &mockStore{moveObjectLocationSize: 4}
	obs := map[string]ObjectBackend{"src": src, "dest": dest}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	var plan []rebalanceMove
	for i := range 5 {
		plan = append(plan, rebalanceMove{
			ObjectKey:   fmt.Sprintf("key%d", i),
			FromBackend: "src",
			ToBackend:   "dest",
			SizeBytes:   4,
		})
	}

	start := time.Now()
	moved := mgr.executeMoves(context.Background(), plan, "spread", 3)
	elapsed := time.Since(start)

	if moved != 5 {
		t.Errorf("moved = %d, want 5", moved)
	}

	// 5 moves at 50ms each with concurrency 3 should take ~100ms (2 batches),
	// not 250ms (sequential). Allow generous margin for CI.
	if elapsed > 200*time.Millisecond {
		t.Errorf("elapsed = %v, expected < 200ms with concurrency 3", elapsed)
	}

	// Verify objects landed on dest
	for i := range 5 {
		if !dest.hasObject(fmt.Sprintf("key%d", i)) {
			t.Errorf("key%d not found on destination", i)
		}
	}
}

func TestExecuteMoves_PartialFailure(t *testing.T) {
	src := newDelayedGetBackend(0)
	dest := newDelayedGetBackend(0)

	src.seedObject("ok1", []byte("data"))
	src.seedObject("ok2", []byte("data"))

	store := &mockStore{moveObjectLocationSize: 4}
	obs := map[string]ObjectBackend{"src": src, "dest": dest}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	// "fail" key does not exist on source, so GetObject returns not-found
	plan := []rebalanceMove{
		{ObjectKey: "ok1", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "fail", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
		{ObjectKey: "ok2", FromBackend: "src", ToBackend: "dest", SizeBytes: 4},
	}

	moved := mgr.executeMoves(context.Background(), plan, "spread", 3)
	if moved != 2 {
		t.Errorf("moved = %d, want 2 (one should fail)", moved)
	}
}

func TestExecuteMoves_SequentialFallback(t *testing.T) {
	src := newDelayedGetBackend(0)
	dest := newDelayedGetBackend(0)

	src.seedObject("a", []byte("hello"))
	src.seedObject("b", []byte("world"))

	store := &mockStore{moveObjectLocationSize: 5}
	obs := map[string]ObjectBackend{"src": src, "dest": dest}
	mgr := NewBackendManager(&BackendManagerConfig{
		Backends:        obs,
		Store:           store,
		Order:           []string{"src", "dest"},
		CacheTTL:        5 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
	})

	plan := []rebalanceMove{
		{ObjectKey: "a", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
		{ObjectKey: "b", FromBackend: "src", ToBackend: "dest", SizeBytes: 5},
	}

	moved := mgr.executeMoves(context.Background(), plan, "pack", 1)
	if moved != 2 {
		t.Errorf("moved = %d, want 2", moved)
	}

	if !dest.hasObject("a") || !dest.hasObject("b") {
		t.Error("expected both objects on destination")
	}
}
