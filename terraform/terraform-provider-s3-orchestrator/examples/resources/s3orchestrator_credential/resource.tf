# Supplying both halves registers a keypair generated somewhere else, which is
# how this composes with a secret manager: the secret is made where secrets are
# made, and the orchestrator is only told about it.
resource "s3orchestrator_credential" "backup" {
  user_id           = s3orchestrator_user.backup.id
  label             = "vault-managed"
  access_key_id     = vault_kv_secret_v2.backup.data["access_key_id"]
  secret_access_key = vault_kv_secret_v2.backup.data["secret_access_key"]
}

# Supplying neither mints one instead. The minted secret crosses the wire once
# and lives in Terraform state afterwards, so losing state loses the credential.
resource "s3orchestrator_credential" "minted" {
  user_id = s3orchestrator_user.backup.id
  label   = "minted by terraform"
}

# Several credentials may name one user, which is what makes a rotation
# overlap: issue the new keypair, move the client onto it, then remove the old
# resource.
output "minted_keypair" {
  value = {
    access_key_id     = s3orchestrator_credential.minted.access_key_id
    secret_access_key = s3orchestrator_credential.minted.secret_access_key
  }
  sensitive = true
}
