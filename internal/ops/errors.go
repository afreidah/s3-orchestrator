// -------------------------------------------------------------------------------
// Ops - Skip and Rejection Errors
//
// Author: Alex Freidah
//
// An operation declines work for reasons that are not failures: a subsystem is
// turned off in config, or a worker planned no work for this pass. Both are
// reported as a SkipError so each transport can word the outcome for its own
// audience without the operations layer knowing what a wire status is.
// -------------------------------------------------------------------------------

package ops

import "errors"

// SkipError reports an operation that declined to run, and why. Transports
// match it with errors.As and render Reason in whatever shape their protocol
// uses.
type SkipError struct {
	Reason string
}

// Error reports the skip reason.
func (e *SkipError) Error() string {
	return "skipped: " + e.Reason
}

// Skip builds a SkipError for a reason decided at runtime, such as the skip a
// worker reports back from a planning pass.
func Skip(reason string) error {
	return &SkipError{Reason: reason}
}

// Skips for subsystems that are unavailable in the running configuration.
// Each is a *SkipError, so a transport handles a fixed and a runtime skip
// through the same errors.As branch.
var (
	ErrIntegrityDisabled     = &SkipError{Reason: "integrity verification is not enabled"}
	ErrReplicationDisabled   = &SkipError{Reason: "replication not configured or factor <= 1"}
	ErrEncryptionDisabled    = &SkipError{Reason: "encryption not enabled"}
	ErrRebalancerUnavailable = &SkipError{Reason: "rebalancer not available"}
	ErrLifecycleUnavailable  = &SkipError{Reason: "lifecycle manager not available"}

	// ErrCompressionUnavailable reports no codec, which is a different state
	// from compression being disabled for writes: a codec is built either way
	// so stored objects stay readable, and only its absence stops a rewrite.
	ErrCompressionUnavailable = &SkipError{Reason: "compression codec not available"}
)

// Rejections an object operation raises before it reaches a backend. Each maps
// to a client error rather than a server fault.
var (
	ErrKeyRequired    = errors.New("key is required")
	ErrKeyIDRequired  = errors.New("old_key_id is required")
	ErrPrefixRequired = errors.New("prefix is required")
	ErrInvalidKey     = errors.New("key must start with a configured bucket name")
	ErrNotFound       = errors.New("object not found")
)
