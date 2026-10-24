// Package postgres implements every core store role plus the admin roles
// against PostgreSQL through sqlc-generated queries. Goose migrations bring the
// schema up at start, advisory locks keep background passes to one instance,
// and every statement passes through the database circuit breaker at the
// driver chokepoint.
package postgres
