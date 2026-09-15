# -----------------------------------------------------------------------------
# S3-Orchestrator Module Variables
# -----------------------------------------------------------------------------

variable "identities" {
  description = "Map of identity name to its keypair and the resources it reaches; supply both keypair halves to register one held elsewhere, or neither to have it minted"

  type = map(object({
    label             = optional(string)
    access_key_id     = optional(string)
    secret_access_key = optional(string)
    grants = list(object({
      kind        = optional(string, "bucket")
      name        = optional(string)
      permissions = list(string)
    }))
  }))

  # Not marked sensitive: the map keys become resource instance keys, which
  # Terraform refuses to derive from a sensitive value. The secret inside is
  # redacted anyway, by the provider schema that declares it sensitive.
  default = {}

  validation {
    condition = alltrue([
      for i in var.identities : (i.access_key_id == null) == (i.secret_access_key == null)
    ])
    error_message = "Set access_key_id and secret_access_key together, or neither to mint a keypair."
  }

  validation {
    condition = alltrue(flatten([
      for i in var.identities : [for g in i.grants : length(g.permissions) > 0]
    ]))
    error_message = "A grant carrying no permissions reaches its resource and is refused every operation."
  }

  validation {
    condition = alltrue(flatten([
      for i in var.identities : [
        for g in i.grants : contains(["bucket", "backend", "orchestrator"], g.kind)
      ]
    ]))
    error_message = "Grant kind must be bucket, backend, or orchestrator."
  }

  validation {
    condition = alltrue(flatten([
      for i in var.identities : [
        for g in i.grants : g.kind == "orchestrator" || g.name != null
      ]
    ]))
    error_message = "A bucket or backend grant names the resource it is over; only an orchestrator grant omits it."
  }
}
