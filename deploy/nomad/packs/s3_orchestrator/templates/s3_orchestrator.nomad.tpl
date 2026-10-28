# -------------------------------------------------------------------------------
# s3-orchestrator Pack - Job Template
#
# Author: Alex Freidah
#
# Runs the orchestrator as a service job with its config from config_file and
# its secrets in the task's environment. The preflight checks in _helpers.tpl
# run first and stop the render on a combination that cannot work.
# -------------------------------------------------------------------------------

[[ template "preflight" . -]]
[[- $static := gt (int (var "http_port" .)) 0 ]]

# -------------------------------------------------------------------------------
# Job
# -------------------------------------------------------------------------------

job [[ template "job_name" . ]] {
  [[- if var "region" . ]]
  region = [[ var "region" . | quote ]]
  [[- end ]]
  datacenters = [[ var "datacenters" . | toStringList ]]
  node_pool   = [[ var "node_pool" . | quote ]]
  type        = "service"
  [[- if var "namespace" . ]]
  namespace = [[ var "namespace" . | quote ]]
  [[- end ]]

  # ---------------------------------------------------------------------------
  # Update Strategy
  # ---------------------------------------------------------------------------

  update {
    max_parallel = 1
    [[- if $static ]]
    canary = 0
    [[- else ]]
    canary       = 1
    auto_promote = true
    [[- end ]]
    health_check      = "checks"
    min_healthy_time  = "30s"
    healthy_deadline  = "5m"
    progress_deadline = "10m"
    auto_revert       = true
  }
  [[- if var "constraints" . ]]

  # ---------------------------------------------------------------------------
  # Constraints
  # ---------------------------------------------------------------------------

  [[- range var "constraints" . ]]

  constraint {
    attribute = [[ .attribute | quote ]]
    [[- if .operator ]]
    operator = [[ .operator | quote ]]
    [[- end ]]
    value = [[ .value | quote ]]
  }
  [[- end ]]
  [[- end ]]

  # ---------------------------------------------------------------------------
  # Task Group: s3-orchestrator
  # ---------------------------------------------------------------------------

  group "s3-orchestrator" {
    count          = [[ var "count" . ]]
    shutdown_delay = [[ var "shutdown_delay" . | quote ]]
    [[- if var "distinct_hosts" . ]]

    # -------------------------------------------------------------------------
    # Placement
    # -------------------------------------------------------------------------

    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }
    [[- end ]]

    # -------------------------------------------------------------------------
    # Network: S3 listener, plus the metrics listener when registered
    # -------------------------------------------------------------------------

    network {
      port "http" {
        [[- if $static ]]
        static = [[ var "http_port" . ]]
        [[- end ]]
        to = 9000
      }
      [[- if var "register_metrics_service" . ]]
      port "metrics" {
        to = 9001
      }
      [[- end ]]
    }

    # -------------------------------------------------------------------------
    # Services
    # -------------------------------------------------------------------------

    service {
      name     = "[[ template "service_name" . ]]"
      port     = "http"
      provider = [[ var "service_provider" . | quote ]]
      [[- if var "service_tags" . ]]
      tags = [[ var "service_tags" . | toStringList ]]
      [[- end ]]

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
    [[- if var "register_metrics_service" . ]]

    service {
      name     = "[[ template "service_name" . ]]-metrics"
      port     = "metrics"
      provider = [[ var "service_provider" . | quote ]]
      tags     = ["metrics"]
    }
    [[- end ]]

    # -------------------------------------------------------------------------
    # Task: s3-orchestrator
    # -------------------------------------------------------------------------

    task "s3-orchestrator" {
      driver = "docker"
      [[- if var "vault_role" . ]]

      # -----------------------------------------------------------------------
      # Vault
      # -----------------------------------------------------------------------

      vault {
        role        = [[ var "vault_role" . | quote ]]
        change_mode = "noop"
      }
      [[- end ]]

      # -----------------------------------------------------------------------
      # Container
      # -----------------------------------------------------------------------

      config {
        image = "[[ var "image" . ]]:[[ var "version" . ]]"
        args  = ["-config", "/secrets/config.yaml", "-mode", [[ var "mode" . | quote ]]]
        [[- if var "register_metrics_service" . ]]
        ports = ["http", "metrics"]
        [[- else ]]
        ports = ["http"]
        [[- end ]]

        # --- Client, backend and database connections share one fd table ---
        ulimit {
          nofile = "65535:65535"
        }
      }

      # -----------------------------------------------------------------------
      # Templates: config, environment and extra files
      # -----------------------------------------------------------------------

      # --- config.yaml from config_file, secrets left as $NAME ---
      template {
        data        = <<EOH
[[ template "config" . ]]
EOH
        destination = "secrets/config.yaml"
        change_mode = "restart"
        [[- if var "config_wait.min" . ]]

        wait {
          min = [[ var "config_wait.min" . | quote ]]
          [[- if var "config_wait.max" . ]]
          max = [[ var "config_wait.max" . | quote ]]
          [[- end ]]
        }
        [[- end ]]
      }
      [[- if var "env_template" . ]]

      # --- Environment the config's $NAME references expand from ---
      template {
        data        = <<EOH
[[ var "env_template" . ]]
EOH
        destination = "secrets/env"
        env         = true
        change_mode = "restart"
      }
      [[- end ]]
      [[- range var "templates" . ]]

      # --- Extra file the config points at ---
      template {
        data        = <<EOH
[[ .data ]]
EOH
        destination = "secrets/[[ .destination ]]"
        change_mode = "restart"
      }
      [[- end ]]

      # -----------------------------------------------------------------------
      # Resources and Shutdown
      # -----------------------------------------------------------------------

      resources {
        cpu    = [[ var "resources.cpu" . ]]
        memory = [[ var "resources.memory" . ]]
        [[- if gt (int (var "resources.memory_max" .)) 0 ]]
        memory_max = [[ var "resources.memory_max" . ]]
        [[- end ]]
      }

      kill_signal  = "SIGTERM"
      kill_timeout = [[ var "kill_timeout" . | quote ]]
    }
  }
}
