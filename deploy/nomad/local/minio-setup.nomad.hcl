# -------------------------------------------------------------------------------
# Demo MinIO Bucket Setup - Local Dev Nomad Batch Job
#
# Author: Alex Freidah
#
# Creates the bucket each MinIO backend in services.nomad.hcl serves, the job
# the minio-setup service does in docker-compose.test.yml. Each backend's
# address comes from Nomad's service registry. demo.sh runs this once the
# backends are healthy, and purges any earlier run first so it runs again.
# -------------------------------------------------------------------------------

job "demo-minio-setup" {
  datacenters = ["dc1"]
  type        = "batch"

  group "minio-setup" {
    # The script already waits for every backend to answer, so a failure
    # past that is worth surfacing rather than retrying indefinitely.
    restart {
      attempts = 2
      delay    = "5s"
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "mc" {
      driver = "docker"

      config {
        image      = "ghcr.io/afreidah/mc:RELEASE.2025-04-16T18-13-26Z"
        entrypoint = ["/bin/sh"]
        args       = ["/local/setup.sh"]
      }

      # make_bucket waits for one backend to answer, then creates its bucket.
      # A backend not yet in the registry renders as an empty address; the
      # template re-renders once it registers, which restarts the task (the
      # default change_mode) against the real one.
      template {
        destination = "local/setup.sh"
        data        = <<-EOT
          set -eu

          make_bucket() {
            alias="$1" addr="$2" bucket="$3" tries=0
            until mc alias set "$alias" "http://$addr" minioadmin minioadmin >/dev/null 2>&1; do
              tries=$((tries + 1))
              if [ "$tries" -ge 60 ]; then
                echo "$alias at '$addr' did not answer" >&2
                exit 1
              fi
              sleep 2
            done
            mc mb --ignore-existing "$alias/$bucket"
          }

          make_bucket minio1 '{{ range nomadService "demo-minio-1" }}{{ .Address }}:{{ .Port }}{{ end }}' backend1
          make_bucket minio2 '{{ range nomadService "demo-minio-2" }}{{ .Address }}:{{ .Port }}{{ end }}' backend2
          make_bucket minio3 '{{ range nomadService "demo-minio-3" }}{{ .Address }}:{{ .Port }}{{ end }}' backend3
        EOT
      }

      resources {
        cpu    = 50
        memory = 128
      }
    }
  }
}
