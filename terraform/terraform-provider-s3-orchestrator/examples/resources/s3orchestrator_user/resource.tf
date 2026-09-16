# A user is the identity credentials prove and grants empower. One created on
# its own authenticates and reaches nothing, which is a safe state to leave it
# in while the grants that empower it are still being decided.
#
# One user per process, not per bucket: a job that reads from two buckets is
# still one identity, and giving it two would mean rotating two keypairs.
resource "s3orchestrator_user" "backup" {
  name = "temporal-backup-job"
}
