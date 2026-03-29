# Security Policy

## Supported Versions

Only the latest release is actively supported with security fixes.

| Version | Supported |
|---------|-----------|
| Latest  | Yes       |
| Older   | No        |

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly via email:

**alex.freidah@gmail.com**

Please include:

- Description of the vulnerability
- Steps to reproduce
- S3 Orchestrator version and environment details
- Any relevant logs or configuration (redact secrets)

You should receive an acknowledgment within 48 hours. Please do not open a public GitHub issue for security vulnerabilities.

## Artifact Signing

All container images pushed to `ghcr.io` and release checksums on GitHub Releases are signed with [cosign](https://github.com/sigstore/cosign) using keyless Sigstore OIDC. SBOMs (SPDX) are attached to every release. See the [README](README.md#verify-artifact-signatures) for verification commands.

## Disclosure Policy

- Confirmed vulnerabilities will be patched in a new release as soon as possible.
- A security advisory will be published on GitHub once the fix is available.
- Credit will be given to reporters unless they prefer to remain anonymous.
