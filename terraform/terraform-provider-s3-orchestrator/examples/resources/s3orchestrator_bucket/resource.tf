# A bucket declares a namespace on the orchestrator. Which backends its objects
# land on stays in the orchestrator's configuration. max_multipart_uploads caps
# the uploads in flight against it at once; zero, the default, is unlimited.
resource "s3orchestrator_bucket" "photos" {
  name                  = "photos"
  max_multipart_uploads = 4
}

# Browser access takes repeated cors_rule blocks. The deployment refuses a rule
# that cannot match anything, and dropping a block removes that rule.
resource "s3orchestrator_bucket" "assets" {
  name = "assets"

  cors_rule {
    allowed_origins = ["https://app.example.com"]
    allowed_methods = ["GET", "HEAD"]
    expose_headers  = ["ETag"]
    max_age         = 600
  }
}

# A client that compresses and encrypts before uploading, such as restic, gains
# nothing from the orchestrator doing either again. stored_form replaces the
# deployment's layers for the bucket's new objects; omit it to keep them.
resource "s3orchestrator_bucket" "restic" {
  name        = "restic"
  stored_form = ["hashed"]
}

# Expendable data is kept at one copy whatever the deployment's factor, which
# halves what it stores and keeps it out of the replicator's work.
resource "s3orchestrator_bucket" "traces" {
  name               = "traces"
  replication_factor = 1
}
