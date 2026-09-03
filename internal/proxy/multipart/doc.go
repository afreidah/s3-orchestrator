// Package multipart owns the S3 multipart-upload lifecycle: creation,
// per-part uploads, completion, abort and cleanup.
//
// It also holds the encryption helpers parts and the assembled object share,
// the part and upload-row helpers used across those paths, and the advisory
// lock ID derivation that serializes CompleteMultipartUpload for one key.
package multipart
