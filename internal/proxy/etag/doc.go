// Package etag computes S3 entity tags for objects this orchestrator wrote.
//
// S3 defines the ETag of a single-part upload as the MD5 of the object's bytes
// and the ETag of a multipart upload as the MD5 of the concatenated binary part
// digests suffixed with the part count. Clients compare both against locally
// computed digests, so the algorithm is the contract rather than an
// implementation choice.
//
// The digests are always of the bytes the client sent. What lands on a backend
// may be compressed, encrypted or both, and the backend's own ETag describes
// that stored form instead.
package etag
