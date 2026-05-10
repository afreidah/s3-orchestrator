// -------------------------------------------------------------------------------
// Postgres Store Constructor Tests
//
// Author: Alex Freidah
//
// Verifies that NewStore surfaces actionable connection diagnostics
// when the database is unreachable. The constructor is the first
// surface a misconfigured deployment hits, so its error must name the
// host:port the orchestrator tried to reach and list the common causes
// an operator can check without reading source.
// -------------------------------------------------------------------------------

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/config"
)

// -------------------------------------------------------------------------
// CONNECTION ERROR DIAGNOSTICS
// -------------------------------------------------------------------------

// TestNewStore_PingFailureSurfacesHostAndRemediation verifies that a
// connect failure produces an error containing host=HOST:PORT,
// db=NAME, and the four-cause remediation hint. The host points at
// TEST-NET-2 (RFC 5737) so the connect attempt is guaranteed to fail
// without depending on local network state.
func TestNewStore_PingFailureSurfacesHostAndRemediation(t *testing.T) {
	t.Parallel()

	cfg := &config.DatabaseConfig{
		Host:     "198.51.100.1",
		Port:     5432,
		Database: "fake_db",
		User:     "fake_user",
		Password: "fake_pw",
		SSLMode:  "disable",
		MaxConns: 1,
		MinConns: 0,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := NewStore(ctx, cfg, nil)
	if err == nil {
		t.Fatal("expected connect error against unroutable host")
	}
	for _, want := range []string{
		"host=198.51.100.1:5432",
		"db=fake_db",
		"verify the host is reachable",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing substring %q\nerror: %v", want, err)
		}
	}
}
