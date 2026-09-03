// Package writepath owns the helpers that combine the per-role store views
// with the backend runtime primitives to record objects, promote pending
// intents, enqueue cleanups, move copies between backends and pick write
// targets.
//
// The object and multipart managers hold a *Coordinator directly, so each is
// fully initialised at construction time without post-construction patching.
package writepath
