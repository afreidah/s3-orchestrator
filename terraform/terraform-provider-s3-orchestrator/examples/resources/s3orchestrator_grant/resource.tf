# A grant naming no kind is over a bucket, which is the common case: onboarding
# a client usually means naming the bucket it reaches and what it may do there.
resource "s3orchestrator_grant" "photos" {
  user_id     = s3orchestrator_user.backup.id
  name        = "photos"
  permissions = ["list", "read", "write"]
}

# A wildcard reaches every bucket including ones added later. A named grant
# replaces it for the bucket it names rather than adding to it, so the two
# together are how broad access gets carved down on one bucket.
resource "s3orchestrator_grant" "everything" {
  user_id     = s3orchestrator_user.backup.id
  name        = "*"
  permissions = ["list", "read"]
}

# The orchestrator is the deployment itself, and there is only one of it, so
# this kind takes no name. Its permissions are a separate vocabulary from a
# bucket's, and the two do not mix.
resource "s3orchestrator_grant" "operator" {
  user_id     = s3orchestrator_user.backup.id
  kind        = "orchestrator"
  permissions = ["admin-read", "admin-logs"]
}
