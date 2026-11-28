# -------------------------------------------------------------------------------
# S3 Orchestrator - Nomad Job
#
# Author: Alex Freidah
#
# Runs the orchestrator as a service job with its config read from a file when
# the job is submitted. Secrets stay out of the file and reach the task as
# environment variables, by default from the Nomad Variable at
# nomad/jobs/s3-orchestrator. Example values are in s3-orchestrator.vars.hcl:
#   nomad job run -var-file=s3-orchestrator.vars.hcl -var=version=v0.191.0 s3-orchestrator.nomad.hcl
# Clients need a max_kill_timeout of at least kill_timeout, or they cut the
# drain short at their 30s default.
# -------------------------------------------------------------------------------

# -------------------------------------------------------------------------------
# Variables
# -------------------------------------------------------------------------------

# --- Job and placement ---
variable "region" {
  description = "Region to run in; empty uses the agent's region"
  type        = string
  default     = ""
}

variable "namespace" {
  description = "Namespace to run in; empty uses the default namespace"
  type        = string
  default     = ""
}

variable "datacenters" {
  description = "Datacenters eligible for placement"
  type        = list(string)
  default     = ["*"]
}

variable "node_pool" {
  description = "Node pool eligible for placement"
  type        = string
  default     = "default"
}

variable "count" {
  description = "Instances serving S3; more than one instance in all requires redis in the config"
  type        = number
  default     = 1

  validation {
    condition     = var.count >= 1
    error_message = "count must be at least 1."
  }
}

variable "worker_count" {
  description = "Instances in a worker group of their own; 0 runs every instance in all mode, above 0 runs the S3 group in api mode"
  type        = number
  default     = 0

  validation {
    condition     = var.worker_count >= 0
    error_message = "worker_count must not be negative."
  }
}

variable "distinct_hosts" {
  description = "Place each instance of a group on a different client node"
  type        = bool
  default     = false
}

variable "constraints" {
  description = "Extra job constraints: [{ attribute, operator, value }]; an empty operator means ="
  type = list(object({
    attribute = string
    operator  = string
    value     = string
  }))
  default = []
}

# --- Image ---
variable "image" {
  description = "Container image repository"
  type        = string
  default     = "ghcr.io/afreidah/s3-orchestrator"
}

variable "version" {
  description = "Container image tag, such as v0.191.0; a released job file defaults to its own release"
  type        = string
  default     = ""

  validation {
    condition     = var.version != "" && var.version != "latest"
    error_message = "version must name a release, such as v0.191.0."
  }
}

# --- Networking and service ---
variable "http_port" {
  description = "Static host port for the S3 listener; 0 picks a dynamic port. The worker group always takes a dynamic one"
  type        = number
  default     = 0
}

variable "tls_mode" {
  description = "What server.tls in the config turns on, which decides how the health checks connect: off, tls (cert_file and key_file) or mtls (client_ca_file as well)"
  type        = string
  default     = "off"

  validation {
    condition     = contains(["off", "tls", "mtls"], var.tls_mode)
    error_message = "tls_mode must be off, tls or mtls."
  }
}

variable "service_provider" {
  description = "Service registration provider: nomad or consul"
  type        = string
  default     = "nomad"

  validation {
    condition     = contains(["nomad", "consul"], var.service_provider)
    error_message = "service_provider must be nomad or consul."
  }
}

variable "service_tags" {
  description = "Tags on the S3 service, such as Traefik router rules; the worker service gets none"
  type        = list(string)
  default     = []
}

variable "register_metrics_service" {
  description = "Register <service>-metrics on port 9001 for Prometheus discovery; the config must set telemetry.metrics.listen to 0.0.0.0:9001"
  type        = bool
  default     = true
}

# --- Resources and shutdown ---
variable "resources" {
  description = "CPU in MHz and memory in MiB; memory_max above memory allows oversubscription, 0 leaves it off"
  type = object({
    cpu        = number
    memory     = number
    memory_max = number
  })
  default = {
    cpu        = 1000
    memory     = 1024
    memory_max = 0
  }
}

variable "ephemeral_disk" {
  description = "Disk in MB reserved at placement for spill files, which land in the task's tmp directory; size for max_concurrent_writes times the largest upload. Nomad does not enforce it"
  type        = number
  default     = 300
}

variable "shutdown_delay" {
  description = "Time between deregistering the services and sending SIGTERM, so clients stop routing first"
  type        = string
  default     = "5s"
}

variable "kill_timeout" {
  description = "SIGTERM to SIGKILL; covers up to 70s of HTTP, upload and background-service drains. A client caps it at its max_kill_timeout, 30s by default"
  type        = string
  default     = "90s"
}

# --- Configuration and secrets ---
variable "config_file" {
  description = "Path to the config.yaml to run, read when the job is submitted and rendered as a Nomad template; reference secrets in it as $${NAME}"
  type        = string
}

variable "config_wait" {
  description = "Quiet period before a changed config re-renders, so service lookups that settle in steps restart the task once; an empty min leaves it off, an empty max uses min"
  type = object({
    min = string
    max = string
  })
  default = {
    min = ""
    max = ""
  }
}

variable "env" {
  description = "Environment variables set on the task as given, such as GOMAXPROCS"
  type        = map(string)
  default     = {}
}

variable "env_template" {
  description = "Nomad template rendered as the task's environment; the default exports every item of the job's Nomad Variable, and empty sets nothing"
  type        = string
  default     = <<EOH
{{- with nomadVar (print "nomad/jobs/" (env "NOMAD_JOB_ID")) }}
{{- range $k, $v := . }}
{{ $k }}={{ $v }}
{{- end }}
{{- end }}
EOH
}

variable "templates" {
  description = "Extra files the config points at, such as a CA bundle or key file, rendered into secrets/<destination>; a change restarts the task"
  type = list(object({
    data        = string
    destination = string
  }))
  default = []
}

variable "cert_templates" {
  description = "The server.tls cert_file and key_file, rendered into secrets/<destination>; a renewal sends SIGHUP, which reloads them without a restart"
  type = list(object({
    data        = string
    destination = string
  }))
  default = []
}

variable "vault_role" {
  description = "Vault role the task logs in with through workload identity, which needs vault.default_identity in the server config; empty leaves Vault out"
  type        = string
  default     = ""
}

# -------------------------------------------------------------------------------
# Locals
# -------------------------------------------------------------------------------

locals {
  # --- One group in all mode, or an api group beside a worker group ---
  groups = concat(
    [{
      name        = "s3-orchestrator"
      mode        = var.worker_count > 0 ? "api" : "all"
      count       = var.count
      static_port = var.http_port
      tags        = var.service_tags
    }],
    var.worker_count > 0 ? [{
      name        = "s3-orchestrator-worker"
      mode        = "worker"
      count       = var.worker_count
      static_port = 0
      tags        = []
    }] : [],
  )

  # --- A canary needs a spare placement, which a static port or distinct_hosts denies ---
  canary = var.http_port > 0 || var.distinct_hosts ? 0 : 1

  # --- mTLS leaves an HTTP check no client certificate, so it falls back to TCP ---
  check_type     = var.tls_mode == "mtls" ? "tcp" : "http"
  check_protocol = var.tls_mode == "tls" ? "https" : ""

  # --- An HTTPS check dials the instance's address, which the certificate need not name ---
  check_skip_verify = var.tls_mode == "tls"
}

# -------------------------------------------------------------------------------
# Job
# -------------------------------------------------------------------------------

job "s3-orchestrator" {
  region      = var.region == "" ? null : var.region
  datacenters = var.datacenters
  node_pool   = var.node_pool
  type        = "service"
  namespace   = var.namespace == "" ? null : var.namespace

  # ---------------------------------------------------------------------------
  # Update Strategy
  # ---------------------------------------------------------------------------

  # --- The deadline leaves room for the 5m startup grace first-boot migrations can use ---
  update {
    max_parallel      = 1
    canary            = local.canary
    auto_promote      = local.canary > 0
    health_check      = "checks"
    min_healthy_time  = "30s"
    healthy_deadline  = "8m"
    progress_deadline = "10m"
    auto_revert       = true
  }

  # ---------------------------------------------------------------------------
  # Constraints
  # ---------------------------------------------------------------------------

  dynamic "constraint" {
    for_each = var.constraints
    content {
      attribute = constraint.value.attribute
      operator  = constraint.value.operator == "" ? "=" : constraint.value.operator
      value     = constraint.value.value
    }
  }

  # ---------------------------------------------------------------------------
  # Task Groups: s3-orchestrator, plus s3-orchestrator-worker with worker_count
  # ---------------------------------------------------------------------------

  dynamic "group" {
    for_each = local.groups
    labels   = [group.value.name]

    content {
      count          = group.value.count
      shutdown_delay = var.shutdown_delay

      # -----------------------------------------------------------------------
      # Placement
      # -----------------------------------------------------------------------

      dynamic "constraint" {
        for_each = var.distinct_hosts ? [true] : []
        content {
          operator = "distinct_hosts"
          value    = "true"
        }
      }

      # -----------------------------------------------------------------------
      # Network: S3 listener, plus the metrics listener when registered
      # -----------------------------------------------------------------------

      network {
        dynamic "port" {
          for_each = group.value.static_port > 0 ? [] : [true]
          labels   = ["http"]
          content {
            to = 9000
          }
        }
        dynamic "port" {
          for_each = group.value.static_port > 0 ? [group.value.static_port] : []
          labels   = ["http"]
          content {
            static = port.value
            to     = 9000
          }
        }
        dynamic "port" {
          for_each = var.register_metrics_service ? [true] : []
          labels   = ["metrics"]
          content {
            to = 9001
          }
        }
      }

      # --- Reserves room for the spill files in the task's tmp directory ---
      ephemeral_disk {
        size = var.ephemeral_disk
      }

      # -----------------------------------------------------------------------
      # Services
      # -----------------------------------------------------------------------

      service {
        name     = group.value.name
        port     = "http"
        provider = var.service_provider
        tags     = group.value.tags

        # --- Readiness: 503 until started and while draining ---
        check {
          name            = "ready"
          type            = local.check_type
          path            = local.check_type == "http" ? "/health/ready" : ""
          protocol        = local.check_protocol
          tls_skip_verify = local.check_skip_verify
          interval        = "5s"
          timeout         = "2s"
          on_update       = "require_healthy"
        }

        # --- Liveness: stays 200 through a database outage; the grace covers first-boot migrations ---
        check {
          name            = "alive"
          type            = local.check_type
          path            = local.check_type == "http" ? "/health" : ""
          protocol        = local.check_protocol
          tls_skip_verify = local.check_skip_verify
          interval        = "10s"
          timeout         = "3s"

          check_restart {
            limit = 3
            grace = "5m"
          }
        }
      }

      dynamic "service" {
        for_each = var.register_metrics_service ? [true] : []
        content {
          name     = "${group.value.name}-metrics"
          port     = "metrics"
          provider = var.service_provider
          tags     = ["metrics"]
        }
      }

      # -----------------------------------------------------------------------
      # Task: s3-orchestrator
      # -----------------------------------------------------------------------

      task "s3-orchestrator" {
        driver = "docker"

        # ---------------------------------------------------------------------
        # Vault, only when vault_role is set
        # ---------------------------------------------------------------------

        dynamic "vault" {
          for_each = var.vault_role == "" ? [] : [var.vault_role]
          content {
            role        = vault.value
            change_mode = "noop"
          }
        }

        # ---------------------------------------------------------------------
        # Container
        # ---------------------------------------------------------------------

        config {
          image           = "${var.image}:${var.version}"
          args            = ["-config", "/secrets/config.yaml", "-mode", group.value.mode]
          ports           = var.register_metrics_service ? ["http", "metrics"] : ["http"]
          readonly_rootfs = true
          cap_drop        = ["all"]
          security_opt    = ["no-new-privileges"]

          # --- Client, backend and database connections share one fd table ---
          ulimit {
            nofile = "65535:65535"
          }

          # --- The OS temp directory, where server.spill_dir defaults ---
          mount {
            type   = "bind"
            source = "tmp"
            target = "/tmp"
          }
        }

        env = var.env

        # ---------------------------------------------------------------------
        # Templates: config, environment and extra files
        # ---------------------------------------------------------------------

        # --- config.yaml from config_file, secrets left as ${NAME} ---
        template {
          data        = file(var.config_file)
          destination = "secrets/config.yaml"
          change_mode = "restart"

          dynamic "wait" {
            for_each = var.config_wait.min == "" ? [] : [var.config_wait]
            content {
              min = wait.value.min
              max = wait.value.max == "" ? wait.value.min : wait.value.max
            }
          }
        }

        # --- Environment the config's ${NAME} references expand from ---
        dynamic "template" {
          for_each = var.env_template == "" ? [] : [var.env_template]
          content {
            data        = template.value
            destination = "secrets/env"
            env         = true
            change_mode = "restart"
          }
        }

        # --- Extra files the config points at ---
        dynamic "template" {
          for_each = var.templates
          content {
            data        = template.value.data
            destination = "secrets/${template.value.destination}"
            change_mode = "restart"
          }
        }

        # --- TLS certificate and key, reloaded in place ---
        dynamic "template" {
          for_each = var.cert_templates
          content {
            data          = template.value.data
            destination   = "secrets/${template.value.destination}"
            change_mode   = "signal"
            change_signal = "SIGHUP"
          }
        }

        # ---------------------------------------------------------------------
        # Resources and Shutdown
        # ---------------------------------------------------------------------

        resources {
          cpu        = var.resources.cpu
          memory     = var.resources.memory
          memory_max = var.resources.memory_max > 0 ? var.resources.memory_max : null
        }

        kill_signal  = "SIGTERM"
        kill_timeout = var.kill_timeout
      }
    }
  }
}
