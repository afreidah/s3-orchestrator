---
title: "Downloads"
seoTitle: "s3-orchestrator downloads"
description: "Every artifact an s3-orchestrator release publishes: binaries, Debian packages, the container image, the Helm chart, the Nomad pack and job, the Terraform provider, and how to verify them."
weight: 25
---

Every release publishes the artifacts below at the release's version. The [latest release](https://github.com/afreidah/s3-orchestrator/releases/latest) shows the current version. The commands set it once, without the leading `v`:

```bash
VERSION=0.178.4
```

## Binaries

Statically linked binaries for Linux on amd64 and arm64.

- **Download:** [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases)
- **Docs:** [Quickstart](../docs/quickstart/)

```bash
curl -LO https://github.com/afreidah/s3-orchestrator/releases/download/v${VERSION}/s3-orchestrator_${VERSION}_linux_amd64.tar.gz
tar -xzf s3-orchestrator_${VERSION}_linux_amd64.tar.gz s3-orchestrator
./s3-orchestrator version
```

## Debian Packages

Packages for amd64 and arm64 that install the binary, a hardened systemd unit, and `/etc/default/s3-orchestrator` for secrets.

- **Download:** [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases)
- **Docs:** [Deploying with systemd](../guides/systemd-deployment/)

```bash
curl -LO https://github.com/afreidah/s3-orchestrator/releases/download/v${VERSION}/s3-orchestrator_${VERSION}_linux_amd64.deb
sudo dpkg -i s3-orchestrator_${VERSION}_linux_amd64.deb
```

## Container Image

Multi-arch image for amd64 and arm64, signed with cosign.

- **Download:** [ghcr.io/afreidah/s3-orchestrator](https://github.com/afreidah/s3-orchestrator/pkgs/container/s3-orchestrator)
- **Docs:** [Deployment](../docs/deployment/)

```bash
docker pull ghcr.io/afreidah/s3-orchestrator:v${VERSION}
```

## Helm Chart

OCI chart for Kubernetes, signed with cosign.

- **Download:** [ghcr.io/afreidah/charts/s3-orchestrator](https://github.com/afreidah/s3-orchestrator/pkgs/container/charts%2Fs3-orchestrator), [Artifact Hub](https://artifacthub.io/packages/helm/s3-orchestrator/s3-orchestrator)
- **Docs:** [Chart README](https://github.com/afreidah/s3-orchestrator/blob/main/deploy/helm/s3-orchestrator/README.md)

```bash
helm install s3-orchestrator oci://ghcr.io/afreidah/charts/s3-orchestrator \
  --version ${VERSION} -n s3-orchestrator --create-namespace -f my-values.yaml
```

## Nomad Pack

Pack for nomad-pack that runs the release's image by default.

- **Download:** [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases), [afreidah/nomad-pack-s3-orchestrator](https://github.com/afreidah/nomad-pack-s3-orchestrator), [Ramble](https://ramble.openwander.org/afreidah/nomad-pack-s3-orchestrator)
- **Docs:** [Pack README](https://github.com/afreidah/nomad-pack-s3-orchestrator#readme)

```bash
curl -LO https://github.com/afreidah/s3-orchestrator/releases/download/v${VERSION}/s3_orchestrator-pack-v${VERSION}.tar.gz
tar -xzf s3_orchestrator-pack-v${VERSION}.tar.gz
nomad-pack run ./s3_orchestrator --var config_file=./config.yaml
```

## Nomad Job

Plain Nomad job file that takes variables, with production values for two instances behind Traefik through Consul and secrets from Vault.

- **Download:** [s3-orchestrator.nomad.hcl](https://github.com/afreidah/s3-orchestrator/blob/main/deploy/nomad/s3-orchestrator.nomad.hcl), [s3-orchestrator.vars.hcl](https://github.com/afreidah/s3-orchestrator/blob/main/deploy/nomad/s3-orchestrator.vars.hcl), [Ramble](https://ramble.openwander.org/afreidah/s3-orchestrator)
- **Docs:** [Deploying on Nomad with Vault](../guides/nomad-vault-deployment/)

```bash
nomad job run -var-file=s3-orchestrator.vars.hcl -var=version=v${VERSION} -var=config_file=./config.yaml s3-orchestrator.nomad.hcl
```

## Terraform Provider

Provider for Terraform and OpenTofu that manages the buckets, users, credentials and grants a deployment serves.

- **Download:** [Terraform Registry](https://registry.terraform.io/providers/afreidah/s3-orchestrator/latest/docs), [OpenTofu Registry](https://search.opentofu.org/provider/afreidah/s3-orchestrator/latest)
- **Docs:** [Provisioning with Terraform](../guides/terraform-provider/)

```hcl
terraform {
  required_providers {
    s3orchestrator = {
      source = "afreidah/s3-orchestrator"
    }
  }
}
```

## Grafana Dashboard

Dashboard for the orchestrator's Prometheus metrics.

- **Download:** [s3-orchestrator.json](https://github.com/afreidah/s3-orchestrator/blob/main/grafana/s3-orchestrator.json)
- **Docs:** [Grafana Dashboard](../docs/grafana-dashboard/)

```bash
curl -LO https://raw.githubusercontent.com/afreidah/s3-orchestrator/v${VERSION}/grafana/s3-orchestrator.json
```

## Verification

Images, charts and each release's `checksums.txt` are signed with [cosign](https://github.com/sigstore/cosign) through GitHub Actions, with no long-lived key. `checksums.txt` covers every archive and package on the release, and each archive has an SBOM beside it.

- **Download:** [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases)
- **Docs:** [Verifying with cosign](https://docs.sigstore.dev/cosign/verifying/verify/)

```bash
# Release checksums, then the files they cover
cosign verify-blob checksums.txt --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github.com/afreidah/s3-orchestrator/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt

# Container image
cosign verify ghcr.io/afreidah/s3-orchestrator:v${VERSION} \
  --certificate-identity-regexp '^https://github.com/afreidah/s3-orchestrator/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Helm chart
cosign verify ghcr.io/afreidah/charts/s3-orchestrator:${VERSION} \
  --certificate-identity-regexp '^https://github.com/afreidah/s3-orchestrator/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Release Notes

Notes for each release, and the changes that need action on upgrade.

- **Download:** [GitHub Releases](https://github.com/afreidah/s3-orchestrator/releases)
- **Docs:** [Changelog](../docs/changelog/), [Version Migration](../docs/version-migration/)
