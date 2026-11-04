// -------------------------------------------------------------------------------
// Admin API - Parameter Names
//
// Author: Alex Freidah
//
// The path and query parameter names the admin API reads. The server's route
// table and handlers, the dashboard's actions, the admin CLI and the TUI all
// name a parameter through these, so a client cannot send one under a name the
// server no longer reads.
// -------------------------------------------------------------------------------

package adminapi

// Parameter names, in the path or the query string as each route declares.
const (
	ParamName         = "name"
	ParamID           = "id"
	ParamKind         = "kind"
	ParamBackend      = "backend"
	ParamBatchSize    = "batch_size"
	ParamMax          = "max"
	ParamKey          = "key"
	ParamPrefix       = "prefix"
	ParamDelimiter    = "delimiter"
	ParamContinuation = "continuation"
	ParamLimit        = "limit"
	ParamLevel        = "level"
	ParamPurge        = "purge"
	ParamConfirm      = "confirm"
	ParamDelayMS      = "delay_ms"
)
