// -------------------------------------------------------------------------------
// Mock Backend - Test Double for s3be.ObjectBackend
//
// Author: Alex Freidah
//
// Configurable in-memory s3be.ObjectBackend implementation for unit testing. Supports
// pre-set responses, injectable errors, and call tracking for assertion.
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
)

// mockBackend is an in-memory s3be.ObjectBackend for unit testing.
type mockBackend struct {
	mu         sync.Mutex
	objects    map[string]mockObject
	putErr     error
	getErr     error
	getReadErr error  // injected into the Body reader so reads fail mid-stream
	getPanic   bool   // if true, GetObject panics instead of returning
	headErr    error
	delErr     error
	delDelay   time.Duration
}

type mockObject struct {
	data        []byte
	contentType string
	etag        string
	metadata    map[string]string
}

func newMockBackend() *mockBackend {
	return &mockBackend{objects: make(map[string]mockObject)}
}

var _ s3be.ObjectBackend = (*mockBackend)(nil)

func (m *mockBackend) PutObject(_ context.Context, key string, body io.Reader, _ int64, contentType string, metadata map[string]string) (string, error) {
	m.mu.Lock()
	err := m.putErr
	m.mu.Unlock()
	if err != nil {
		return "", err
	}

	// Read body outside the lock to avoid deadlocking with pipe-based copies
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}

	etag := fmt.Sprintf(`"%x"`, len(data))

	m.mu.Lock()
	m.objects[key] = mockObject{data: data, contentType: contentType, etag: etag, metadata: metadata}
	m.mu.Unlock()

	return etag, nil
}

func (m *mockBackend) GetObject(_ context.Context, key string, _ string) (*s3be.GetObjectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getPanic {
		panic("injected GetObject panic for testing")
	}
	if m.getErr != nil {
		return nil, m.getErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	// Return a copy of the data so the caller can read it after the lock is released
	cp := make([]byte, len(obj.data))
	copy(cp, obj.data)
	body := io.NopCloser(bytes.NewReader(cp))
	if m.getReadErr != nil {
		body = io.NopCloser(&errReader{err: m.getReadErr})
	}
	return &s3be.GetObjectResult{
		Body:        body,
		Size:        int64(len(cp)),
		ContentType: obj.contentType,
		ETag:        obj.etag,
		Metadata:    obj.metadata,
	}, nil
}

func (m *mockBackend) HeadObject(_ context.Context, key string) (*s3be.HeadObjectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.headErr != nil {
		return nil, m.headErr
	}
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return &s3be.HeadObjectResult{
		Size:        int64(len(obj.data)),
		ContentType: obj.contentType,
		ETag:        obj.etag,
		Metadata:    obj.metadata,
	}, nil
}

func (m *mockBackend) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	delay := m.delDelay
	m.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delErr != nil {
		return m.delErr
	}
	delete(m.objects, key)
	return nil
}

// hasObject returns true if the key exists in the mock backend's store.
func (m *mockBackend) hasObject(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}
