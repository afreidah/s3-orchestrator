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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/event"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

func TestDampening_SuppressesRepeatedCapacityWarning(t *testing.T) {
	n := &Notifier{}

	dampenKey := event.BackendCapacityWarning + ":oci"

	if _, ok := n.dampener.Load(dampenKey); ok {
		t.Fatal("dampener should be empty initially")
	}

	n.dampener.Store(dampenKey, time.Now())

	if last, ok := n.dampener.Load(dampenKey); ok {
		if time.Since(last.(time.Time)) >= time.Hour {
			t.Error("expected suppression within the hour")
		}
	}

	n.dampener.Store(dampenKey, time.Now().Add(-2*time.Hour))
	if last, ok := n.dampener.Load(dampenKey); ok {
		if time.Since(last.(time.Time)) < time.Hour {
			t.Error("should not suppress after dampening window expires")
		}
	}
}

func TestGenerateEventID_Unique(t *testing.T) {
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

func TestHMACSigning(t *testing.T) {
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

func computeTestHMAC(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestFindEndpoint_Found(t *testing.T) {
	n := &Notifier{
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

func TestFindEndpoint_NotFound(t *testing.T) {
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://a.example.com/hook"},
		},
	}
	ep := n.findEndpoint("https://unknown.example.com")
	if ep != nil {
		t.Error("findEndpoint should return nil for unknown URL")
	}
}

func TestEmit_InsertsNotificationForMatchingEndpoint(t *testing.T) {
	ms := &mockOutboxStore{}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:Put"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "test.txt"})

	if ms.insertCount != 1 {
		t.Errorf("expected 1 insert, got %d", ms.insertCount)
	}
}

func TestEmit_SkipsNonMatchingEndpoint(t *testing.T) {
	ms := &mockOutboxStore{}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:Put"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectRemovedDelete, Subject: "test.txt"})

	if ms.insertCount != 0 {
		t.Errorf("expected 0 inserts for non-matching event, got %d", ms.insertCount)
	}
}

func TestEmit_DampensRepeatedCapacityWarnings(t *testing.T) {
	ms := &mockOutboxStore{}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"*"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.BackendCapacityWarning, Subject: "b1"})
	n.emit(event.Event{Type: event.BackendCapacityWarning, Subject: "b1"})

	if ms.insertCount != 1 {
		t.Errorf("expected 1 insert (second dampened), got %d", ms.insertCount)
	}
}

func TestDeliver_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{client: &http.Client{Timeout: 5 * time.Second}}
	ep := &config.NotificationEndpoint{URL: srv.URL}
	row := store.NotificationRow{Payload: []byte(`{"type":"test"}`)}

	err := n.deliver(context.Background(), row, ep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeliver_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &Notifier{client: &http.Client{Timeout: 5 * time.Second}}
	ep := &config.NotificationEndpoint{URL: srv.URL}
	row := store.NotificationRow{Payload: []byte(`{"type":"test"}`)}

	err := n.deliver(context.Background(), row, ep)
	if err == nil {
		t.Error("expected error on 500 response")
	}
}

func TestDeliver_HMACSignature(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{client: &http.Client{Timeout: 5 * time.Second}}
	ep := &config.NotificationEndpoint{URL: srv.URL, Secret: "test-secret"}
	payload := []byte(`{"type":"test"}`)
	row := store.NotificationRow{Payload: payload}

	_ = n.deliver(context.Background(), row, ep)

	expected := computeTestHMAC(payload, "test-secret")
	if gotSig != expected {
		t.Errorf("signature = %q, want %q", gotSig, expected)
	}
}

func TestNewNotifier_SetsEmitHook(t *testing.T) {
	// Reset the global hook before test
	event.Emit = nil

	cfg := &config.NotificationConfig{
		Endpoints: []config.NotificationEndpoint{{URL: "https://example.com"}},
	}
	ms := &mockOutboxStore{}
	_ = NewNotifier(cfg, ms)

	if event.Emit == nil {
		t.Error("NewNotifier should set event.Emit hook")
	}
	// Clean up
	event.Emit = nil
}

// mockOutboxStore is a minimal stub for notify tests.
type mockOutboxStore struct {
	insertCount  int
	lastPayload  string
	pending      []store.NotificationRow
	completedIDs []int64
	retriedIDs   []int64
}

func (m *mockOutboxStore) InsertNotification(_ context.Context, _, payload, _ string) error {
	m.insertCount++
	m.lastPayload = payload
	return nil
}

func (m *mockOutboxStore) GetPendingNotifications(_ context.Context, _ int) ([]store.NotificationRow, error) {
	return m.pending, nil
}

func (m *mockOutboxStore) CompleteNotification(_ context.Context, id int64) error {
	m.completedIDs = append(m.completedIDs, id)
	return nil
}

func (m *mockOutboxStore) RetryNotification(_ context.Context, id int64, _ time.Duration, _ string) error {
	m.retriedIDs = append(m.retriedIDs, id)
	return nil
}

func (m *mockOutboxStore) WithAdvisoryLock(_ context.Context, _ int64, fn func(context.Context) error) (bool, error) {
	return true, fn(context.Background())
}

func TestEmit_PrefixFiltering(t *testing.T) {
	ms := &mockOutboxStore{}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"s3:ObjectCreated:*"}, Prefix: "uploads/"},
		},
		store: ms,
	}

	// Matching prefix
	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "uploads/photo.jpg"})
	if ms.insertCount != 1 {
		t.Errorf("expected 1 insert for matching prefix, got %d", ms.insertCount)
	}

	// Non-matching prefix
	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "downloads/file.zip"})
	if ms.insertCount != 1 {
		t.Errorf("expected still 1 insert after non-matching prefix, got %d", ms.insertCount)
	}
}

func TestEmit_FillsCloudEventsDefaults(t *testing.T) {
	ms := &mockOutboxStore{}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://hook.example.com", Events: []string{"*"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "test.txt"})

	if ms.insertCount != 1 {
		t.Fatal("expected 1 insert")
	}
	if ms.lastPayload == "" {
		t.Fatal("expected payload to be captured")
	}
	// Verify defaults were filled by checking the serialized payload
	if !contains(ms.lastPayload, `"specversion":"1.0"`) {
		t.Error("expected specversion 1.0 in payload")
	}
	if !contains(ms.lastPayload, `"source":"/s3-orchestrator"`) {
		t.Error("expected source /s3-orchestrator in payload")
	}
	if !contains(ms.lastPayload, `"datacontenttype":"application/json"`) {
		t.Error("expected datacontenttype in payload")
	}
}

func TestEmit_MultipleEndpoints(t *testing.T) {
	ms := &mockOutboxStore{}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: "https://a.example.com", Events: []string{"s3:ObjectCreated:*"}},
			{URL: "https://b.example.com", Events: []string{"*"}},
			{URL: "https://c.example.com", Events: []string{"s3:ObjectRemoved:*"}},
		},
		store: ms,
	}

	n.emit(event.Event{Type: event.ObjectCreatedPut, Subject: "test.txt"})
	// a matches (ObjectCreated:*), b matches (*), c doesn't match
	if ms.insertCount != 2 {
		t.Errorf("expected 2 inserts (a + b), got %d", ms.insertCount)
	}
}

func TestDrainOnce_DeliversAndCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ms := &mockOutboxStore{
		pending: []store.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{"type":"test"}`), EndpointURL: srv.URL},
		},
	}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, Timeout: 5 * time.Second, MaxRetries: 3},
		},
		store:  ms,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	if len(ms.completedIDs) != 1 || ms.completedIDs[0] != 1 {
		t.Errorf("expected ID 1 completed, got %v", ms.completedIDs)
	}
}

func TestDrainOnce_RetriesOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := &mockOutboxStore{
		pending: []store.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{"type":"test"}`), EndpointURL: srv.URL, Attempts: 0},
		},
	}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, Timeout: 5 * time.Second, MaxRetries: 3},
		},
		store:  ms,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	if len(ms.retriedIDs) != 1 || ms.retriedIDs[0] != 1 {
		t.Errorf("expected ID 1 retried, got %v", ms.retriedIDs)
	}
}

func TestDrainOnce_ExhaustsAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ms := &mockOutboxStore{
		pending: []store.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{"type":"test"}`), EndpointURL: srv.URL, Attempts: 2},
		},
	}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{
			{URL: srv.URL, Events: []string{"*"}, Timeout: 5 * time.Second, MaxRetries: 3},
		},
		store:  ms,
		client: &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	// At max retries, should complete (drop) rather than retry
	if len(ms.completedIDs) != 1 {
		t.Errorf("expected exhausted notification to be completed, got completed=%v retried=%v", ms.completedIDs, ms.retriedIDs)
	}
}

func TestDrainOnce_UnknownEndpointCompleted(t *testing.T) {
	ms := &mockOutboxStore{
		pending: []store.NotificationRow{
			{ID: 1, EventType: "test", Payload: []byte(`{}`), EndpointURL: "https://gone.example.com"},
		},
	}
	n := &Notifier{
		endpoints: []config.NotificationEndpoint{},
		store:     ms,
		client:    &http.Client{Timeout: 5 * time.Second},
	}

	n.drainOnce(context.Background())

	if len(ms.completedIDs) != 1 {
		t.Errorf("notification to unknown endpoint should be completed (dropped), got %v", ms.completedIDs)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && strings.Contains(s, substr)
}

func TestHasPrefix(t *testing.T) {
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
