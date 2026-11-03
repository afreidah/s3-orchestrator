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
	"github.com/afreidah/s3-orchestrator/internal/compression"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
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
// Reader reads the stored bytes and charges whatever meters its caller holds.
// Source labels the metric and audit trail with which pass did the import.
// Codec is optional; without one a compressed object is imported as verbatim.
type ClassifyDeps struct {
	Reader backend.RangeReader
	Stores SiblingLocator
	Codec  StoredInspector
	Source string
	Log    *slog.Logger
}

// keyRange fetches byte ranges of one object, adapting a reader to the codec's
// RangeFetcher so the seek table can be read without pulling the object.
type keyRange struct {
	r   backend.RangeReader
	key string
}

// meteredReader reads one backend's objects directly and charges each read to
// usage.
type meteredReader struct {
	be    backend.ObjectBackend
	name  string
	usage UsageRecorder
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// FetchRange implements compression.RangeFetcher.
func (k keyRange) FetchRange(ctx context.Context, start, end int64) ([]byte, error) {
	return k.r.ReadRange(ctx, k.key, start, end)
}

// ReadRange implements backend.RangeReader. A failed read is charged one call
// and a successful one the bytes it returned.
func (m meteredReader) ReadRange(ctx context.Context, key string, start, end int64) ([]byte, error) {
	data, err := backend.DirectReader{Backend: m.be}.ReadRange(ctx, key, start, end)
	if err != nil {
		m.usage.APICall(ctx, s3op.GetObject, m.name)
		return nil, err
	}
	m.usage.Egress(ctx, s3op.GetObject, m.name, int64(len(data)))
	return data, nil
}

// ClassifyImport determines the stored form a discovered object is imported
// with, by reading its stored bytes and matching any envelope against the rows
// the ledger already holds for the key. Without it, an encrypted object would
// be recorded as plaintext and served to clients as raw ciphertext. A nil
// return means the bytes are stored verbatim.
func ClassifyImport(ctx context.Context, deps ClassifyDeps, backendName, key string, size int64) (*core.StoredForm, error) {
	discovered, err := DiscoverBytes(ctx, deps.Reader, deps.Codec, key, size)
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
// It reads the head for an encryption envelope and, only when the head starts
// like a zstd frame, the tail for this codec's seek table. A client's plain .zst
// file has no seek table and is reported uncompressed, as is any object whose
// tail cannot be read or when codec is nil.
func DiscoverBytes(ctx context.Context, r backend.RangeReader, codec StoredInspector, path string, size int64) (core.DiscoveredBytes, error) {
	header, err := backend.ReadEnvelopeHeader(ctx, r, path)
	if err != nil {
		return core.DiscoveredBytes{}, fmt.Errorf("failed to inspect %s: %w", path, err)
	}
	found := core.DiscoveredBytes{Header: header}
	if codec == nil || size <= 0 || encryption.HasEnvelopeMagic(header) || !compression.HasFrameMagic(header) {
		return found, nil
	}
	found.LogicalSize, found.Compressed = codec.InspectStored(ctx, keyRange{r: r, key: path}, size)
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
