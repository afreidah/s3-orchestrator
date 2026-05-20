// -------------------------------------------------------------------------------
// Object Manager - LIST
//
// Author: Alex Freidah
//
// ListObjects + the cursor-advance and per-page consume helpers it depends
// on. Folds raw keys into virtual-directory CommonPrefixes when a
// delimiter is set, paginates underneath until maxKeys post-grouping
// items are collected, and caps the per-call pagination so a pathological
// prefix layout cannot drag the database through unbounded scans.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// AdvancePastEmittedCommonPrefix rewrites a continuation cursor so the
// next ListObjects call cannot re-emit a CommonPrefix the current call
// already returned. The seen map is local to a single ListObjects
// invocation, so without this rewrite a cursor that lands inside an
// already-emitted CP (e.g., maxPages cap reached deep in a tenant's keys
// or the page boundary aligned mid-group) would let the next call walk
// the same group and emit its CP a second time.
//
// The rewrite increments the last byte of the CP, producing the smallest
// string lex-greater than every key starting with that CP. The store's
// next-page WHERE object_key > cursor then skips the rest of the group
// cleanly. Returns the input unchanged when the delimiter is unset, the
// cursor does not fall inside an emitted CP, or the last byte is 0xff
// (no representable advance  -  accept potential re-emission rather than
// corrupt the cursor).
func AdvancePastEmittedCommonPrefix(prefix, delimiter, cursor string, seen map[string]bool) string {
	if delimiter == "" || cursor == "" {
		return cursor
	}
	if !strings.HasPrefix(cursor, prefix) {
		return cursor
	}
	rest := cursor[len(prefix):]
	idx := strings.Index(rest, delimiter)
	if idx < 0 {
		return cursor
	}
	cp := cursor[:len(prefix)+idx+len(delimiter)]
	if !seen[cp] {
		return cursor
	}
	last := cp[len(cp)-1]
	if last == 0xff {
		return cursor
	}
	return cp[:len(cp)-1] + string([]byte{last + 1})
}

// ListObjectsV2Result holds the processed result for the S3 ListObjectsV2 response.
type ListObjectsV2Result struct {
	Objects               []core.ObjectLocation `json:"objects,omitempty"`
	CommonPrefixes        []string              `json:"common_prefixes,omitempty"`
	IsTruncated           bool                  `json:"is_truncated,omitempty"`
	NextContinuationToken string                `json:"next_continuation_token,omitempty"`
	KeyCount              int                   `json:"key_count,omitempty"`
}

// ListObjects returns objects matching the given prefix with optional
// delimiter support for virtual directory grouping. When a delimiter is
// set, many raw objects may collapse into a single CommonPrefix, so the
// loop fetches store pages until maxKeys post-grouping items are
// collected or the store is exhausted.
func (o *Manager) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListObjectsV2Result, error) {
	const operation = "ListObjects"
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation,
		attribute.String("s3o.prefix", prefix),
		attribute.String("s3o.delimiter", delimiter),
		attribute.Int("s3o.max_keys", maxKeys),
	)
	defer span.End()

	result := &ListObjectsV2Result{}
	cursor := startAfter
	seen := make(map[string]bool)
	lastStoreTruncated := false

	maxPages := ListObjectsMaxPages
	for page := 0; page < maxPages && result.KeyCount < maxKeys; page++ {
		storeResult, err := o.stores.ListObjects(ctx, prefix, cursor, maxKeys)
		if err != nil {
			return nil, listObjectsError(span, err)
		}
		if len(storeResult.Objects) == 0 {
			break
		}
		lastStoreTruncated = storeResult.IsTruncated

		o.consumeListPage(storeResult.Objects, prefix, delimiter, maxKeys, seen, result)
		if result.IsTruncated || !storeResult.IsTruncated {
			break
		}
		cursor = storeResult.Objects[len(storeResult.Objects)-1].ObjectKey

		if page == maxPages-1 && storeResult.IsTruncated && !result.IsTruncated {
			result.IsTruncated = true
			result.NextContinuationToken = AdvancePastEmittedCommonPrefix(prefix, delimiter, cursor, seen)
			telemetry.ListPagesCappedTotal.Inc()
		}
	}

	if !result.IsTruncated && lastStoreTruncated && result.KeyCount >= maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = AdvancePastEmittedCommonPrefix(prefix, delimiter, cursor, seen)
	}

	o.core.Acct().Operation(operation, "", start, nil)

	pobserve.ListCompleted(ctx, prefix, result.KeyCount, result.IsTruncated)
	span.SetStatus(codes.Ok, "")
	span.SetAttributes(attribute.Int("s3o.key_count", result.KeyCount))
	return result, nil
}

// listObjectsError translates a store-side ListObjects error into the
// error returned to the caller. ErrDBUnavailable becomes a 503; anything
// else is wrapped with context.
func listObjectsError(span trace.Span, err error) error {
	if errors.Is(err, core.ErrDBUnavailable) {
		observe.MarkSpanError(span, "database unavailable")
		return &core.S3Error{StatusCode: 503, Code: "ServiceUnavailable", Message: "listing unavailable during database outage"}
	}
	observe.RecordSpanError(span, err)
	return fmt.Errorf("failed to list objects: %w", err)
}

// consumeListPage walks one store page, folding raw objects into
// CommonPrefixes when delimiter is set and appending plain objects
// otherwise. Mutates result and seen, and sets result.IsTruncated when
// maxKeys is hit mid-page.
func (o *Manager) consumeListPage(
	objects []core.ObjectLocation,
	prefix, delimiter string,
	maxKeys int,
	seen map[string]bool,
	result *ListObjectsV2Result,
) {
	var lastKey string
	for oi := range objects {
		key := objects[oi].ObjectKey
		if delimiter != "" {
			handled, truncated := tryEmitCommonPrefix(key, prefix, delimiter, maxKeys, seen, result, lastKey)
			if handled {
				lastKey = key
				if truncated {
					return
				}
				continue
			}
		}
		if result.KeyCount >= maxKeys {
			result.IsTruncated = true
			result.NextContinuationToken = lastKey
			return
		}
		result.Objects = append(result.Objects, objects[oi])
		result.KeyCount++
		lastKey = key
	}
}

// tryEmitCommonPrefix folds key into a CommonPrefix when one applies.
// handled=false signals the key should fall through to plain-object
// handling. truncated=true signals the caller to stop iterating because
// maxKeys was hit while emitting a new prefix.
func tryEmitCommonPrefix(
	key, prefix, delimiter string,
	maxKeys int,
	seen map[string]bool,
	result *ListObjectsV2Result,
	lastKey string,
) (bool, bool) {
	rest := key[len(prefix):]
	idx := strings.Index(rest, delimiter)
	if idx < 0 {
		return false, false
	}
	cp := key[:len(prefix)+idx+len(delimiter)]
	if seen[cp] {
		return true, false
	}
	if result.KeyCount >= maxKeys {
		result.IsTruncated = true
		result.NextContinuationToken = lastKey
		return true, true
	}
	seen[cp] = true
	result.CommonPrefixes = append(result.CommonPrefixes, cp)
	result.KeyCount++
	return true, false
}
