// Package backendstate owns the operator side of a backend's state record:
// starting, cancelling, and reporting a drain, removing a backend, and exposing
// AcceptsWrites and IsDraining for the proxy core's eligibility filters. Moving
// a draining backend's objects is the drainer worker's.
package backendstate
