// -------------------------------------------------------------------------------
// Logfmt - Structured Logging Conventions
//
// Author: Alex Freidah
//
// Helper attribute constructors that pin every operational log line to the
// project's logging glossary. The Err helper exists primarily to defeat a
// JSON-handler footgun: passing a raw error value yields {} for any error
// type whose underlying struct lacks JSON tags, which downstream JS log
// viewers render as "[object Object]" and operators cannot grep. Using
// err.Error() guarantees a printable string in every handler.
//
// Outcome / Component / RequestID enforce the canonical attribute keys
// documented in docs/contributing/logging.md so dashboards can aggregate
// without text-matching message strings.
// -------------------------------------------------------------------------------

// Package logfmt provides typed slog.Attr constructors for the project's
// logging conventions.
package logfmt

import (
	"log/slog"
)

// Outcome values for the canonical "outcome" attribute. Use these constants
// rather than ad-hoc strings so dashboards can rely on a closed value set.
const (
	// OutcomeOK marks a successful terminal log line.
	OutcomeOK = "ok"
	// OutcomeError marks a terminal failure.
	OutcomeError = "error"
	// OutcomeSkipped marks a no-op (precondition false, already done, etc).
	OutcomeSkipped = "skipped"
	// OutcomeTimeout marks a deadline-exceeded failure.
	OutcomeTimeout = "timeout"
	// OutcomeNotFound marks a 404-equivalent failure.
	OutcomeNotFound = "not_found"
)

// Err returns a slog.Attr carrying err.Error() under the canonical "error"
// key. Returns an empty Attr (which slog drops) when err is nil so callers
// can use it unconditionally.
//
// Always prefer Err over passing the raw error: slog's JSON handler
// serializes complex error types as {} via encoding/json, which renders as
// "[object Object]" in JS log viewers (Nomad UI, Grafana Loki) and hides
// the actual failure mode from operators.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String("error", err.Error())
}

// Outcome returns a slog.Attr under the canonical "outcome" key. Use one
// of the Outcome* constants for the value.
func Outcome(value string) slog.Attr {
	return slog.String("outcome", value)
}

// Component returns a slog.Attr under the canonical "component" key. Use
// at logger construction so every log from a long-lived service carries
// the same component label, not in the message string.
func Component(name string) slog.Attr {
	return slog.String("component", name)
}
