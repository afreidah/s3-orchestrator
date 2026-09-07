// -------------------------------------------------------------------------------
// Provisioning - Assembly Notices
//
// Author: Alex Freidah
//
// What the merge found that an operator should see and that does not stop the
// instance serving. Every one of these describes a state a running fleet can
// reach without anyone doing anything wrong, so they are reported rather than
// treated as a failure to start.
// -------------------------------------------------------------------------------

package provisioning

// NoticeBucketShadowed, NoticeCredentialShadowed and NoticeDanglingGrant are the
// kinds of thing the merge reports without refusing to serve.
const (
	NoticeBucketShadowed     = "bucket_shadowed"
	NoticeCredentialShadowed = "credential_shadowed" //nolint:gosec // G101: a notice kind, not a credential
	NoticeDanglingGrant      = "dangling_grant"
)

// Notice is one such finding, named by Kind and described in Detail.
type Notice struct {
	Kind   string
	Detail string
}
