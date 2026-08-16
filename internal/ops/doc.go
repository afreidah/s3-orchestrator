// -------------------------------------------------------------------------------
// Ops - Package Documentation
//
// Author: Alex Freidah
//
// The operations layer: what the orchestrator can be told to do, expressed
// once, independently of who asked. Both in-process transports call these
// types, so an operation behaves the same whether it arrived over the admin
// API or the dashboard, and adminctl and the TUI inherit that through the
// admin API they already speak.
// -------------------------------------------------------------------------------

// Package ops holds the orchestrator's operations layer: object reads and
// writes, integrity passes, replication and rebalance cycles, and the
// fleet-wide encryption transitions.
//
// Each operation is a method on a small service type (Objects, Integrity,
// Replication, Rebalance, Encryption) built from narrow consumer-defined
// interfaces, so a caller supplies behaviour rather than concrete workers and
// stores. Results are domain values; nothing here knows about HTTP, JSON, or
// status codes.
//
// Operations report three kinds of outcome. A completed run returns its
// counts. A run that declined - because the subsystem is disabled, or a worker
// planned no work - returns a *SkipError carrying the reason, which callers
// match with errors.As and word for their own audience. Anything else is a
// real failure and returns an ordinary error.
//
// Long-running operations accept a progress.Observer, nil when the caller
// wants no reporting, so the same method serves a blocking call and a
// streaming one.
package ops
