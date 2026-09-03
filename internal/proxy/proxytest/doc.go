// Package proxytest provides cross-package test helpers for the proxy
// subpackages. Importing it from production code is not supported.
//
// The builders here mirror what internal/di assembles, one collaborator at a
// time, so a test constructs only the pieces it exercises. Stack composes them
// for a test that needs the whole read/write path, and exists because three of
// the wiring rules between them are invariants rather than choices: the object
// and multipart managers share one integrity-config pointer, every collaborator
// shares one write coordinator, and the runtime has to be told about the drain
// manager. A test that re-derives those by hand gets no error when it gets them
// wrong - it gets a fixture that silently disagrees with production.
package proxytest
