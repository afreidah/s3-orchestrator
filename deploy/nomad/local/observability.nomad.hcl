# -------------------------------------------------------------------------------
# Demo Observability Stack - Local Dev Nomad Job
#
# Author: Alex Freidah
#
# Prometheus, Grafana, Tempo, Loki and Alloy for the local Nomad demo, run
# with the same images and the same deploy/monitoring configs as
# docker-compose.test.yml. The configs are bind-mounted from the repository,
# which needs the Docker volumes agent.hcl enables, so editing one and
# re-running the demo is enough to pick it up.
#
# Prometheus, Tempo and Loki run on the host network, as they do in compose:
# Prometheus discovers the orchestrator instances from the Nomad API and
# scrapes them at the 127.0.0.1 addresses the dev agent registers, neither of
# which a bridged container can reach. Grafana and Alloy stay on the default
# bridge and reach them through the bridge gateway as host.docker.internal.
#
# demo.sh passes host_ip (the bridge gateway) and repo_root.
# -------------------------------------------------------------------------------

variable "host_ip" {
  type        = string
  description = "Docker bridge gateway, where bridged containers reach the host-network tasks."
}

variable "repo_root" {
  type        = string
  description = "Absolute path of the repository, for mounting the deploy/monitoring configs."
}

job "demo-observability" {
  datacenters = ["dc1"]
  type        = "service"

  # -------------------------------------------------------------------------
  # PROMETHEUS
  # -------------------------------------------------------------------------

  group "prometheus" {
    # Reserved so nothing else is placed on it; the task binds it directly
    # on the host network.
    network {
      port "http" {
        static = 19090
      }
    }

    task "prometheus" {
      driver = "docker"

      config {
        image        = "prom/prometheus:latest"
        network_mode = "host"
        args = [
          "--config.file=/etc/prometheus/prometheus.yml",
          "--web.listen-address=:19090",
          # Tempo's metrics generator writes service-graph and span metrics.
          "--web.enable-remote-write-receiver",
        ]
        volumes = [
          "${var.repo_root}/deploy/monitoring/prometheus.yml:/etc/prometheus/prometheus.yml:ro",
        ]
      }

      service {
        name     = "demo-prometheus"
        port     = "http"
        provider = "nomad"

        check {
          type     = "http"
          path     = "/-/ready"
          interval = "10s"
          timeout  = "2s"
        }
      }

      resources {
        cpu        = 50
        memory     = 256
        memory_max = 1024
      }
    }
  }

  # -------------------------------------------------------------------------
  # TEMPO
  # -------------------------------------------------------------------------

  group "tempo" {
    network {
      port "http" {
        static = 3200
      }
      # OTLP gRPC, where the orchestrator instances send traces.
      port "otlp" {
        static = 4317
      }
    }

    task "tempo" {
      driver = "docker"

      # PROMETHEUS_REMOTE_WRITE_URL is left unset, so tempo.yml's default -
      # Prometheus on the host network - applies.
      config {
        image        = "grafana/tempo:2.9.1"
        network_mode = "host"
        args         = ["-config.file=/etc/tempo.yml", "-config.expand-env=true"]
        volumes = [
          "${var.repo_root}/deploy/monitoring/tempo.yml:/etc/tempo.yml:ro",
        ]
      }

      service {
        name     = "demo-tempo"
        port     = "http"
        provider = "nomad"

        check {
          type     = "http"
          path     = "/ready"
          interval = "10s"
          timeout  = "2s"
        }
      }

      # Trace volume under a perf run is what sizes this, not the idle case.
      resources {
        cpu        = 50
        memory     = 256
        memory_max = 2048
      }
    }
  }

  # -------------------------------------------------------------------------
  # LOKI
  # -------------------------------------------------------------------------

  group "loki" {
    network {
      port "http" {
        static = 3100
      }
      port "grpc" {
        static = 9096
      }
    }

    task "loki" {
      driver = "docker"
      # Root, as in compose, so Loki can create its storage under /var/loki
      # inside the container.
      user = "0"

      config {
        image        = "grafana/loki:3.4.2"
        network_mode = "host"
        args         = ["-config.file=/etc/loki/config.yml"]
        volumes = [
          "${var.repo_root}/deploy/monitoring/loki.yml:/etc/loki/config.yml:ro",
        ]
      }

      service {
        name     = "demo-loki"
        port     = "http"
        provider = "nomad"

        check {
          type     = "http"
          path     = "/ready"
          interval = "10s"
          timeout  = "2s"
        }
      }

      resources {
        cpu        = 50
        memory     = 256
        memory_max = 1024
      }
    }
  }

  # -------------------------------------------------------------------------
  # GRAFANA
  # -------------------------------------------------------------------------

  group "grafana" {
    network {
      port "http" {
        static = 13000
        to     = 3000
      }
    }

    task "grafana" {
      driver = "docker"

      config {
        image       = "grafana/grafana:latest"
        ports       = ["http"]
        extra_hosts = ["host.docker.internal:${var.host_ip}"]
        volumes = [
          "${var.repo_root}/deploy/monitoring/grafana/provisioning:/etc/grafana/provisioning:ro",
          "${var.repo_root}/grafana/s3-orchestrator.json:/var/lib/grafana/dashboards/s3-orchestrator.json:ro",
        ]
      }

      # An anonymous admin session, so the demo opens straight onto the
      # dashboard. The datasource URLs feed the provisioning file.
      env {
        GF_AUTH_ANONYMOUS_ENABLED  = "true"
        GF_AUTH_ANONYMOUS_ORG_ROLE = "Admin"
        GF_AUTH_DISABLE_LOGIN_FORM = "true"
        PROMETHEUS_URL             = "http://host.docker.internal:19090"
        LOKI_URL                   = "http://host.docker.internal:3100"
        TEMPO_URL                  = "http://host.docker.internal:3200"
      }

      service {
        name     = "demo-grafana"
        port     = "http"
        provider = "nomad"

        check {
          type     = "http"
          path     = "/api/health"
          interval = "10s"
          timeout  = "2s"
        }
      }

      # Grafana idles near 600 MB, and with the dashboard open during a perf
      # run its query load pushes it past 1 GB.
      resources {
        cpu        = 50
        memory     = 256
        memory_max = 2048
      }
    }
  }

  # -------------------------------------------------------------------------
  # ALLOY
  # -------------------------------------------------------------------------

  # Tails every container's logs through the Docker socket and ships them to
  # Loki. Container names carry the allocation ID, which alloy.alloy strips
  # to label each stream with its task name. No service: nothing calls it,
  # and the deployment counts it healthy once it stays running.
  group "alloy" {
    task "alloy" {
      driver = "docker"

      config {
        image       = "grafana/alloy:latest"
        args        = ["run", "/etc/alloy/config.alloy"]
        extra_hosts = ["host.docker.internal:${var.host_ip}"]
        volumes = [
          "${var.repo_root}/deploy/monitoring/alloy.alloy:/etc/alloy/config.alloy:ro",
          "/var/run/docker.sock:/var/run/docker.sock:ro",
        ]
      }

      resources {
        cpu        = 50
        memory     = 128
        memory_max = 512
      }
    }
  }
}
