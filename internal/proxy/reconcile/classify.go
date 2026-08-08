// -------------------------------------------------------------------------------
// Import Classification
//
// Author: Alex Freidah
//
// Works out what encryption metadata a discovered backend object should be
// recorded with. Shared by the reconcile passes and the sync subcommand, which
// are the two ways objects enter the ledger from bytes rather than from a
// client request.
// -------------------------------------------------------------------------------

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// SiblingLocator reads the rows the ledger already holds for a key, which is
// where the encryption key for a rediscovered object has to come from.
type SiblingLocator interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
}

// ClassifyDeps is what ClassifyImport needs to reach the bytes and the ledger.
// Source labels the metric and audit trail with which pass did the import.
type ClassifyDeps struct {
	Backend backend.ObjectBackend
	Stores  SiblingLocator
	Source  string
	Log     *slog.Logger
}

// ClassifyImport determines the encryption metadata a discovered object should
// be imported with, by reading its envelope header off the backend and testing
// that header against the rows the ledger already holds for the key.
//
// Import is the only write path that starts from bytes instead of from a
// client request, so skipping this is what records an encrypted object as
// plaintext and leaves the read path serving raw ciphertext to clients.
//
// A nil return means import the object with no encryption metadata.
func ClassifyImport(ctx context.Context, deps ClassifyDeps, backendName, key string) (*core.EncryptionMeta, error) {
	header, err := backend.FetchEnvelopeHeader(ctx, deps.Backend, key)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect %s: %w", key, err)
	}
	// Only an envelope needs a key to go with it, so only then is the
	// per-key ledger lookup worth paying for.
	if !encryption.HasEnvelopeMagic(header) {
		deps.record(ctx, core.ImportPlaintext, backendName, key)
		return nil, nil
	}

	siblings, err := deps.Stores.GetAllObjectLocations(ctx, key)
	if err != nil && !errors.Is(err, core.ErrObjectNotFound) {
		return nil, fmt.Errorf("failed to look up existing copies of %s: %w", key, err)
	}

	decision, enc := core.ClassifyImport(header, siblings)
	deps.record(ctx, decision, backendName, key)
	return enc, nil
}

// record emits the metric and audit trail for one decision, and warns on the
// one outcome an operator has to act on.
func (d ClassifyDeps) record(ctx context.Context, decision core.ImportDecision, backendName, key string) {
	telemetry.ImportClassifiedTotal.WithLabelValues(d.Source, decision.String()).Inc()
	audit.Log(ctx, "import.classified",
		slog.String("key", key),
		slog.String("backend", backendName),
		slog.String("decision", decision.String()),
	)
	if decision == core.ImportUnreadable && d.Log != nil {
		d.Log.WarnContext(ctx, "importing encrypted object with no usable key",
			"key", key, "backend", backendName)
	}
}
