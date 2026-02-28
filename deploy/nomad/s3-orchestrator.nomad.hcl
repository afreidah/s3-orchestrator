# -------------------------------------------------------------------------------
# S3 Orchestrator - Nomad Job Specification
#
# Author: Alex Freidah
#
# Production-ready Nomad job for the s3-orchestrator. Deploys the service with
# three storage backends (OCI Object Storage, Backblaze B2, and MinIO) spanning
# multiple providers. With replication factor 2 and spread routing, every object
# is written to the least-utilized backend and asynchronously replicated to a
# second backend -- distributing copies across the pool like a storage mesh.
# Secrets are injected from HashiCorp Vault via Nomad template stanzas.
#
# Customize the variables block and Vault path, then deploy:
#   nomad job run -var="version=v1.0.0" s3-orchestrator.nomad.hcl
# -------------------------------------------------------------------------------

variable "version" {
  description = "Container image tag (use explicit versions, not 'latest')"
  type        = string
  default     = "v1.0.0"
}

variable "datacenter" {
  description = "Nomad datacenter"
  type        = string
  default     = "dc1"
}

job "s3-orchestrator" {
  datacenters = [var.datacenter]
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

      # Uncomment for Traefik integration:
      # tags = [
      #   "traefik.enable=true",
      #   "traefik.http.routers.s3-orchestrator.rule=Host(`s3.example.com`)",
      #   "traefik.http.routers.s3-orchestrator.tls=true",
      # ]
    }

    task "s3-orchestrator" {
      driver = "docker"

      config {
        image = "ghcr.io/afreidah/s3-orchestrator:${var.version}"
        ports = ["http"]

        volumes = [
          "secrets/config.yaml:/etc/s3-orchestrator/config.yaml",
        ]
      }

      # --- Vault integration for secret injection ---
      vault {
        policies = ["s3-orchestrator"]
      }

      # --- Configuration rendered from Vault secrets ---
      # All sensitive values are pulled from Vault KV v2.
      # Create the secret at: vault kv put secret/s3-orchestrator \
      #   db_password=... bucket_access_key=... bucket_secret_key=... \
      #   ui_admin_key=... ui_admin_secret=... \
      #   oci_endpoint=... oci_region=... oci_bucket=... \
      #   oci_access_key=... oci_secret_key=... \
      #   b2_endpoint=... b2_region=... b2_bucket=... \
      #   b2_access_key=... b2_secret_key=... \
      #   minio_endpoint=... minio_bucket=... \
      #   minio_access_key=... minio_secret_key=...
      template {
        destination = "secrets/config.yaml"
        perms       = "0400"
        data        = <<-YAML
          {{ with secret "secret/data/s3-orchestrator" }}
          # --- HTTP server ---
          server:
            listen_addr: "0.0.0.0:9000"
            max_object_size: 5368709120      # 5 GB
            backend_timeout: "2m"

            # --- TLS termination (optional) ---
            # Uncomment to enable TLS on the S3 endpoint. Provide PEM-encoded
            # certificate and key files mounted into the container.
            # tls:
            #   cert_file: "/secrets/tls/tls.crt"
            #   key_file: "/secrets/tls/tls.key"
            #   min_version: "1.2"
            #
            #   # Mutual TLS (mTLS) -- require client certificates signed by a
            #   # trusted CA. Clients must present a valid certificate to connect.
            #   # Useful for service-to-service authentication without S3 SigV4.
            #   # client_ca_file: "/secrets/tls/client-ca.crt"

          # --- PostgreSQL metadata store ---
          database:
            host: "postgres.service.consul"
            port: 5432
            database: "s3orchestrator"
            user: "s3orchestrator"
            password: "{{ .Data.data.db_password }}"
            ssl_mode: "require"
            max_conns: 10
            min_conns: 5
            max_conn_lifetime: "5m"

          # --- Virtual buckets ---
          # Each bucket is an isolated namespace with its own credentials.
          # Multiple credential sets per bucket support reader/writer separation.
          buckets:
            - name: "app-data"
              credentials:
                - access_key_id: "{{ .Data.data.bucket_access_key }}"
                  secret_access_key: "{{ .Data.data.bucket_secret_key }}"

          # --- Storage backends ---
          # Three backends across different providers give 40 GB combined
          # capacity. With replication factor 2, every object exists on two of
          # the three backends -- the replicator picks the best target each
          # cycle, spreading copies across the pool like a storage mesh.
          backends:
            - name: "oci"
              endpoint: "{{ .Data.data.oci_endpoint }}"
              region: "{{ .Data.data.oci_region }}"
              bucket: "{{ .Data.data.oci_bucket }}"
              access_key_id: "{{ .Data.data.oci_access_key }}"
              secret_access_key: "{{ .Data.data.oci_secret_key }}"
              force_path_style: true
              quota_bytes: 21474836480       # 20 GB
              api_request_limit: 50000       # monthly API call cap
              egress_byte_limit: 10737418240 # 10 GB monthly egress

            - name: "b2"
              endpoint: "{{ .Data.data.b2_endpoint }}"
              region: "{{ .Data.data.b2_region }}"
              bucket: "{{ .Data.data.b2_bucket }}"
              access_key_id: "{{ .Data.data.b2_access_key }}"
              secret_access_key: "{{ .Data.data.b2_secret_key }}"
              force_path_style: true
              quota_bytes: 10737418240       # 10 GB
              api_request_limit: 1000000

            - name: "minio"
              endpoint: "{{ .Data.data.minio_endpoint }}"
              region: "us-east-1"
              bucket: "{{ .Data.data.minio_bucket }}"
              access_key_id: "{{ .Data.data.minio_access_key }}"
              secret_access_key: "{{ .Data.data.minio_secret_key }}"
              force_path_style: true
              quota_bytes: 10737418240       # 10 GB

          # --- Write routing ---
          # "spread" distributes writes by lowest utilization ratio across backends.
          # "pack" fills backends sequentially until quota is reached.
          routing_strategy: "spread"

          # --- Replication ---
          # With 3 backends and factor 2, every object is written to one backend
          # then asynchronously replicated to a second. The replicator selects a
          # different backend each time, so copies spread across the pool rather
          # than always landing on the same pair. Reads automatically fail over
          # to the replica if the primary copy is unavailable.
          replication:
            factor: 2
            worker_interval: "5m"
            batch_size: 50

          # --- Rebalancer ---
          # Periodically redistributes objects to equalize utilization across
          # backends. Generates egress/ingress traffic on your backends.
          rebalance:
            enabled: true
            strategy: "spread"
            interval: "6h"
            batch_size: 100
            threshold: 0.1
            concurrency: 5

          # --- Circuit breaker ---
          # Detects database outages and enters degraded mode: reads broadcast
          # to all backends, writes return 503 until the database recovers.
          circuit_breaker:
            failure_threshold: 3
            open_timeout: "15s"
            cache_ttl: "60s"

          # --- Per-IP rate limiting ---
          rate_limit:
            enabled: true
            requests_per_sec: 100
            burst: 200
            # Trust X-Forwarded-For from internal load balancers
            trusted_proxies:
              - "10.0.0.0/8"
              - "172.16.0.0/12"

          # --- Web dashboard ---
          ui:
            enabled: true
            admin_key: "{{ .Data.data.ui_admin_key }}"
            admin_secret: "{{ .Data.data.ui_admin_secret }}"

          # --- Object lifecycle ---
          # Auto-delete objects matching a prefix after the configured retention.
          lifecycle:
            rules:
              - prefix: "tmp/"
                expiration_days: 7

          # --- Usage counter flush ---
          # Adaptive flushing shortens the interval when any backend approaches
          # a usage limit, improving enforcement accuracy.
          usage_flush:
            interval: "30s"
            adaptive_enabled: true
            adaptive_threshold: 0.8
            fast_interval: "5s"

          # --- Observability ---
          telemetry:
            metrics:
              enabled: true
              path: "/metrics"
            tracing:
              enabled: true
              endpoint: "tempo.service.consul:4317"
              insecure: true
              sample_rate: 0.1
          {{ end }}
        YAML
      }

      resources {
        cpu    = 256
        memory = 256
      }
    }
  }
}
