// -------------------------------------------------------------------------------
// Event - Notification Event Types and Hook
//
// Author: Alex Freidah
//
// Leaf package with zero internal dependencies. Defines notification event
// types, the Event struct, and the emitter hook every package publishes
// through. Every package in the codebase can import this without creating
// cycles. The notifier registers itself at startup; Publish is a no-op until
// it does.
// -------------------------------------------------------------------------------

package event

import (
	"strings"
	"sync/atomic"
	"time"
)

// -------------------------------------------------------------------------
// EMIT HOOK
// -------------------------------------------------------------------------

// emitter holds the notifier's delivery hook, or nil when the deployment has
// no notifications configured.
//
// Atomic, and unexported so the only ways to touch it are SetEmitter and
// Publish. The hook is written once during wiring and read from every request
// and worker goroutine in the process; a plain func var is a data race the
// moment anything writes it after startup, which registering the notifier from
// a reload hook would do. audit.onEvent is the same shape for the same reason.
var emitter atomic.Pointer[func(Event)]

// SetEmitter registers the hook Publish delivers through. Pass nil to clear a
// previously registered one, which is what a notifier does when it stops.
func SetEmitter(fn func(Event)) {
	if fn == nil {
		emitter.Store(nil)
		return
	}
	emitter.Store(&fn)
}

// Publish sends one event to the configured notifier, or does nothing when the
// deployment has none. The envelope fields a CloudEvent needs are filled in by
// the notifier, so a caller supplies only what is specific to the occurrence.
//
// subject is the resource the event is about - a backend name, an object key -
// and is empty for fleet-wide events that name no single one.
func Publish(eventType, subject string, data map[string]any) {
	fn := emitter.Load()
	if fn == nil {
		return
	}
	(*fn)(Event{Type: eventType, Subject: subject, Data: data})
}

// -------------------------------------------------------------------------
// EVENT TYPE CONSTANTS
// -------------------------------------------------------------------------

// Operational events  -  infrastructure state changes.
const (
	BackendCircuitOpened       = "backend.circuit.opened"
	BackendCircuitClosed       = "backend.circuit.closed"
	BackendCapacityWarning     = "backend.capacity.warning"
	IntegrityCorruptionFound   = "integrity.corruption_detected"
	CleanupExhausted           = "cleanup.exhausted"
	ReplicationTargetExhausted = "replication.target_exhausted"
	BackendDrainFailed         = "backend.drain.failed"
	ConfigReloadFailed         = "config.reload_failed"
	BackendDrainCompleted      = "backend.drain.completed"
	BackendRemoved             = "backend.removed"
	RebalanceCompleted         = "rebalance.completed"
	ReplicationCompleted       = "replication.completed"
	LifecycleCompleted         = "lifecycle.completed"
	ServiceStarted             = "service.started"
	ServiceStopping            = "service.stopping"
)

// Data events  -  S3-compatible object mutation names.
const (
	ObjectCreatedPut                     = "s3:ObjectCreated:Put"
	ObjectCreatedCopy                    = "s3:ObjectCreated:Copy"
	ObjectCreatedCompleteMultipartUpload = "s3:ObjectCreated:CompleteMultipartUpload"
	ObjectRemovedDelete                  = "s3:ObjectRemoved:Delete"
	ObjectRemovedDeleteBatch             = "s3:ObjectRemoved:DeleteBatch"
	LifecycleDelete                      = "lifecycle.delete"
)

// -------------------------------------------------------------------------
// EVENT STRUCT
// -------------------------------------------------------------------------

// Event represents a CloudEvents 1.0 notification. The Data field carries
// event-specific attributes as a map for JSON serialization flexibility.
type Event struct {
	SpecVersion     string         `json:"specversion"`
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Type            string         `json:"type"`
	Time            time.Time      `json:"time"`
	Subject         string         `json:"subject,omitempty"`
	DataContentType string         `json:"datacontenttype"`
	Data            map[string]any `json:"data"`
}

// -------------------------------------------------------------------------
// FILTER MATCHING
// -------------------------------------------------------------------------

// MatchesFilter reports whether an event type matches any of the configured
// filter patterns. Supports trailing wildcards: "s3:ObjectCreated:*" matches
// "s3:ObjectCreated:Put", and "backend.*" matches "backend.circuit.opened".
func MatchesFilter(eventType string, patterns []string) bool {
	for _, p := range patterns {
		if p == "*" || p == eventType {
			return true
		}
		if strings.HasSuffix(p, "*") {
			prefix := p[:len(p)-1]
			if strings.HasPrefix(eventType, prefix) {
				return true
			}
		}
	}
	return false
}
