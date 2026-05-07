// -------------------------------------------------------------------------------
// Error Collector - Accumulating Validator Helper
//
// Author: Alex Freidah
//
// Collector accumulates non-nil errors across a sequence of validation steps
// and returns a single joined result. Avoids the per-validator boilerplate
// of declaring a slice, conditionally appending, and either returning the
// first error or wrapping the rest. Used by config setDefaultsAndValidate
// functions where every step is independent and the caller wants every
// failure surfaced at once rather than only the first.
// -------------------------------------------------------------------------------

// Package errcollect provides a tiny error accumulator used by
// multi-step validation paths.
package errcollect

import "errors"

// Collector accumulates non-nil errors and returns them joined.
// The zero value is ready to use.
type Collector struct {
	errs []error
}

// Append records err if it is non-nil. nil errors are ignored so callers
// can pass any error-returning expression directly without an explicit
// check at the call site.
func (c *Collector) Append(err error) {
	if err != nil {
		c.errs = append(c.errs, err)
	}
}

// Done returns nil when no errors were appended, the single error when
// exactly one was appended, or errors.Join of all appended errors when
// more than one is present.
func (c *Collector) Done() error {
	switch len(c.errs) {
	case 0:
		return nil
	case 1:
		return c.errs[0]
	default:
		return errors.Join(c.errs...)
	}
}
