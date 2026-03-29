// -------------------------------------------------------------------------------
// Event - Notification Event Types and Hook
//
// Author: Alex Freidah
//
// Leaf package with zero internal dependencies. Defines notification event
// types, the Event struct, and the package-level Emit hook. Every package in
// the codebase can import this without creating cycles. The notifier sets the
// Emit function at startup; callers nil-check before calling.
// -------------------------------------------------------------------------------

package event

import (
	"strings"
	"time"
)

// -------------------------------------------------------------------------
// EMIT HOOK
// -------------------------------------------------------------------------

// Emit is the package-level hook for emitting notification events. Nil when
// notifications are not configured. Callers must nil-check before calling.
var Emit func(Event)

// -------------------------------------------------------------------------
// EVENT TYPE CONSTANTS
// -------------------------------------------------------------------------

// Operational events — infrastructure state changes.
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

// Data events — S3-compatible object mutation names.
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
