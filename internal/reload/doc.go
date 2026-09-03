// Package reload owns SIGHUP-driven configuration reload. The
// coordinator runs a two-phase Check / Apply pass over a sequence of
// hooks, swaps the atomic config on success, and reports full /
// partial / validation / load outcomes via Result.
package reload
