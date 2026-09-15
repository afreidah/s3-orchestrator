# -----------------------------------------------------------------------------
# S3-Orchestrator Module Outputs
# -----------------------------------------------------------------------------

output "user_ids" {
  description = "Map of identity name to the generated user id its credentials and grants reference"
  value       = { for k, u in s3orchestrator_user.this : k => u.id }
}

output "access_key_ids" {
  description = "Map of identity name to the access key it authenticates with"
  value       = { for k, c in s3orchestrator_credential.this : k => c.access_key_id }
}

# --- minted secrets appear nowhere else; a supplied one is echoed back ---
output "keypairs" {
  description = "Map of identity name to the keypair it authenticates with, for writing into a secret store"
  value = {
    for k, c in s3orchestrator_credential.this : k => {
      access_key_id     = c.access_key_id
      secret_access_key = c.secret_access_key
    }
  }
  sensitive = true
}
