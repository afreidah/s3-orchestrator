// -------------------------------------------------------------------------------
// Logfmt - Structured Logging Conventions
//
// Author: Alex Freidah
//
// Helper attribute constructors that pin every operational log line to the
// project's logging glossary. The Err helper exists primarily to defeat a
// JSON-handler behaviour: passing a raw error value yields {} for any error
// type whose underlying struct lacks JSON tags, which downstream JS log
// viewers render as "[object Object]" and operators cannot grep. Using
// err.Error() guarantees a printable string in every handler. Component pins
// the canonical key documented in docs/contributing/logging.md, and Request
// names an inbound HTTP request the same way on every surface.
// -------------------------------------------------------------------------------

package logfmt

import (
	"log/slog"
	"net/http"
)

// Err returns a slog.Attr carrying err.Error() under the canonical "error"
// key, or an empty Attr (which slog drops) when err is nil. Use it in
// typed-attr calls and where err may be nil; elsewhere pass "error", err.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String("error", err.Error())
}

// Component returns a slog.Attr under the canonical "component" key. Use
// at logger construction so every log from a long-lived service carries
// the same component label, not in the message string.
func Component(name string) slog.Attr {
	return slog.String("component", name)
}

// Request returns the method, path and client address that name an inbound
// HTTP request in a log line or audit entry, followed by extra. Every surface
// starts its request records with these three, so one query reads them all.
func Request(r *http.Request, extra ...slog.Attr) []slog.Attr {
	return RequestFrom(r, r.RemoteAddr, extra...)
}

// RequestFrom is Request naming client, an address the caller resolved through
// its trusted proxies, in place of r.RemoteAddr.
func RequestFrom(r *http.Request, client string, extra ...slog.Attr) []slog.Attr {
	return append([]slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("client_addr", client),
	}, extra...)
}

// TransformAttr returns a with an error value replaced by its Error() string,
// recursing into groups, so call sites can pass a raw error and still log a
// printable string. Any other value is returned unchanged.
func TransformAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
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
