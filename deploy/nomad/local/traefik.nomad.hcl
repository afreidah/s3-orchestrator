# -------------------------------------------------------------------------------
# Traefik - Local Dev Nomad Job
#
# Author: Alex Freidah
#
# Load balancer in front of the orchestrator instances, the way a production
# deployment runs them. Routes are read from Nomad's native service registry,
# so no Consul is needed; only services tagged traefik.enable=true are exposed.
# Clients, demo.sh and the perf suite all keep the single :9000 endpoint.
# -------------------------------------------------------------------------------

job "traefik" {
  datacenters = ["dc1"]
  type        = "service"

  group "traefik" {
    count = 1

    network {
      port "s3" {
        static = 9000
      }
      port "dashboard" {
        static = 8081
      }
    }

    task "traefik" {
      driver = "docker"

      config {
        image = "traefik:v3.5"
        # Host networking, so Traefik reaches the Nomad API and the
        # instances at the 127.0.0.1 addresses the dev agent registers.
        network_mode = "host"

        args = [
          "--entrypoints.s3.address=:9000",
          # Traefik cuts request bodies off after 60s by default, which
          # would fail large uploads on a slow link.
          "--entrypoints.s3.transport.respondingTimeouts.readTimeout=0",
          "--entrypoints.traefik.address=:8081",
          "--api.dashboard=true",
          "--api.insecure=true",
          "--providers.nomad=true",
          "--providers.nomad.endpoint.address=http://127.0.0.1:4646",
          "--providers.nomad.exposedByDefault=false",
          "--providers.nomad.refreshInterval=5s",
        ]
      }

      # The perf suite can hold thousands of connections open when an
      # upstream slows, which runs Traefik past 256 MB.
      resources {
        cpu    = 1000
        memory = 512
      }
    }
  }
}
