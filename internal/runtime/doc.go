// Package runtime is the daemon composition root. It assembles every
// long-lived subsystem - observability, DI, the HTTP listener, background
// workers, the reload coordinator - and owns the shutdown order so the
// CLI entry point does not.
package runtime
