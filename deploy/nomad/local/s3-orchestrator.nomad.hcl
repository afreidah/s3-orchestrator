# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Dev Nomad Job (nomad agent -dev + docker-compose)
#
# Author: Alex Freidah
#
# Simplified job for local testing against docker-compose backing services.
# No Vault dependency: demo.sh inlines the shared deploy/local/config.yaml with
# hardcoded dev credentials in place of __CONFIG__, and replaces __INSTANCES__
# with the instance count. The production example is
# deploy/nomad/s3-orchestrator.nomad.hcl.
#
# Runs several instances sharing Postgres and Redis behind the Traefik job in
# traefik.nomad.hcl, which is how a production fleet is deployed.
# -------------------------------------------------------------------------------

job "s3-orchestrator" {
  datacenters = ["dc1"]
  type        = "service"

  group "s3-orchestrator" {
    count = __INSTANCES__

    # Dynamic host ports, so every instance on the one dev agent gets its
    # own. Clients reach the fleet through Traefik on :9000.
    network {
      port "http" {
        to = 9000
      }
      # Dedicated metrics+pprof listener. Pprof endpoints are mounted
      # here, not on the S3 listener, so runtime internals stay off
      # the public port. In production this binds to an internal-only
      # interface (the demo binds 0.0.0.0 so Prometheus can reach each
      # instance through its published port).
      port "metrics" {
        to = 9001
      }
    }

    service {
      name     = "s3-orchestrator"
      port     = "http"
      provider = "nomad"
      tags = [
        "traefik.enable=true",
        "traefik.http.routers.s3-orchestrator.entrypoints=s3",
        "traefik.http.routers.s3-orchestrator.rule=PathPrefix(`/`)",
      ]

      # Liveness - always 200, keeps the allocation alive during DB outages.
      check {
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "3s"
      }

      # Readiness - returns 503 until startup completes and during shutdown
      # drain. Gates rolling deploys so traffic only routes to ready instances.
      check {
        type      = "http"
        path      = "/health/ready"
        interval  = "5s"
        timeout   = "2s"
        on_update = "require_healthy"
      }
    }

    # Registered separately so Prometheus discovers every instance's metrics
    # listener from the Nomad registry.
    service {
      name     = "s3-orchestrator-metrics"
      port     = "metrics"
      provider = "nomad"
    }

    task "s3-orchestrator" {
      driver = "docker"

      config {
        image = "s3-orchestrator:local"
        # `metrics` must be listed alongside `http` so Nomad's docker
        # driver actually publishes the port from the container to the
        # host. Declaring it only in the network block reserves the
        # number but does not bind it.
        ports = ["http", "metrics"]

        volumes = [
          "local/config.yaml:/etc/s3-orchestrator/config.yaml",
        ]

        ulimit {
          nofile = "65535:65535"
        }
      }

      # The same limits as the Kubernetes demo's pods, with the Go memory
      # limit at 90% of the task's memory.
      env {
        GOMEMLIMIT = "920MiB"
        GOMAXPROCS = "2"
      }

      # The shared deploy/local/config.yaml, rendered by demo.sh and inlined
      # in place of __CONFIG__.
      template {
        destination = "local/config.yaml"
        data        = <<-YAML
          __CONFIG__
        YAML
      }

      resources {
        cpu    = 2000
        memory = 1024
      }
    }
  }
}
