# -----------------------------------------------------------------------------
# S3-ORCHESTRATOR MODULE
# -----------------------------------------------------------------------------
#
# Owns the identities a running s3-orchestrator authorizes requests against:
# each user, the keypair that proves it, and the grants that say what it
# reaches. One map entry onboards one client, so adding a client is a config
# change rather than a deploy.
#
# Buckets stay out. The ones a deployment serves are declared in its config
# file, which the admin API refuses to change, so a resource for them would
# refuse most of the buckets an operator has.
#
# Author: Alex Freidah / Project: s3-orchestrator
# -----------------------------------------------------------------------------

resource "s3orchestrator_user" "this" {
  for_each = var.identities

  name = each.key
}

# --- omitted keypair halves are minted by the orchestrator ---
resource "s3orchestrator_credential" "this" {
  for_each = var.identities

  user_id           = s3orchestrator_user.this[each.key].id
  label             = each.value.label
  access_key_id     = each.value.access_key_id
  secret_access_key = each.value.secret_access_key
}

# -----------------------------------------------------------------------------
# GRANTS
# -----------------------------------------------------------------------------

# --- flatten { identity = [grant, ...] } into "identity/kind/name" => pair ---
locals {
  grants = merge([
    for name, identity in var.identities : {
      for g in identity.grants :
      "${name}/${g.kind}/${g.name == null ? "" : g.name}" => {
        identity    = name
        kind        = g.kind
        name        = g.name
        permissions = g.permissions
      }
    }
  ]...)
}

resource "s3orchestrator_grant" "this" {
  for_each = local.grants

  user_id     = s3orchestrator_user.this[each.value.identity].id
  kind        = each.value.kind
  name        = each.value.name
  permissions = each.value.permissions
}
