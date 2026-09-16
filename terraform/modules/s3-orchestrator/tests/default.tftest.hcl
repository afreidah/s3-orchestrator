# -----------------------------------------------------------------------------
# s3-orchestrator module tests (plan-only)
#
# Project: s3-orchestrator / Author: Alex Freidah
#
# Asserts that one map entry onboards one client, that the grant list under
# each identity flattens to a stable "identity/kind/name" key, that a grant
# naming no kind lands on bucket, that an orchestrator grant keys with an
# empty name segment, and that an empty map builds nothing.
# -----------------------------------------------------------------------------

mock_provider "s3orchestrator" {}

variables {
  identities = {
    "temporal-backup-job" = {
      label = "temporal backups"
      grants = [
        { name = "unified", permissions = ["list", "read", "write"] },
        { name = "artifacts", permissions = ["read"] },
      ]
    }
    "aptly" = {
      access_key_id     = "AKIAAPTLYEXAMPLE0000"
      secret_access_key = "aptly-secret"
      grants = [
        { kind = "orchestrator", permissions = ["admin-read"] },
      ]
    }
  }
}

# -------------------------------------------------------------------------
# Identity fan-out: one user and one credential per map entry
# -------------------------------------------------------------------------

run "identities_for_each" {
  command = plan

  # --- two identities input -> two users ---
  assert {
    condition     = length(s3orchestrator_user.this) == 2
    error_message = "two identities input -> two users"
  }

  # --- the map key is the user name, so the name is the handle everywhere ---
  assert {
    condition     = s3orchestrator_user.this["aptly"].name == "aptly"
    error_message = "user name must be the identity map key"
  }

  # --- one credential per identity, keyed the same way ---
  assert {
    condition     = toset(keys(s3orchestrator_credential.this)) == toset(["temporal-backup-job", "aptly"])
    error_message = "one credential per identity, keyed by identity name"
  }

  # --- a supplied keypair is passed through rather than minted ---
  assert {
    condition     = s3orchestrator_credential.this["aptly"].access_key_id == "AKIAAPTLYEXAMPLE0000"
    error_message = "a supplied access key must reach the resource unchanged"
  }

  # --- an omitted label stays null; the orchestrator fills one in ---
  assert {
    condition     = s3orchestrator_credential.this["aptly"].label == null
    error_message = "an omitted label must stay null so the orchestrator picks one"
  }
}

# -------------------------------------------------------------------------
# Grant flattening: nested lists collapse to one keyed map
# -------------------------------------------------------------------------

run "grants_flatten" {
  command = plan

  # --- three grants across two identities -> three resources ---
  assert {
    condition     = length(s3orchestrator_grant.this) == 3
    error_message = "three grants across two identities -> three resources"
  }

  # --- key is identity/kind/name, so two identities may grant the same bucket ---
  assert {
    condition = toset(keys(s3orchestrator_grant.this)) == toset([
      "temporal-backup-job/bucket/unified",
      "temporal-backup-job/bucket/artifacts",
      "aptly/orchestrator/",
    ])
    error_message = "grants must key as identity/kind/name"
  }

  # --- a grant naming no kind is over a bucket ---
  assert {
    condition     = s3orchestrator_grant.this["temporal-backup-job/bucket/unified"].kind == "bucket"
    error_message = "a grant naming no kind must default to bucket"
  }

  # --- an orchestrator grant carries no name; there is only one orchestrator ---
  assert {
    condition     = s3orchestrator_grant.this["aptly/orchestrator/"].name == null
    error_message = "an orchestrator grant must carry no resource name"
  }

  # --- permissions reach the resource as written ---
  assert {
    condition = toset(s3orchestrator_grant.this["temporal-backup-job/bucket/unified"].permissions) == toset([
      "list", "read", "write",
    ])
    error_message = "permissions must reach the resource unchanged"
  }
}

# -------------------------------------------------------------------------
# Outputs are keyed by identity, so a caller looks a client up by name
# -------------------------------------------------------------------------

run "outputs_keyed_by_identity" {
  command = plan

  # --- OUTPUT: one user id per identity ---
  assert {
    condition     = toset(keys(output.user_ids)) == toset(["temporal-backup-job", "aptly"])
    error_message = "output.user_ids must carry one key per identity"
  }

  # --- OUTPUT: one access key per identity ---
  assert {
    condition     = toset(keys(output.access_key_ids)) == toset(["temporal-backup-job", "aptly"])
    error_message = "output.access_key_ids must carry one key per identity"
  }

  # --- OUTPUT: keypairs pair the two halves under the same key ---
  assert {
    condition     = nonsensitive(output.keypairs)["aptly"].access_key_id == "AKIAAPTLYEXAMPLE0000"
    error_message = "output.keypairs must pair both halves under the identity name"
  }
}

# -------------------------------------------------------------------------
# An empty map builds nothing, so the module is safe to always call
# -------------------------------------------------------------------------

run "empty_identities" {
  command = plan

  variables {
    identities = {}
  }

  assert {
    condition = alltrue([
      length(s3orchestrator_user.this) == 0,
      length(s3orchestrator_credential.this) == 0,
      length(s3orchestrator_grant.this) == 0,
    ])
    error_message = "an empty identities map -> zero resources"
  }
}
