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
      name = "s3-orchestrator"
      port = "http"

      check {
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "3s"
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
      }

      template {
        destination = "local/config.yaml"
        data        = <<-YAML
          server:
            listen_addr: "0.0.0.0:9000"
            backend_timeout: "30s"

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
              quota_bytes: 10737418240

            - name: "minio-2"
              endpoint: "http://__HOST_IP__:19002"
              region: "us-east-1"
              bucket: "backend2"
              access_key_id: "minioadmin"
              secret_access_key: "minioadmin"
              force_path_style: true
              quota_bytes: 10737418240

          routing_strategy: "spread"

          replication:
            factor: 2
            worker_interval: "1m"
            batch_size: 50

          circuit_breaker:
            failure_threshold: 3
            open_timeout: "15s"
            cache_ttl: "60s"

          telemetry:
            metrics:
              enabled: true

          ui:
            enabled: true
            admin_key: "admin"
            admin_secret: "admin"
        YAML
      }

      resources {
        cpu    = 256
        memory = 256
      }
    }
  }
}
