// -------------------------------------------------------------------------------
// Object Manager - LIST
//
// Author: Alex Freidah
//
// ListObjects routes to the store's delimiter-grouped query when a delimiter is
// set and to the flat prefix listing otherwise. The store owns CommonPrefix
// folding, group skip-seeking, and pagination tokens (a loose index scan), so
// this layer only maps one returned page into the S3 response shape and records
// the operation's telemetry and span.
// -------------------------------------------------------------------------------

package object

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	pobserve "github.com/afreidah/s3-orchestrator/internal/proxy/observe"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ListObjectsV2Result holds the processed result for the S3 ListObjectsV2 response.
type ListObjectsV2Result struct {
	Objects               []core.ObjectLocation `json:"objects,omitempty"`
	CommonPrefixes        []string              `json:"common_prefixes,omitempty"`
	IsTruncated           bool                  `json:"is_truncated,omitempty"`
	NextContinuationToken string                `json:"next_continuation_token,omitempty"`
	KeyCount              int                   `json:"key_count,omitempty"`
}

// ListObjects returns one page of objects under the given prefix, folding keys
// into virtual-directory CommonPrefixes when a delimiter is set.
func (o *Manager) ListObjects(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListObjectsV2Result, error) {
	const operation = s3op.ListObjects
	start := time.Now()

	ctx, span := telemetry.StartSpan(ctx, managerSpanPrefix+operation.String(),
		attribute.String("s3o.prefix", prefix),
		attribute.String("s3o.delimiter", delimiter),
		attribute.Int("s3o.max_keys", maxKeys),
	)
	defer span.End()

	result, err := o.listPage(ctx, prefix, delimiter, startAfter, maxKeys)
	if err != nil {
		return nil, listObjectsError(span, err)
	}

	o.core.Acct().Operation(operation, "", start, nil)

	pobserve.ListCompleted(ctx, prefix, result.KeyCount, result.IsTruncated)
	span.SetStatus(codes.Ok, "")
	span.SetAttributes(attribute.Int("s3o.key_count", result.KeyCount))
	return result, nil
}

// listPage fetches one page from the store: the delimiter-grouped query when a
// delimiter is set (CommonPrefixes plus interleaved leaf objects), otherwise the
// flat prefix listing. The store does the grouping and skip-seeking; this maps
// its result into the S3 response shape.
func (o *Manager) listPage(ctx context.Context, prefix, delimiter, startAfter string, maxKeys int) (*ListObjectsV2Result, error) {
	if delimiter != "" {
		page, err := o.stores.ListObjectsDelimited(ctx, prefix, delimiter, startAfter, maxKeys)
		if err != nil {
			return nil, err
		}
		return &ListObjectsV2Result{
			Objects:               page.Objects,
			CommonPrefixes:        page.CommonPrefixes,
			IsTruncated:           page.IsTruncated,
			NextContinuationToken: page.NextContinuationToken,
			KeyCount:              len(page.Objects) + len(page.CommonPrefixes),
		}, nil
	}

	page, err := o.stores.ListObjects(ctx, prefix, startAfter, maxKeys)
	if err != nil {
		return nil, err
	}
	return &ListObjectsV2Result{
		Objects:               page.Objects,
		IsTruncated:           page.IsTruncated,
		NextContinuationToken: page.NextContinuationToken,
		KeyCount:              len(page.Objects),
	}, nil
}

// listObjectsError translates a store-side ListObjects error into the
// error returned to the caller. ErrDBUnavailable becomes a 503; anything
// else is wrapped with context.
func listObjectsError(span trace.Span, err error) error {
	if errors.Is(err, core.ErrDBUnavailable) {
		observe.MarkSpanError(span, "database unavailable")
		return &core.S3Error{StatusCode: http.StatusServiceUnavailable, Code: "ServiceUnavailable", Message: "listing unavailable during database outage"}
	}
	observe.RecordSpanError(span, err)
	return fmt.Errorf("failed to list objects: %w", err)
}
