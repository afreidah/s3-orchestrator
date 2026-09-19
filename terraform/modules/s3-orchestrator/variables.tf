# -----------------------------------------------------------------------------
# S3-Orchestrator Module Variables
# -----------------------------------------------------------------------------

variable "buckets" {
  description = "Map of bucket name to what it carries. A name the config file also declares is allowed: the row lands dormant behind it and takes over when that entry is removed"

  type = map(object({
    max_multipart_uploads = optional(number)
    cors = optional(list(object({
      allowed_origins = list(string)
      allowed_methods = list(string)
      allowed_headers = optional(list(string))
      expose_headers  = optional(list(string))
      max_age         = optional(number)
    })), [])
  }))

  default = {}

  validation {
    condition     = alltrue([for name, _ in var.buckets : can(regex("^[a-z0-9][a-z0-9.-]*$", name))])
    error_message = "Bucket names are lowercase alphanumeric with . - and cannot start with a separator."
  }

  validation {
    condition     = alltrue([for b in var.buckets : b.max_multipart_uploads == null || b.max_multipart_uploads >= 0])
    error_message = "A negative multipart limit is not a limit; omit it or use 0 for unlimited."
  }

  # --- the deployment refuses a rule that cannot match anything, so catching
  #     it here names the bucket rather than failing mid-apply ---
  validation {
    condition = alltrue(flatten([
      for b in var.buckets : [for r in b.cors : length(r.allowed_origins) > 0 && length(r.allowed_methods) > 0]
    ]))
    error_message = "A CORS rule names at least one origin and one method."
  }

  validation {
    condition = alltrue(flatten([
      for b in var.buckets : [
        for r in b.cors : [for o in r.allowed_origins : length(regexall("\\*", o)) <= 1]
      ]
    ]))
    error_message = "A CORS origin carries at most one wildcard."
  }
}

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
