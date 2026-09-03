// Package sqlite implements every core store role plus the admin
// roles using an embedded SQLite database via modernc.org/sqlite. WAL
// mode handles concurrent reads, a process-local mutex emulates
// advisory locks, and the schema migrates on first start.
package sqlite
