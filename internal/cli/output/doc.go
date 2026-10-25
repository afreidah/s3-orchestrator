// Package output renders admin API responses for a terminal: an indented,
// YAML-like block for decoded JSON, plus duration, table, error and backend
// formatting shared by the CLI commands and the TUI.
//
// Scalars print without JSON quoting or float exponent noise, and a body that
// does not parse as JSON is written through unchanged so an unexpected response
// still reaches the operator.
package output
