# -------------------------------------------------------------------------------
# s3-orchestrator Pack - Variables
#
# Author: Alex Freidah
#
# Every input the pack takes. The config comes from a file read when the pack
# renders and holds no secrets: it references them as $NAME, and the task's
# environment supplies them, by default from the Nomad Variable at
# nomad/jobs/<job name>, which the job can read with no extra policy.
# -------------------------------------------------------------------------------

# -------------------------------------------------------------------------------
# Job and Placement
# -------------------------------------------------------------------------------

variable "job_name" {
  description = "Job ID, and the Nomad Variable path the default config reads"
  type        = string
  default     = "s3-orchestrator"
}

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
}

variable "distinct_hosts" {
  description = "Place each instance on a different client node"
  type        = bool
  default     = false
}

variable "constraints" {
  description = "Extra job constraints: [{ attribute, operator, value }]"
  type = list(object({
    attribute = string
    operator  = string
    value     = string
  }))
  default = []
}

# -------------------------------------------------------------------------------
# Image and Mode
# -------------------------------------------------------------------------------

variable "image" {
  description = "Container image repository"
  type        = string
  default     = "ghcr.io/afreidah/s3-orchestrator"
}

variable "version" {
  description = "Container image tag; a released pack defaults to its own release"
  type        = string
  default     = ""
}

variable "mode" {
  description = "Run mode: all, api or worker; split a fleet by running the pack twice under different job names"
  type        = string
  default     = "all"
}

# -------------------------------------------------------------------------------
# Networking and Service
# -------------------------------------------------------------------------------

variable "http_port" {
  description = "Static host port for the S3 listener; 0 picks a dynamic port"
  type        = number
  default     = 0
}

variable "service_provider" {
  description = "Service registration provider: nomad or consul"
  type        = string
  default     = "nomad"
}

variable "service_name" {
  description = "Service name for the S3 listener; empty uses the job name"
  type        = string
  default     = ""
}

variable "service_tags" {
  description = "Tags on the S3 service, such as Traefik router rules"
  type        = list(string)
  default     = []
}

variable "register_metrics_service" {
  description = "Register <service name>-metrics on port 9001 for Prometheus discovery; the config must listen there"
  type        = bool
  default     = true
}

# -------------------------------------------------------------------------------
# Resources and Shutdown
# -------------------------------------------------------------------------------

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

# -------------------------------------------------------------------------------
# Configuration and Secrets
# -------------------------------------------------------------------------------

variable "config_file" {
  description = "Path to the config.yaml to run, read when the pack renders; reference secrets in it as $NAME or $${NAME}"
  type        = string
  default     = ""
}

variable "config_wait" {
  description = "Quiet period before a changed config re-renders, so service lookups that settle in steps restart the task once; an empty min leaves it off"
  type = object({
    min = string
    max = string
  })
  default = {
    min = ""
    max = ""
  }
}

variable "env_template" {
  description = "Nomad template rendered as the task's environment; the default exports every item of the Nomad Variable at nomad/jobs/<job name>, and empty sets nothing"
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
  description = "Extra files the config points at, such as TLS certificates or key files, rendered into secrets/<destination>: [{ data, destination }]"
  type = list(object({
    data        = string
    destination = string
  }))
  default = []
}

variable "vault_role" {
  description = "Vault role the task logs in with through workload identity, for an env_template that reads Vault; empty leaves Vault out"
  type        = string
  default     = ""
}
