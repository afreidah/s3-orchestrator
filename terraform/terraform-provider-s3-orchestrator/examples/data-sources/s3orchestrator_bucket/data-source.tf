# A grant has to name a bucket that exists, and one the configuration file
# declares is not managed by this provider. The data source reads it from
# either source and fails the plan when it does not exist, rather than the
# apply. Its source attribute reports config or store.
data "s3orchestrator_bucket" "photos" {
  name = "photos"
}

resource "s3orchestrator_grant" "backup" {
  user_id     = s3orchestrator_user.backup.id
  name        = data.s3orchestrator_bucket.photos.name
  permissions = ["list", "read"]
}
