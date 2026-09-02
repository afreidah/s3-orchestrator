// Package observe centralizes per-operation completion observability  -
// audit log, notification event, and span status  -  so storage paths in
// object/, multipart/, and writepath/ can mark success with one call and
// the audit/event attribute shapes stay defined in exactly one place.
package observe
