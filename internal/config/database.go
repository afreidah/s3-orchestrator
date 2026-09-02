// -------------------------------------------------------------------------------
// Database Configuration
//
// Author: Alex Freidah
//
// Defines DatabaseConfig - the metadata-store connection block. Selects
// between the postgres engine (production) and the embedded sqlite engine
// (development, single-instance demos), carries the pgx pool tunables
// (max/min conns, lifetimes, SSL mode), and validates that the chosen
// engine has the fields it needs. The validator surfaces every per-field
// problem in one pass so operators can fix multiple typos at once.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// DatabaseConfig holds metadata store connection settings. The Driver field
// selects between "sqlite" (embedded, zero-dependency default) and "postgres"
// (required for multi-instance deployments).
type DatabaseConfig struct {
	Driver          string        `yaml:"driver"` // "sqlite" or "postgres" (default: inferred from config)
	Path            string        `yaml:"path"`   // SQLite file path (default: "s3-orchestrator.db")
	Host            string        `yaml:"host"`
	Port            int           `yaml:"port"`
	Database        string        `yaml:"database"`
	User            string        `yaml:"user"`
	Password        string        `yaml:"password"` //nolint:gosec // G117: config struct field, not a hardcoded credential
	SSLMode         string        `yaml:"ssl_mode"`
	MaxConns        int32         `yaml:"max_conns"`         // Max pool connections (default: 50; size to 2-3x max concurrent requests)
	MinConns        int32         `yaml:"min_conns"`         // Min idle connections (default: 10)
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"` // Max connection age (default: 5m)
}

// ConnectionString returns a PostgreSQL connection URI with properly escaped
// credentials, safe for passwords containing special characters.
func (c *DatabaseConfig) ConnectionString() string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:     c.Database,
		RawQuery: fmt.Sprintf("sslmode=%s", url.QueryEscape(c.SSLMode)),
	}
	return u.String()
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// setDefaultsAndValidate sets defaults and validate.
func (d *DatabaseConfig) setDefaultsAndValidate() []error {
	// Infer driver from config if not set explicitly.
	if d.Driver == "" {
		if d.Host != "" {
			d.Driver = "postgres"
		} else {
			d.Driver = "sqlite"
		}
	}

	switch d.Driver {
	case "sqlite":
		return d.validateSQLite()
	case "postgres":
		return d.validatePostgres()
	default:
		return []error{fmt.Errorf("database.driver must be 'sqlite' or 'postgres', got %q", d.Driver)}
	}
}

// validateSQLite fills the SQLite engine's default file path when
// unset and returns no errors otherwise. Single-instance SQLite
// deployments only need a writable path; everything else has sane
// defaults.
func (d *DatabaseConfig) validateSQLite() []error {
	d.Path = cmp.Or(d.Path, "s3-orchestrator.db")
	return nil
}

// validatePostgres enforces the required Postgres fields (host,
// database, user) and validates the pgx pool tunables. Fans every
// missing-required error into the returned slice so operators can fix
// them all in one config-edit cycle.
func (d *DatabaseConfig) validatePostgres() []error {
	var errs []error

	if d.Host == "" {
		errs = append(errs, fmt.Errorf("database.host is required"))
	}
	if d.Database == "" {
		errs = append(errs, fmt.Errorf("database.database is required"))
	}
	if d.User == "" {
		errs = append(errs, fmt.Errorf("database.user is required"))
	}
	d.Port = cmp.Or(d.Port, 5432)
	d.SSLMode = cmp.Or(d.SSLMode, "require")
	d.MaxConns = cmp.Or(d.MaxConns, 50)
	d.MinConns = cmp.Or(d.MinConns, 10)
	d.MaxConnLifetime = cmp.Or(d.MaxConnLifetime, 5*time.Minute)

	if d.MinConns > d.MaxConns {
		errs = append(errs, fmt.Errorf("database.min_conns (%d) cannot exceed max_conns (%d)", d.MinConns, d.MaxConns))
	}

	return errs
}
