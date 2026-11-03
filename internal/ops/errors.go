// -------------------------------------------------------------------------------
// Ops - Skip and Rejection Errors
//
// Author: Alex Freidah
//
// An operation declines work for reasons that are not failures: a subsystem is
// turned off in config, or a worker planned no work for this pass. Both are
// reported as a SkipError. A request the operation refuses outright is a
// Rejection carrying its kind. Each transport words both for its own audience
// without the operations layer knowing what a wire status is.
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

// SkipReason reports whether err is a skip anywhere in its chain, and its
// reason.
func SkipReason(err error) (string, bool) {
	if skip, ok := errors.AsType[*SkipError](err); ok {
		return skip.Reason, true
	}
	return "", false
}

// Skips for subsystems that are unavailable in the running configuration.
// Each is a *SkipError, so a transport handles a fixed and a runtime skip
// through the same errors.As branch. ErrCompressionUnavailable means no codec
// was built, not that compression is disabled for writes.
var (
	ErrIntegrityDisabled     = &SkipError{Reason: "integrity verification is not enabled"}
	ErrReplicationDisabled   = &SkipError{Reason: "replication not configured or factor <= 1"}
	ErrEncryptionDisabled    = &SkipError{Reason: "encryption not enabled"}
	ErrRebalancerUnavailable = &SkipError{Reason: "rebalancer not available"}
	ErrLifecycleUnavailable  = &SkipError{Reason: "lifecycle manager not available"}
	ErrScrubInProgress       = &SkipError{Reason: "another instance is already scrubbing"}

	ErrCompressionUnavailable = &SkipError{Reason: "compression codec not available"}
)

// RejectionKind is what a rejection says about the request: something in it
// is invalid, names nothing that exists, asks for what may never be done, or
// conflicts with the state it would change.
type RejectionKind int

// The rejection kinds.
const (
	RejectInvalid RejectionKind = iota + 1
	RejectNotFound
	RejectForbidden
	RejectConflict
)

// Rejection reports a request an operation refused before changing anything.
// Its message is written for the caller, who stated or asked for what it
// names.
type Rejection struct {
	Kind RejectionKind
	err  error
}

// Error reports what was wrong with the request.
func (r *Rejection) Error() string { return r.err.Error() }

// Unwrap returns the refusal the rejection was built from.
func (r *Rejection) Unwrap() error { return r.err }

// reject builds a rejection of kind with a fixed message.
func reject(kind RejectionKind, msg string) *Rejection {
	return &Rejection{Kind: kind, err: errors.New(msg)}
}

// Rejections an object operation raises before it reaches a backend.
var (
	ErrKeyRequired    = reject(RejectInvalid, "key is required")
	ErrKeyIDRequired  = reject(RejectInvalid, "exactly one of old_key_id or all_stale is required")
	ErrPrefixRequired = reject(RejectInvalid, "prefix is required")
	ErrInvalidKey     = reject(RejectInvalid, "key must start with a configured bucket name")
	ErrNotFound       = reject(RejectNotFound, "object not found")
)

// Rejections a provisioning operation raises. ErrConfigDeclared covers every
// entry the config file declares: those are visible through the API and never
// editable through it, so an operator reading that file can trust what it says.
var (
	ErrNameRequired       = reject(RejectInvalid, "name is required")
	ErrUserRequired       = reject(RejectInvalid, "user is required")
	ErrConfigDeclared     = reject(RejectForbidden, "declared in the config file and not editable through the API")
	ErrBucketExists       = reject(RejectConflict, "bucket already exists")
	ErrBucketNotFound     = reject(RejectNotFound, "bucket not found")
	ErrBucketNotEmpty     = reject(RejectConflict, "bucket still holds objects")
	ErrBucketGranted      = reject(RejectConflict, "bucket is still granted to a user")
	ErrUserNotFound       = reject(RejectNotFound, "user not found")
	ErrUserInUse          = reject(RejectConflict, "user still holds credentials or grants")
	ErrCredentialNotFound = reject(RejectNotFound, "credential not found")
	ErrCredentialExists   = reject(RejectConflict, "access key is already claimed")
	ErrKeypairIncomplete  = reject(RejectInvalid,
		"access_key_id and secret_access_key must both be supplied, or both omitted to mint one")
	ErrInvalidBucket   = reject(RejectInvalid, "bucket is not valid")
	ErrNoPermissions   = reject(RejectInvalid, "a grant must carry at least one permission")
	ErrBackendNotFound = reject(RejectNotFound, "backend not found")
	ErrInvalidResource = reject(RejectInvalid, "grant resource is not valid")
)
