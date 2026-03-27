# -------------------------------------------------------------------------------
# S3 Orchestrator - Local Dev Nomad Job (nomad agent -dev + docker-compose)
#
# Author: Alex Freidah
#
# Simplified job for local testing against docker-compose backing services.
# No Vault dependency -- config is rendered directly with hardcoded dev
# credentials. The __HOST_IP__ placeholder is replaced by demo.sh with the
# Docker bridge gateway so the container can reach host-network services.
# -------------------------------------------------------------------------------

job "s3-orchestrator" {
  datacenters = ["dc1"]
  type        = "service"

  group "s3-orchestrator" {
    count = 1

    network {
      port "http" {
        static = 9000
      }
    }

    service {
      name     = "s3-orchestrator"
      port     = "http"
      provider = "nomad"

      # Liveness — always 200, keeps the allocation alive during DB outages.
      check {
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "3s"
      }

      # Readiness — returns 503 until startup completes and during shutdown
      # drain. Gates rolling deploys so traffic only routes to ready instances.
      check {
        type      = "http"
        path      = "/health/ready"
        interval  = "5s"
        timeout   = "2s"
        on_update = "require_healthy"
      }
    }

    task "s3-orchestrator" {
      driver = "docker"

      config {
        image = "s3-orchestrator:local"
        ports = ["http"]

        volumes = [
          "local/config.yaml:/etc/s3-orchestrator/config.yaml",
        ]

        ulimit {
          nofile = "65535:65535"
        }
      }

      env {
        GOMEMLIMIT  = "1843MiB"
        GOMAXPROCS  = "2"
      }

      template {
        destination = "local/config.yaml"
        data        = <<-YAML
          server:
            listen_addr: "0.0.0.0:9000"
            backend_timeout: "30s"
            max_concurrent_reads: 200
            max_concurrent_writes: 200
            load_shed_threshold: 0.8
            admission_wait: "100ms"

          database:
            host: "__HOST_IP__"
            port: 15432
            database: "s3proxy_test"
            user: "s3proxy"
            password: "s3proxy"
            ssl_mode: "disable"

          buckets:
            - name: "photos"
              credentials:
                - access_key_id: "photoskey"
                  secret_access_key: "photossecret"

          backends:
            - name: "minio-1"
              endpoint: "http://__HOST_IP__:19000"
              region: "us-east-1"
              bucket: "backend1"
              access_key_id: "minioadmin"
              secret_access_key: "minioadmin"
              force_path_style: true
              unsigned_payload: true
              quota_bytes: 10737418240

            - name: "minio-2"
              endpoint: "http://__HOST_IP__:19002"
              region: "us-east-1"
              bucket: "backend2"
              access_key_id: "minioadmin"
              secret_access_key: "minioadmin"
              force_path_style: true
              unsigned_payload: true
              quota_bytes: 10737418240

            - name: "minio-3"
              endpoint: "http://__HOST_IP__:19004"
              region: "us-east-1"
              bucket: "backend3"
              access_key_id: "minioadmin"
              secret_access_key: "minioadmin"
              force_path_style: true
              unsigned_payload: true
              quota_bytes: 10737418240

          routing_strategy: "spread"

          replication:
            factor: 2
            worker_interval: "10s"
            batch_size: 400

          rebalance:
            enabled: true
            strategy: "spread"
            interval: "6h"
            batch_size: 300
            threshold: 0.7
            concurrency: 5

          encryption:
            enabled: true
            master_key: "F2rpnHM7TmwJ4/DalNfk0cvCCPmHTfvB9LyhBLPoCVc="
            chunk_size: 262144

          integrity:
            enabled: true
            verify_on_read: true
            scrubber_interval: "1h"
            scrubber_batch_size: 50

          # --- Object data cache (disabled by default) ---
          # In-memory LRU cache for frequently read objects. Reduces backend
          # API calls and egress by serving repeated reads from memory. Objects
          # are cached after the first full GET and invalidated on write.
          cache:
            enabled: true
            max_size: "256MB"            # total cache capacity (default: 256MB)
            max_object_size: "10MB"      # skip caching objects larger than this (default: 10MB)
            ttl: "5m"                    # cached entry lifetime (default: 5m)

          circuit_breaker:
            failure_threshold: 3
            open_timeout: "15s"
            cache_ttl: "60s"

          backend_circuit_breaker:
            enabled: true
            failure_threshold: 3
            open_timeout: "15s"

          telemetry:
            metrics:
              enabled: true
            tracing:
              enabled: true
              endpoint: "__HOST_IP__:4317"
              insecure: true

          rate_limit:
            enabled: true
            requests_per_sec: 1000
            burst: 2000

          ui:
            enabled: true
            admin_key: "admin"
            admin_secret: "admin"
            admin_token: "admin"     # Separate token for admin API (defaults to admin_key)
            # force_secure_cookies: false        # Local dev — no TLS proxy
        YAML
      }

      resources {
        cpu    = 2048
        memory = 2048
      }
    }
  }
}
