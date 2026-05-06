// -------------------------------------------------------------------------------
// Logfmt Slog Handler Middleware
//
// Author: Alex Freidah
//
// ErrAttrHandler is a slog.Handler middleware that stringifies any attribute
// whose value is a Go error before delegating to the wrapped handler. The
// stdlib JSON handler renders error types via encoding/json, which produces
// "{}" for any error whose underlying struct lacks JSON tags  -  rendering as
// "[object Object]" in JS log viewers and hiding the actual failure mode.
//
// Wrapping the JSON handler with ErrAttrHandler means call sites can pass the
// raw error (`slog.X(ctx, "msg", "error", err)`) and operators still get a
// printable string downstream. logfmt.Err remains available for callers who
// prefer the explicit slog.Attr style.
// -------------------------------------------------------------------------------

package logfmt

import (
	"context"
	"log/slog"
)

// ErrAttrHandler wraps a slog.Handler and replaces any attribute value of
// type error (passed via slog.Any or as a raw key/value pair) with the
// result of err.Error(). Group attributes are recursed into. Nil errors
// land as empty strings under the original key.
type ErrAttrHandler struct {
	inner slog.Handler
}

// NewErrAttrHandler wraps inner so error-typed attribute values render as
// their Error() string instead of the JSON handler's "{}" placeholder.
func NewErrAttrHandler(inner slog.Handler) *ErrAttrHandler {
	return &ErrAttrHandler{inner: inner}
}

// Enabled forwards to the inner handler.
func (h *ErrAttrHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.inner.Enabled(ctx, lvl)
}

// Handle walks record attributes, converting error-typed values to
// strings, then delegates to the inner handler.
//
//nolint:gocritic // slog.Handler.Handle's interface signature requires Record by value.
func (h *ErrAttrHandler) Handle(ctx context.Context, r slog.Record) error {
	transformed := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		transformed.AddAttrs(TransformAttr(a))
		return true
	})
	return h.inner.Handle(ctx, transformed)
}

// WithAttrs delegates after transforming any error-typed attrs in the
// scoping list so With(...) chains preserve the same rendering.
func (h *ErrAttrHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = TransformAttr(a)
	}
	return &ErrAttrHandler{inner: h.inner.WithAttrs(out)}
}

// WithGroup delegates unchanged; group nesting is handled by the inner handler.
func (h *ErrAttrHandler) WithGroup(name string) slog.Handler {
	return &ErrAttrHandler{inner: h.inner.WithGroup(name)}
}

// TransformAttr returns a if its value is not an error, otherwise returns
// a new slog.Attr with the same key and value `a.Value.Any().(error).Error()`.
// Group values are recursed into so nested groups carrying errors also
// render as strings. Exported so non-handler call sites (e.g. the
// in-memory log buffer) can apply the same rule outside the slog.Handler
// chain.
func TransformAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			if err == nil {
				return slog.String(a.Key, "")
			}
			return slog.String(a.Key, err.Error())
		}
	case slog.KindGroup:
		group := a.Value.Group()
		out := make([]slog.Attr, len(group))
		for i, child := range group {
			out[i] = TransformAttr(child)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}
