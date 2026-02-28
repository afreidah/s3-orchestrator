# Quickstart

Get the S3 Orchestrator running locally in under a minute. The only prerequisites are **Go**, **Docker**, and **Make**.

## Start the orchestrator

```bash
git clone https://github.com/afreidah/s3-orchestrator.git
cd s3-orchestrator
make run
```

This starts two MinIO instances and a PostgreSQL database via Docker Compose, then launches the orchestrator pointing at them. The included `config.yaml` is pre-configured for this environment — no manual setup required.

## What `make run` does

1. **`make integration-deps`** — runs `docker compose -f docker-compose.test.yml up -d` to start:
   - MinIO 1 on `localhost:19000` (bucket: `backend1`)
   - MinIO 2 on `localhost:19002` (bucket: `backend2`)
   - PostgreSQL on `localhost:15432` (database: `s3proxy_test`)
   - A setup container that creates the MinIO buckets
2. **`go run ./cmd/s3-orchestrator -config config.yaml`** — compiles and starts the server on port 9000

## Test it

Upload and retrieve an object using the AWS CLI:

```bash
aws --endpoint-url http://localhost:9000 \
    s3 cp /etc/hostname s3://photos/test.txt \
    --region us-east-1

aws --endpoint-url http://localhost:9000 \
    s3 cp s3://photos/test.txt - \
    --region us-east-1
```

The config defines three virtual buckets (`photos`, `documents`, `backups`) with these credentials:

| Bucket | Access Key | Secret Key |
|--------|-----------|------------|
| `photos` | `photoskey` | `photossecret` |
| `documents` | `docskey` | `docssecret` |
| `backups` | `backupskey` | `backupssecret` |

Configure the AWS CLI profile to match:

```bash
aws configure --profile orchestrator
# AWS Access Key ID: photoskey
# AWS Secret Access Key: photossecret
# Default region name: us-east-1
# Default output format: json
```

Then use it:

```bash
alias s3o='aws --profile orchestrator --endpoint-url http://localhost:9000'
s3o s3 cp ./myfile.txt s3://photos/myfile.txt
s3o s3 ls s3://photos/
```

## Web dashboard

The dashboard is enabled at [http://localhost:9000/ui/](http://localhost:9000/ui/) with credentials `admin` / `admin`.

## Metrics

Prometheus metrics are available at [http://localhost:9000/metrics](http://localhost:9000/metrics).

## Validate the config

Check the configuration file without starting the server:

```bash
go run ./cmd/s3-orchestrator validate -config config.yaml
```

## Clean up

Stop the Docker containers and remove build artifacts:

```bash
make clean
```

## Next steps

- [Admin Guide](admin-guide.md) — production deployment, backend configuration, operational procedures
- [User Guide](user-guide.md) — client setup for AWS CLI, rclone, boto3, and Go SDK
- [README](../README.md) — architecture, features, and full configuration reference
