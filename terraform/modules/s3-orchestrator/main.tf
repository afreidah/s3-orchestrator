# -----------------------------------------------------------------------------
# S3-ORCHESTRATOR MODULE
# -----------------------------------------------------------------------------
#
# Owns what a running s3-orchestrator serves: the buckets, and the identities it
# authorizes requests against - each user, the keypair that proves it, and the
# grants that say what it reaches. One map entry onboards one client, so adding
# a client is a config change rather than a deploy.
#
# A bucket the config file also declares is served from there, and the row
# created here waits behind it until that entry is removed. That is what lets a
# bucket move into the store without a moment where neither source declares it.
#
# Author: Alex Freidah / Project: s3-orchestrator
# -----------------------------------------------------------------------------

resource "s3orchestrator_bucket" "this" {
  for_each = var.buckets

  name                  = each.key
  max_multipart_uploads = each.value.max_multipart_uploads

  dynamic "cors_rule" {
    for_each = each.value.cors

    content {
      allowed_origins = cors_rule.value.allowed_origins
      allowed_methods = cors_rule.value.allowed_methods
      allowed_headers = cors_rule.value.allowed_headers
      expose_headers  = cors_rule.value.expose_headers
      max_age         = cors_rule.value.max_age
    }
  }
}

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

  # A grant naming a bucket nothing declares is refused, and the name here is a
  # literal rather than a reference, so nothing else orders these two.
  depends_on = [s3orchestrator_bucket.this]
}
