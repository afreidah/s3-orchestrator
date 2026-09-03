// Package cache provides an object data cache layer that sits between the
// orchestrator and storage backends, reducing API calls and egress by serving
// repeated reads from local storage.
package cache
