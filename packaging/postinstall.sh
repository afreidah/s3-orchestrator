#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Post-Install Script
#
# Author: Alex Freidah
#
# Reloads systemd unit files and enables the service. Does not start the
# service automatically to allow the operator to configure it first.
# -------------------------------------------------------------------------------

set -eu

# Create SQLite data directory with correct ownership.
mkdir -p /var/lib/s3-orchestrator
if id s3-orchestrator >/dev/null 2>&1; then
    chown s3-orchestrator:s3-orchestrator /var/lib/s3-orchestrator
fi

systemctl daemon-reload
systemctl enable s3-orchestrator
