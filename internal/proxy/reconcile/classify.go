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
	discovered, err := DiscoverBytes(ctx, deps.Backend, deps.Codec, key, size)
	if err != nil {
		return nil, err
	}

	// Only an envelope needs a key to go with it, so only then is the
	// per-key ledger lookup worth paying for.
	if !encryption.HasEnvelopeMagic(discovered.Header) {
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

// DiscoverBytes reads what an object's stored bytes say about themselves, for
// a caller that has no database row describing them: import, which found the
// object on a backend, and a read while the database is down.
//
// It reads the head of the object at path, where an encryption envelope
// announces itself. When the head is not an envelope but starts like a zstd
// frame, it also reads the tail to check for this codec's seek table, which
// marks the bytes as compressed and gives their logical size. A plain .zst
// file a client uploaded has no seek table, so it is reported as uncompressed.
//
// The frame magic is checked first, so only objects that could be compressed
// cost the second ranged read. A tail that cannot be read is not an error: the
// bytes are then reported as stored verbatim. codec may be nil, in which case
// nothing is reported as compressed.
func DiscoverBytes(ctx context.Context, be backend.ObjectBackend, codec StoredInspector, path string, size int64) (core.DiscoveredBytes, error) {
	header, err := backend.FetchEnvelopeHeader(ctx, be, path)
	if err != nil {
		return core.DiscoveredBytes{}, fmt.Errorf("failed to inspect %s: %w", path, err)
	}
	found := core.DiscoveredBytes{Header: header}
	if codec == nil || size <= 0 || encryption.HasEnvelopeMagic(header) || !compression.HasFrameMagic(header) {
		return found, nil
	}
	found.LogicalSize, found.Compressed = codec.InspectStored(ctx, backendRange{be: be, key: path}, size)
	return found, nil
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
