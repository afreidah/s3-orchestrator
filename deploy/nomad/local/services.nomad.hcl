# -------------------------------------------------------------------------------
# Demo Backing Services - Local Dev Nomad Job
#
# Author: Alex Freidah
#
# PostgreSQL, Redis and the three MinIO backends the local Nomad demo's fleet
# runs against. Each service registers in Nomad's native service registry
# with its container address (address_mode = "driver"), and the orchestrator
# job's config template looks them up there. The container address is the
# one that works between tasks: the dev agent advertises 127.0.0.1 as the
# host address, and publishes ports on that address, so neither is
# reachable from inside another container, while containers on the default
# Docker bridge reach each other directly. The checks stay on the host
# address, the only mode a Nomad-provider check supports, and probe the
# published port. A restarted task usually comes back at a new container
# address; the orchestrator's template re-renders and its instances restart
# onto it.
#
# The host ports match the ones docker-compose.test.yml publishes, so psql,
# redis-cli and mc on the host reach either the same way. PostgreSQL and
# MinIO keep their data on Docker named volumes, as compose does. The task
# directory would be the obvious place, but the dev agent keeps it under
# /tmp, which is tmpfs on most Linux systems: data written there is held in
# RAM and charged to the task's memory limit, so a perf run's objects would
# get MinIO OOM-killed. The volumes outlive task restarts and allocation
# replacements alike; "demo.sh down" removes them. Mounting them needs the
# Docker volumes agent.hcl enables.
#
# Every task reserves little memory and may burst to memory_max, which
# agent.hcl also enables, so the stack fits beside the orchestrator fleet on
# a laptop.
# -------------------------------------------------------------------------------

job "demo-services" {
  datacenters = ["dc1"]
  type        = "service"

  # -------------------------------------------------------------------------
  # POSTGRESQL
  # -------------------------------------------------------------------------

  group "postgres" {
    network {
      port "db" {
        static = 15432
        to     = 5432
      }
    }

    task "postgres" {
      driver = "docker"

      config {
        # Debian rather than alpine: musl has no locale data, so text sorts
        # by byte regardless of collation and the stack under test stops
        # resembling a real deployment.
        image   = "postgres:16"
        ports   = ["db"]
        command = "postgres"
        mount {
          type   = "volume"
          target = "/var/lib/postgresql/data"
          source = "demo-postgres-data"
        }
        # The stock buffer and checkpoint settings checkpoint every 5
        # minutes, and the full-page writes after each one stall commits
        # for long enough to show up as multi-second write latency in a
        # perf run on a shared laptop disk. The connection limit covers
        # three instances at the shared config's max_conns of 90.
        args = [
          "-c", "max_connections=300",
          "-c", "shared_buffers=512MB",
          "-c", "checkpoint_timeout=15min",
          "-c", "max_wal_size=4GB",
        ]
      }

      # Demo-only credentials, matching deploy/local/config.yaml.
      env {
        POSTGRES_DB       = "s3proxy_test"
        POSTGRES_USER     = "s3proxy"
        POSTGRES_PASSWORD = "s3proxy"
      }

      service {
        name         = "demo-postgres"
        port         = "db"
        provider     = "nomad"
        address_mode = "driver"

        # Through Docker's userland proxy on the published port, which can
        # accept a connection before Postgres listens, so this only says the
        # task is up. demo.sh waits for pg_isready before the fleet starts.
        check {
          type     = "tcp"
          interval = "5s"
          timeout  = "2s"
        }
      }

      # shared_buffers alone is 512 MB, and each of up to 300 backends adds
      # its own working memory under a perf run.
      resources {
        cpu        = 200
        memory     = 1024
        memory_max = 4096
      }
    }
  }

  # -------------------------------------------------------------------------
  # REDIS
  # -------------------------------------------------------------------------

  # Shared usage counters: every instance sees the fleet's spend.
  group "redis" {
    network {
      port "redis" {
        static = 16379
        to     = 6379
      }
    }

    task "redis" {
      driver = "docker"

      config {
        image = "redis:7.4-alpine"
        ports = ["redis"]
      }

      service {
        name         = "demo-redis"
        port         = "redis"
        provider     = "nomad"
        address_mode = "driver"

        check {
          type     = "tcp"
          interval = "5s"
          timeout  = "2s"
        }
      }

      resources {
        cpu        = 50
        memory     = 128
        memory_max = 512
      }
    }
  }

  # -------------------------------------------------------------------------
  # MINIO BACKENDS
  # -------------------------------------------------------------------------

  # One group per backend, so each is scheduled, restarted and registered on
  # its own the way three independent storage providers would be. The image
  # is the project-owned GHCR mirror of a pinned upstream release, because
  # neither Docker Hub nor quay.io serves MinIO to anonymous pulls. The
  # credentials are demo-only and match deploy/local/config.yaml. Buckets are
  # created afterwards by minio-setup.nomad.hcl.
  group "minio-1" {
    network {
      port "api" {
        static = 19000
        to     = 9000
      }
    }

    task "minio-1" {
      driver = "docker"

      config {
        image = "ghcr.io/afreidah/minio:RELEASE.2025-04-22T22-12-26Z"
        ports = ["api"]
        mount {
          type   = "volume"
          target = "/data"
          source = "demo-minio-1-data"
        }
        args = ["server", "/data", "--console-address", ":9001"]
      }

      env {
        MINIO_ROOT_USER     = "minioadmin"
        MINIO_ROOT_PASSWORD = "minioadmin"
      }

      service {
        name         = "demo-minio-1"
        port         = "api"
        provider     = "nomad"
        address_mode = "driver"

        check {
          type     = "http"
          path     = "/minio/health/ready"
          interval = "5s"
          timeout  = "2s"
        }
      }

      resources {
        cpu        = 100
        memory     = 256
        memory_max = 1024
      }
    }
  }

  group "minio-2" {
    network {
      port "api" {
        static = 19002
        to     = 9000
      }
    }

    task "minio-2" {
      driver = "docker"

      config {
        image = "ghcr.io/afreidah/minio:RELEASE.2025-04-22T22-12-26Z"
        ports = ["api"]
        mount {
          type   = "volume"
          target = "/data"
          source = "demo-minio-2-data"
        }
        args = ["server", "/data", "--console-address", ":9001"]
      }

      env {
        MINIO_ROOT_USER     = "minioadmin"
        MINIO_ROOT_PASSWORD = "minioadmin"
      }

      service {
        name         = "demo-minio-2"
        port         = "api"
        provider     = "nomad"
        address_mode = "driver"

        check {
          type     = "http"
          path     = "/minio/health/ready"
          interval = "5s"
          timeout  = "2s"
        }
      }

      resources {
        cpu        = 100
        memory     = 256
        memory_max = 1024
      }
    }
  }

  group "minio-3" {
    network {
      port "api" {
        static = 19004
        to     = 9000
      }
    }

    task "minio-3" {
      driver = "docker"

      config {
        image = "ghcr.io/afreidah/minio:RELEASE.2025-04-22T22-12-26Z"
        ports = ["api"]
        mount {
          type   = "volume"
          target = "/data"
          source = "demo-minio-3-data"
        }
        args = ["server", "/data", "--console-address", ":9001"]
      }

      env {
        MINIO_ROOT_USER     = "minioadmin"
        MINIO_ROOT_PASSWORD = "minioadmin"
      }

      service {
        name         = "demo-minio-3"
        port         = "api"
        provider     = "nomad"
        address_mode = "driver"

        check {
          type     = "http"
          path     = "/minio/health/ready"
          interval = "5s"
          timeout  = "2s"
        }
      }

      resources {
        cpu        = 100
        memory     = 256
        memory_max = 1024
      }
    }
  }
}
