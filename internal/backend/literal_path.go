// -------------------------------------------------------------------------------
// Backend - Literal Path for Providers That Sign It Unescaped
//
// Author: Alex Freidah
//
// SigV4 escapes every path character outside the unreserved set, so the SDK
// sends and signs !'()* as %21 %27 %28 %29 %2A. Some providers check the
// signature against a path that leaves those five unescaped, the set
// JavaScript's encodeURIComponent leaves alone, and refuse every such request
// with a 403. Every copy path carries a '!', so on such a backend nothing can be
// written, read or deleted.
//
// The first 403 on a path carrying one of them is resent once with the five
// literal: the same path, decoded the same way, signed and sent as the provider
// checks it. A provider that takes that form marks the backend as not
// supporting escaped-path, and it is sent the literal form until restart. The
// object names on the backend are the same either way.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"net/http"
	"strings"

	smithymiddleware "github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// literalSubDelims turns the escapes of !'()* back into the characters. SigV4
// writes uppercase hex, and every '%' in an escaped path starts an escape, so
// no match can begin inside another one.
var literalSubDelims = strings.NewReplacer("%21", "!", "%27", "'", "%28", "(", "%29", ")", "%2A", "*")

// literalPathMiddleware installs literalPath on the backend's requests.
func (b *S3Backend) literalPathMiddleware(stack *smithymiddleware.Stack) error {
	return beforeSigning(stack, "LiteralPath", b.literalPath)
}

// literalPath sends req with !'()* literal on a backend that has learned it
// needs them so, and as escaped otherwise. The first 403 on such a path is
// resent literal from a copy taken before signing, and a provider that takes
// it marks the backend. A body that cannot be rewound is not resent, so a later
// request learns instead.
func (b *S3Backend) literalPath(ctx context.Context, req *smithyhttp.Request, send sendFunc) (smithymiddleware.FinalizeOutput, smithymiddleware.Metadata, error) {
	literal := literalSubDelims.Replace(req.URL.RawPath)
	switch {
	case literal == req.URL.RawPath:
		return send(req)
	case b.lacks(core.FeatureEscapedPath):
		req.URL.RawPath = literal
		return send(req)
	case b.literalTried.Load():
		return send(req)
	}
	retry := req.Clone()
	out, md, err := send(req)
	if statusOf(err) != http.StatusForbidden || retry.RewindStream() != nil || !b.literalTried.CompareAndSwap(false, true) {
		return out, md, err
	}
	retry.URL.RawPath = literal
	retryOut, retryMD, retryErr := send(retry)
	if statusOf(retryErr) == http.StatusForbidden {
		return out, md, err
	}
	b.markUnsupported(ctx, core.FeatureEscapedPath, err)
	return retryOut, retryMD, retryErr
}
