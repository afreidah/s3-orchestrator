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
	"io"
	"log/slog"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// SiblingLocator reads the rows the ledger already holds for a key, which is
// where the encryption key for a rediscovered object has to come from.
type SiblingLocator interface {
	GetAllObjectLocations(ctx context.Context, key string) ([]core.ObjectLocation, error)
}

// StoredInspector recognises stored bytes as something this orchestrator
// encoded, and reports the logical size they decode to.
type StoredInspector interface {
	InspectStored(ctx context.Context, f compression.RangeFetcher, storedSize int64) (int64, bool)
}

// ClassifyDeps is what ClassifyImport needs to reach the bytes and the ledger.
// Source labels the metric and audit trail with which pass did the import.
// Codec is optional; without one a compressed object is imported as verbatim.
type ClassifyDeps struct {
	Backend backend.ObjectBackend
	Stores  SiblingLocator
	Codec   StoredInspector
	Source  string
	Log     *slog.Logger
}

// backendRange fetches byte ranges of one object, adapting a backend to the
// codec's RangeFetcher so the seek table can be read without pulling the object.
type backendRange struct {
	be  backend.ObjectBackend
	key string
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FetchRange implements compression.RangeFetcher.
func (b backendRange) FetchRange(ctx context.Context, start, end int64) ([]byte, error) {
	r, err := b.be.GetObject(ctx, b.key, fmt.Sprintf("bytes=%d-%d", start, end))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

// ClassifyImport determines the stored form a discovered object should be
// imported with, by reading its envelope header off the backend and testing
// that header against the rows the ledger already holds for the key.
//
// Import is the only write path that starts from bytes instead of from a
// client request, so skipping this is what records an encrypted object as
// plaintext and leaves the read path serving raw ciphertext to clients.
//
// A nil return means the bytes are stored verbatim.
func ClassifyImport(ctx context.Context, deps ClassifyDeps, backendName, key string, size int64) (*core.StoredForm, error) {
	header, err := backend.FetchEnvelopeHeader(ctx, deps.Backend, key)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect %s: %w", key, err)
	}
	discovered := core.DiscoveredBytes{Header: header}

	// Only an envelope needs a key to go with it, so only then is the
	// per-key ledger lookup worth paying for. Bytes that are not an envelope
	// get the cheaper question asked of them instead: are they an encoding
	// this orchestrator wrote?
	if !encryption.HasEnvelopeMagic(header) {
		discovered.LogicalSize, discovered.Compressed = deps.inspectStored(ctx, header, key, size)
		decision, form := core.ClassifyImport(discovered, nil)
		deps.record(ctx, decision, backendName, key)
		return form, nil
	}

	siblings, err := deps.Stores.GetAllObjectLocations(ctx, key)
	if err != nil && !errors.Is(err, core.ErrObjectNotFound) {
		return nil, fmt.Errorf("failed to look up existing copies of %s: %w", key, err)
	}

	decision, form := core.ClassifyImport(discovered, siblings)
	deps.record(ctx, decision, backendName, key)
	return form, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// inspectStored asks the codec whether these bytes are one of its own. A failure
// to read the tail is not an error: it means the object is not recognised, and
// it is then imported as the verbatim bytes it appears to be.
//
// The frame magic is checked first, off the head already in hand, so the tail
// fetch is only spent on objects that could plausibly be an encoding. Without
// that filter every plaintext object on a backend would cost a second ranged
// GET during the walk.
func (d ClassifyDeps) inspectStored(ctx context.Context, header []byte, key string, size int64) (int64, bool) {
	if d.Codec == nil || size <= 0 || !compression.HasFrameMagic(header) {
		return 0, false
	}
	return d.Codec.InspectStored(ctx, backendRange{be: d.Backend, key: key}, size)
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
