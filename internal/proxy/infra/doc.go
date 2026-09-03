// Package infra exposes the proxy-package backend infrastructure (backend
// map, usage tracker, drain checker, metrics, admission, per-op timeouts)
// as an importable type so subpackages can share it without an import
// cycle back to the root proxy package.
package infra
