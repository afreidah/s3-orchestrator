// -------------------------------------------------------------------------------
// Database Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"net/url"
	"time"
)

// DatabaseConfig holds metadata store connection settings. The Driver field
// selects between "sqlite" (embedded, zero-dependency default) and "postgres"
// (required for multi-instance deployments).
type DatabaseConfig struct {
	Driver          string        `yaml:"driver"`            // "sqlite" or "postgres" (default: inferred from config)
	Path            string        `yaml:"path"`              // SQLite file path (default: "s3-orchestrator.db")
	Host            string        `yaml:"host"`
	Port            int           `yaml:"port"`
	Database        string        `yaml:"database"`
	User            string        `yaml:"user"`
	Password        string        `yaml:"password"` //nolint:gosec // G117: config struct field, not a hardcoded credential
	SSLMode         string        `yaml:"ssl_mode"`
	MaxConns        int32         `yaml:"max_conns"`         // Max pool connections (default: 50; size to 2-3x max concurrent requests)
	MinConns        int32         `yaml:"min_conns"`         // Min idle connections (default: 5)
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"` // Max connection age (default: 5m)
}

// ConnectionString returns a PostgreSQL connection URI with properly escaped
// credentials, safe for passwords containing special characters.
func (c *DatabaseConfig) ConnectionString() string {
	u := &url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     fmt.Sprintf("%s:%d", c.Host, c.Port),
		Path:     c.Database,
		RawQuery: fmt.Sprintf("sslmode=%s", url.QueryEscape(c.SSLMode)),
	}
	return u.String()
}

func (d *DatabaseConfig) setDefaultsAndValidate() []string {
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
		return []string{fmt.Sprintf("database.driver must be 'sqlite' or 'postgres', got %q", d.Driver)}
	}
}

func (d *DatabaseConfig) validateSQLite() []string {
	if d.Path == "" {
		d.Path = "s3-orchestrator.db"
	}
	return nil
}

func (d *DatabaseConfig) validatePostgres() []string {
	var errs []string

	if d.Host == "" {
		errs = append(errs, "database.host is required")
	}
	if d.Database == "" {
		errs = append(errs, "database.database is required")
	}
	if d.User == "" {
		errs = append(errs, "database.user is required")
	}
	if d.Port == 0 {
		d.Port = 5432
	}
	if d.SSLMode == "" {
		d.SSLMode = "require"
	}
	if d.MaxConns == 0 {
		d.MaxConns = 50
	}
	if d.MinConns == 0 {
		d.MinConns = 10
	}
	if d.MaxConnLifetime == 0 {
		d.MaxConnLifetime = 5 * time.Minute
	}

	if d.MinConns > d.MaxConns {
		errs = append(errs, fmt.Sprintf("database.min_conns (%d) cannot exceed max_conns (%d)", d.MinConns, d.MaxConns))
	}

	return errs
}
