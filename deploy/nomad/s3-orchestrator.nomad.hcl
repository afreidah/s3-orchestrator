# -------------------------------------------------------------------------------
# S3 Orchestrator - Nomad Job
#
# Author: Alex Freidah
#
# Runs the orchestrator as a service job with its config read from a file when
# the job is submitted. Secrets stay out of the file and reach the task as
# environment variables, by default from the Nomad Variable at
# nomad/jobs/s3-orchestrator. Production values are in s3-orchestrator.vars.hcl:
#   nomad job run -var-file=s3-orchestrator.vars.hcl -var=version=v0.178.0 s3-orchestrator.nomad.hcl
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
  description = "Number of instances; more than one requires redis in the config"
  type        = number
  default     = 1

  validation {
    condition     = var.count >= 1
    error_message = "count must be at least 1."
  }
}

variable "distinct_hosts" {
  description = "Place each instance on a different client node"
  type        = bool
  default     = false
}

# --- Image and mode ---
variable "image" {
  description = "Container image repository"
  type        = string
  default     = "ghcr.io/afreidah/s3-orchestrator"
}

variable "version" {
  description = "Container image tag, such as v0.178.0"
  type        = string

  validation {
    condition     = var.version != "" && var.version != "latest"
    error_message = "version must name a release, such as v0.178.0."
  }
}

variable "mode" {
  description = "Run mode: all, api or worker"
  type        = string
  default     = "all"

  validation {
    condition     = contains(["all", "api", "worker"], var.mode)
    error_message = "mode must be all, api or worker."
  }
}

# --- Networking and service ---
variable "http_port" {
  description = "Static host port for the S3 listener; 0 picks a dynamic port"
  type        = number
  default     = 0
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
  description = "Tags on the S3 service, such as Traefik router rules"
  type        = list(string)
  default     = []
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

variable "shutdown_delay" {
  description = "Time between deregistering the services and sending SIGTERM, so clients stop routing first"
  type        = string
  default     = "5s"
}

variable "kill_timeout" {
  description = "SIGTERM to SIGKILL; covers up to 70s of HTTP, upload and background-service drains"
  type        = string
  default     = "90s"
}

# --- Configuration and secrets ---
variable "config_file" {
  description = "Path to the config.yaml to run, read when the job is submitted; reference secrets in it as $${NAME}"
  type        = string
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
  description = "Extra files the config points at, such as TLS certificates, rendered into secrets/<destination>"
  type = list(object({
    data        = string
    destination = string
  }))
  default = []
}

variable "vault_role" {
  description = "Vault role the task logs in with through workload identity; empty leaves Vault out"
  type        = string
  default     = ""
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

  update {
    max_parallel      = 1
    canary            = var.http_port > 0 ? 0 : 1
    auto_promote      = var.http_port == 0
    health_check      = "checks"
    min_healthy_time  = "30s"
    healthy_deadline  = "5m"
    progress_deadline = "10m"
    auto_revert       = true
  }

  # ---------------------------------------------------------------------------
  # Task Group: s3-orchestrator
  # ---------------------------------------------------------------------------

  group "s3-orchestrator" {
    count          = var.count
    shutdown_delay = var.shutdown_delay

    # -------------------------------------------------------------------------
    # Placement
    # -------------------------------------------------------------------------

    dynamic "constraint" {
      for_each = var.distinct_hosts ? [true] : []
      content {
        operator = "distinct_hosts"
        value    = "true"
      }
    }

    # -------------------------------------------------------------------------
    # Network: S3 listener and the dedicated metrics listener
    # -------------------------------------------------------------------------

    network {
      dynamic "port" {
        for_each = var.http_port > 0 ? [] : [true]
        labels   = ["http"]
        content {
          to = 9000
        }
      }
      dynamic "port" {
        for_each = var.http_port > 0 ? [var.http_port] : []
        labels   = ["http"]
        content {
          static = port.value
          to     = 9000
        }
      }
      port "metrics" {
        to = 9001
      }
    }

    # -------------------------------------------------------------------------
    # Services
    # -------------------------------------------------------------------------

    service {
      name     = "s3-orchestrator"
      port     = "http"
      provider = var.service_provider
      tags     = var.service_tags

      # --- Readiness: 503 until started and while draining ---
      check {
        name      = "ready"
        type      = "http"
        path      = "/health/ready"
        interval  = "5s"
        timeout   = "2s"
        on_update = "require_healthy"
      }

      # --- Liveness: stays 200 through a database outage ---
      check {
        name     = "alive"
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "3s"

        check_restart {
          limit = 3
          grace = "2m"
        }
      }
    }

    # --- The config must set telemetry.metrics.listen to 0.0.0.0:9001 ---
    service {
      name     = "s3-orchestrator-metrics"
      port     = "metrics"
      provider = var.service_provider
      tags     = ["metrics"]
    }

    # -------------------------------------------------------------------------
    # Task: s3-orchestrator
    # -------------------------------------------------------------------------

    task "s3-orchestrator" {
      driver = "docker"

      # -----------------------------------------------------------------------
      # Vault, only when vault_role is set
      # -----------------------------------------------------------------------

      dynamic "vault" {
        for_each = var.vault_role == "" ? [] : [var.vault_role]
        content {
          role        = vault.value
          change_mode = "noop"
        }
      }

      # -----------------------------------------------------------------------
      # Container
      # -----------------------------------------------------------------------

      config {
        image = "${var.image}:${var.version}"
        args  = ["-config", "/secrets/config.yaml", "-mode", var.mode]
        ports = ["http", "metrics"]

        # --- Client, backend and database connections share one fd table ---
        ulimit {
          nofile = "65535:65535"
        }
      }

      # -----------------------------------------------------------------------
      # Templates: config, environment and extra files
      # -----------------------------------------------------------------------

      # --- config.yaml from config_file, secrets left as ${NAME} ---
      template {
        data        = file(var.config_file)
        destination = "secrets/config.yaml"
        change_mode = "restart"
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

      # -----------------------------------------------------------------------
      # Resources and Shutdown
      # -----------------------------------------------------------------------

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
