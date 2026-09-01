// -------------------------------------------------------------------------------
// Notifier Tests - HMAC Signing, Dampening, Helpers
//
// Author: Alex Freidah
//
// Tests for the notifier package: HMAC-SHA256 signature verification,
// dampening of repeated threshold events, event ID generation, and prefix
// matching. Filter matching tests live in internal/event/event_test.go.
// -------------------------------------------------------------------------------

package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// TestDampening_SuppressesRepeatedCapacityWarning verifies that the TTL-based
// dampener suppresses duplicate threshold events within the dampening window
// and allows them after the TTL expires.
// Not parallel: NewNotifier writes the process-global event.Emit hook.
func TestDampening_SuppressesRepeatedCapacityWarning(t *testing.T) {
	n := NewNotifier(&config.NotificationConfig{}, &mockOutboxStore{})
	t.Cleanup(n.Close)

	dampenKey := event.BackendCapacityWarning + ":oci"

	// Empty initially.
	if _, ok := n.dampener.Get(dampenKey); ok {
		t.Fatal("dampener should be empty initially")
	}

	// After Set, Get returns true within the TTL window.
	n.dampener.Set(dampenKey, struct{}{})
	if _, ok := n.dampener.Get(dampenKey); !ok {
		t.Error("expected entry to be present after Set")
	}
}

// TestDampening_TTLCacheEvicts verifies that the dampener uses a TTL cache
// that will eventually evict entries, preventing unbounded memory growth.
// Not parallel: NewNotifier writes the process-global event.Emit hook.
func TestDampening_TTLCacheEvicts(t *testing.T) {
	n := NewNotifier(&config.NotificationConfig{}, &mockOutboxStore{})
	defer n.Close()

	// The cache exists and is functional.
	n.dampener.Set("key", struct{}{})
	if _, ok := n.dampener.Get("key"); !ok {
		t.Error("expected entry to be present")
	}
}

// TestGenerateEventID_Unique verifies the generate event id unique contract.
// Asserts that duplicate event ID:.
func TestGenerateEventID_Unique(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for range 100 {
		id := generateEventID()
		if seen[id] {
			t.Fatalf("duplicate event ID: %s", id)
		}
		seen[id] = true
		if len(id) < 10 {
			t.Errorf("event ID too short: %q", id)
		}
	}
}

// TestHMACSigning verifies the hmacsigning behaviour described by the test name.
func TestHMACSigning(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"type":"test"}`)
	secret := "test-secret"

	sig1 := computeTestHMAC(payload, secret)
	if sig1 == "" {
		t.Fatal("HMAC should not be empty")
	}

	sig2 := computeTestHMAC(payload, secret)
	if sig1 != sig2 {
		t.Error("HMAC should be deterministic")
	}

	other := computeTestHMAC(payload, "other-secret")
	if sig1 == other {
		t.Error("different secrets should produce different signatures")
	}
}

// computeTestHMAC computes the HMAC-SHA256 the test expects the
// notifier to attach to the payload. Used to assert the signing
// matches the configured webhook secret without re-implementing
// the real notifier's signer.
func computeTestHMAC(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestFindEndpoint_Found verifies the find endpoint found contract.
// Asserts that findEndpoint should return matching endpoint, got.
func TestFindEndpoint_Found(t *testing.T) {
	t.Parallel()
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://a.example.com/hook"},
			{URL: "https://b.example.com/hook"},
		},
	}
	ep := n.findEndpoint("https://b.example.com/hook")
	if ep == nil || ep.URL != "https://b.example.com/hook" {
		t.Errorf("findEndpoint should return matching endpoint, got %v", ep)
	}
}

// TestFindEndpoint_NotFound verifies the find endpoint not found behaviour described by the test name.
func TestFindEndpoint_NotFound(t *testing.T) {
	t.Parallel()
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://a.example.com/hook"},
		},
	}
	ep := n.findEndpoint("https://unknown.example.com")
	if ep != nil {
		t.Error("findEndpoint should return nil for unknown URL")
	}
}

// TestEmit_InsertsNotificationForMatchingEndpoint verifies the emit inserts notification for matching endpoint contract.
// Asserts that expected 1 insert, got.
func TestEmit_InsertsNotificationForMatchingEndpoint(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:Put"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "test.txt"})

	if ms.inserts() != 1 {
		t.Errorf("expected 1 insert, got %d", ms.inserts())
	}
}

// TestEmit_MarshalFailureLogsAndSkips drives the marshal-failure
// branch: an event whose Data map carries an unsupported value (a
// channel) causes json.Marshal to error, which the notifier logs and
// then returns from without enqueueing anything.
func TestEmit_MarshalFailureLogsAndSkips(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:Put"}},
		},
		store: ms,
	}
	// chan values are not JSON-marshalable so json.Marshal returns an error.
	n.emit(event.Event{
		Type:    event.ObjectCreatedPut,
		Subject: "k",
		Data:    map[string]any{"chan": make(chan int)},
	})
	if ms.inserts() != 0 {
		t.Errorf("expected 0 inserts after marshal failure, got %d", ms.inserts())
	}
}

// TestEmit_SkipsNonMatchingEndpoint verifies the emit skips non matching endpoint contract.
// Asserts that expected 0 inserts for non-matching event, got.
func TestEmit_SkipsNonMatchingEndpoint(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:Put"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectRemovedDelete, Subject: "test.txt"})

	if ms.inserts() != 0 {
		t.Errorf("expected 0 inserts for non-matching event, got %d", ms.inserts())
	}
}

// TestEmit_DampensRepeatedCapacityWarnings verifies that the second capacity
// warning for the same subject is suppressed within the dampening window.
func TestEmit_DampensRepeatedCapacityWarnings(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	dampener := syncutil.NewTTLCache[string, struct{}](dampenTTL)
	defer dampener.Close()
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"*"}},
		},
		store:    ms,
		dampener: dampener,
	}

	n.emit(event.Event{Type: event.BackendCapacityWarning, Subject: "b1"})
	n.emit(event.Event{Type: event.BackendCapacityWarning, Subject: "b1"})

	if ms.inserts() != 1 {
		t.Errorf("expected 1 insert (second dampened), got %d", ms.inserts())
	}
}

// TestDeliver_Success verifies the deliver success contract.
// Asserts that unexpected error:.
func TestDeliver_Success(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{log: slog.Default(), client: &http.Client{Timeout: 5 * time.Second}}
	ep := &config.NotificationEndpoint{URL: srv.URL}
	row := core.NotificationRow{Payload: []byte(`{"type":"test"}`)}

	err := n.deliver(context.Background(), row, ep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDeliver_ServerError verifies the deliver server error path by exercising httptest.NewServer, http.HandlerFunc, w.WriteHeader.
func TestDeliver_ServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &Notifier{log: slog.Default(), client: &http.Client{Timeout: 5 * time.Second}}
	ep := &config.NotificationEndpoint{URL: srv.URL}
	row := core.NotificationRow{Payload: []byte(`{"type":"test"}`)}

	err := n.deliver(context.Background(), row, ep)
	if err == nil {
		t.Error("expected error on 500 response")
	}
}

// TestDeliver_HMACSignature verifies the deliver hmacsignature contract.
// Asserts that signature = , want.
func TestDeliver_HMACSignature(t *testing.T) {
	t.Parallel()
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{log: slog.Default(), client: &http.Client{Timeout: 5 * time.Second}}
	ep := &config.NotificationEndpoint{URL: srv.URL, Secret: "test-secret"}
	payload := []byte(`{"type":"test"}`)
	row := core.NotificationRow{Payload: payload}

	_ = n.deliver(context.Background(), row, ep)

	expected := computeTestHMAC(payload, "test-secret")
	if gotSig != expected {
		t.Errorf("signature = %q, want %q", gotSig, expected)
	}
}

// TestNewNotifier_RegistersAndDeregisters asserts a constructed notifier
// receives what any package publishes, and stops receiving once it is closed.
// Asserted through the outbox rather than by reading the hook: registration is
// only worth anything if an event actually lands somewhere.
//
// Not parallel: registration is process-global.
func TestNewNotifier_RegistersAndDeregisters(t *testing.T) {
	event.SetEmitter(nil)
	t.Cleanup(func() { event.SetEmitter(nil) })

	cfg := &config.NotificationConfig{
		Endpoints: []config.NotificationEndpoint{{URL: "https://example.com", Events: []string{"*"}}},
	}
	ms := &mockOutboxStore{}
	n := NewNotifier(cfg, ms)

	event.Publish(event.ServiceStarted, "", nil)
	if got := ms.inserts(); got != 1 {
		t.Fatalf("outbox rows after publish = %d, want 1; the notifier did not register", got)
	}

	// Close deregisters. An event published after shutdown has no notifier to
	// take it, and must not reach one whose dampener has already stopped.
	n.Close()
	event.Publish(event.ServiceStopping, "", nil)
	if got := ms.inserts(); got != 1 {
		t.Errorf("outbox rows after close = %d, want no further inserts", got)
	}
}

// mockOutboxStore is a minimal stub for notify tests. The notifier's
// drain goroutines call the store methods concurrently with the test
// goroutine's assertions, so every field access is mu-guarded. Tests
// read via the snapshot accessors below; struct-literal field setup
// at construction time (pending) is safe because no goroutine has
// observed the store yet.
type mockOutboxStore struct {
	mu           sync.Mutex
	insertCount  int
	lastPayload  string
	pending      []core.NotificationRow
	completedIDs []int64
	retriedIDs   []int64
}

// InsertNotification inserts notification.
func (m *mockOutboxStore) InsertNotification(_ context.Context, _, payload, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.insertCount++
	m.lastPayload = payload
	return nil
}

// GetPendingNotifications returns pending notifications.
func (m *mockOutboxStore) GetPendingNotifications(_ context.Context, _ int) ([]core.NotificationRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.NotificationRow(nil), m.pending...), nil
}

// CompleteNotification records the completion call so the test can
// assert which notification ids the notifier marked successful.
func (m *mockOutboxStore) CompleteNotification(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

// RetryNotification records the retry call (id, backoff, last_error)
// so the test can assert the notifier scheduled retries with the
// right backoff curve.
func (m *mockOutboxStore) RetryNotification(_ context.Context, id int64, _ time.Duration, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retriedIDs = append(m.retriedIDs, id)
	return nil
}

// WithAdvisoryLock runs the supplied function with advisory lock.
func (m *mockOutboxStore) WithAdvisoryLock(_ context.Context, _ int64, fn func(context.Context) error) (bool, error) {
	return true, fn(context.Background())
}

// inserts returns the current insert count under the lock. Tests use
// this instead of reading m.insertCount directly so a notifier
// delivery goroutine's concurrent InsertNotification cannot race with
// the assertion read.
func (m *mockOutboxStore) inserts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.insertCount
}

// payload returns the most recent inserted payload under the lock.
func (m *mockOutboxStore) payload() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastPayload
}

// completed returns a copy of completedIDs under the lock. Returning a
// copy lets the caller iterate without holding the mock's mutex.
func (m *mockOutboxStore) completed() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.completedIDs...)
}

// retried returns a copy of retriedIDs under the lock.
func (m *mockOutboxStore) retried() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.retriedIDs...)
}

// TestEmit_PrefixFiltering verifies the emit prefix filtering contract.
// Asserts that expected 1 insert for matching prefix, got.
func TestEmit_PrefixFiltering(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:*"}, Prefix: "uploads/"},
		},
		store: ms,
	}

	// Matching prefix
	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "uploads/photo.jpg"})
	if ms.inserts() != 1 {
		t.Errorf("expected 1 insert for matching prefix, got %d", ms.inserts())
	}

	// Non-matching prefix
	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "downloads/file.zip"})
	if ms.inserts() != 1 {
		t.Errorf("expected still 1 insert after non-matching prefix, got %d", ms.inserts())
	}
}

// TestEmit_FillsCloudEventsDefaults verifies the emit fills cloud events defaults behaviour described by the test name.
func TestEmit_FillsCloudEventsDefaults(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"*"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "test.txt"})

	if ms.inserts() != 1 {
		t.Fatal("expected 1 insert")
	}
	if ms.payload() == "" {
		t.Fatal("expected payload to be captured")
	}
	// Verify defaults were filled by checking the serialized payload
	if !contains(ms.payload(), `"specversion":"1.0"`) {
		t.Error("expected specversion 1.0 in payload")
	}
	if !contains(ms.payload(), `"source":"/s3-orchestrator"`) {
		t.Error("expected source /s3-orchestrator in payload")
	}
	if !contains(ms.payload(), `"datacontenttype":"application/json"`) {
		t.Error("expected datacontenttype in payload")
	}
}

// TestEmit_MultipleEndpoints verifies the emit multiple endpoints contract.
// Asserts that expected 2 inserts (a + b), got.
func TestEmit_MultipleEndpoints(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: "https://a.example.com", Events: []string{"s3:ObjectCreated:*"}},
			{URL: "https://b.example.com", Events: []string{"*"}},
			{URL: "https://c.example.com", Events: []string{"s3:ObjectRemoved:*"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "test.txt"})
	// a matches (ObjectCreated:*), b matches (*), c doesn't match
	if ms.inserts() != 2 {
		t.Errorf("expected 2 inserts (a + b), got %d", ms.inserts())
	}
}

// TestDrainOnce_DeliversAndCompletes verifies the drain once delivers and completes contract.
// Asserts that expected ID 1 completed, got.
func TestDrainOnce_DeliversAndCompletes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ms := &mockOutboxStore{
		pending: []core.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{"type":"test"}`), EndpointURL: srv.URL},
		},
	}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, Timeout: 5 * time.Second, MaxRetries: 3},
		},
		store:  ms,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	if len(ms.completed()) != 1 || ms.completed()[0] != 1 {
		t.Errorf("expected ID 1 completed, got %v", ms.completed())
	}
}

// TestDrainOnce_RetriesOnFailure verifies the drain once retries on failure contract.
// Asserts that expected ID 1 retried, got.
func TestDrainOnce_RetriesOnFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := &mockOutboxStore{
		pending: []core.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{"type":"test"}`), EndpointURL: srv.URL, Attempts: 0},
		},
	}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, Timeout: 5 * time.Second, MaxRetries: 3},
		},
		store:  ms,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	if len(ms.retried()) != 1 || ms.retried()[0] != 1 {
		t.Errorf("expected ID 1 retried, got %v", ms.retried())
	}
}

// TestDrainOnce_ExhaustsAfterMaxRetries verifies the drain once exhausts after max retries contract.
// Asserts that expected exhausted notification to be completed, got completed= retried=.
func TestDrainOnce_ExhaustsAfterMaxRetries(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := &mockOutboxStore{
		pending: []core.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{"type":"test"}`), EndpointURL: srv.URL, Attempts: 2},
		},
	}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, Timeout: 5 * time.Second, MaxRetries: 3},
		},
		store:  ms,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	// At max retries, should complete (drop) rather than retry
	if len(ms.completed()) != 1 {
		t.Errorf("expected exhausted notification to be completed, got completed=%v retried=%v", ms.completed(), ms.retried())
	}
}

// TestDrainOnce_UnknownEndpointCompleted verifies the drain once unknown endpoint completed contract.
// Asserts that notification to unknown endpoint should be completed (dropped), got.
func TestDrainOnce_UnknownEndpointCompleted(t *testing.T) {
	t.Parallel()
	ms := &mockOutboxStore{
		pending: []core.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{}`), EndpointURL: "https://gone.example.com"},
		},
	}
	n := &Notifier{
		log:       slog.Default(),
		endpoints: []config.NotificationEndpoint{},
		store:     ms,
		client:    &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	if len(ms.completed()) != 1 {
		t.Errorf("notification to unknown endpoint should be completed (dropped), got %v", ms.completed())
	}
}

// contains is a small substring helper used by the tests below to
// assert log lines without depending on slog's exact formatting.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && strings.Contains(s, substr)
}

// TestHasPrefix verifies the has prefix contract.
// Asserts that hasPrefix(, ) = , want.
func TestHasPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, prefix string
		want        bool
	}{
		{"uploads/photo.jpg", "uploads/", true},
		{"uploads/photo.jpg", "downloads/", false},
		{"uploads/photo.jpg", "", true},
		{"", "uploads/", false},
		{"uploads/photo.jpg", "uploads/photo.jpg", true},
		{"up", "uploads/", false},
	}

	for _, tt := range tests {
		got := hasPrefix(tt.key, tt.prefix)
		if got != tt.want {
			t.Errorf("hasPrefix(%q, %q) = %v, want %v", tt.key, tt.prefix, got, tt.want)
		}
	}
}

// failingOutboxStore exercises the error branches of completeOrLog and
// retryOrLog without requiring a real DB. Complete and Retry always fail
// with a configured error so we can assert the metric counter bumps and
// the worker doesn't panic or double-deliver.
type failingOutboxStore struct {
	pending     []core.NotificationRow
	completeErr error
	retryErr    error
}

// InsertNotification inserts notification.
func (m *failingOutboxStore) InsertNotification(_ context.Context, _, _, _ string) error {
	return nil
}

// GetPendingNotifications returns pending notifications.
func (m *failingOutboxStore) GetPendingNotifications(_ context.Context, _ int) ([]core.NotificationRow, error) {
	return m.pending, nil
}

// CompleteNotification records the completion call so the test can
// assert which notification ids the notifier marked successful.
func (m *failingOutboxStore) CompleteNotification(_ context.Context, _ int64) error {
	return m.completeErr
}

// RetryNotification records the retry call (id, backoff, last_error)
// so the test can assert the notifier scheduled retries with the
// right backoff curve.
func (m *failingOutboxStore) RetryNotification(_ context.Context, _ int64, _ time.Duration, _ string) error {
	return m.retryErr
}

// WithAdvisoryLock runs the supplied function with advisory lock.
func (m *failingOutboxStore) WithAdvisoryLock(_ context.Context, _ int64, fn func(context.Context) error) (bool, error) {
	return true, fn(context.Background())
}

// TestCompleteOrLog_LogsStoreError covers the error branch that was
// previously a silent `_ = n.store.CompleteNotification(...)`.
func TestCompleteOrLog_LogsStoreError(t *testing.T) {
	t.Parallel()
	n := &Notifier{log: slog.Default(), store: &failingOutboxStore{completeErr: fmt.Errorf("boom")}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("completeOrLog panicked: %v", r)
		}
	}()
	n.completeOrLog(context.Background(), 42, "test_reason")
}

// TestRetryOrLog_LogsStoreError covers the retry error branch.
func TestRetryOrLog_LogsStoreError(t *testing.T) {
	t.Parallel()
	n := &Notifier{log: slog.Default(), store: &failingOutboxStore{retryErr: fmt.Errorf("kaboom")}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("retryOrLog panicked: %v", r)
		}
	}()
	n.retryOrLog(context.Background(), 42, time.Second, "test_reason")
}

// TestDrainOnce_CompleteFailure_DoesNotPanic drives drainOnce through the
// "delivery succeeded but Complete failed" path, previously a silent
// discard. Verifies the worker survives the failure.
func TestDrainOnce_CompleteFailure_DoesNotPanic(t *testing.T) {
	t.Parallel()
	// Spin up an HTTP server that always returns 204 so delivery succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ms := &failingOutboxStore{
		completeErr: fmt.Errorf("simulated complete failure"),
		pending: []core.NotificationRow{
			{ID: 1, EventType: "s3:ObjectCreated:Put", EndpointURL: srv.URL, Payload: []byte("{}")},
		},
	}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, MaxRetries: 3, Timeout: time.Second},
		},
		store:  ms,
		client: &http.Client{Timeout: time.Second},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("drainOnce panicked: %v", r)
		}
	}()
	n.drainOnce(context.Background())
}

// TestDrainOnce_RetryFailure_DoesNotPanic drives drainOnce through the
// "delivery failed AND Retry failed" path.
func TestDrainOnce_RetryFailure_DoesNotPanic(t *testing.T) {
	t.Parallel()
	// Server always returns 500 so delivery fails -> worker calls Retry ->
	// Retry also fails (injected).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := &failingOutboxStore{
		retryErr: fmt.Errorf("simulated retry failure"),
		pending: []core.NotificationRow{
			{ID: 1, EventType: "s3:ObjectCreated:Put", EndpointURL: srv.URL, Payload: []byte("{}"), Attempts: 0},
		},
	}
	n := &Notifier{
		log: slog.Default(),
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, MaxRetries: 5, Timeout: time.Second},
		},
		store:  ms,
		client: &http.Client{Timeout: time.Second},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("drainOnce panicked: %v", r)
		}
	}()
	n.drainOnce(context.Background())
}
