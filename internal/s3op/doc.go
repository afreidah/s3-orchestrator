// Package s3op declares the closed set of operations the orchestrator issues
// against a storage backend, as one type rather than a string literal per call
// site.
//
// Providers bill these differently - an upload and a delete are not the same
// charge - so config, usage accounting and the operation metric all have to
// agree on what an operation is called. A leaf package with no app
// dependencies, so config, the counter layer and the proxy can each name an
// operation without importing one another.
package s3op
