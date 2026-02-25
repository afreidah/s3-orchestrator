#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Pre-Install Script
#
# Author: Alex Freidah
#
# Creates the s3-orchestrator system user/group and required directories.
# Runs before package files are unpacked.
# -------------------------------------------------------------------------------

set -eu

if ! getent group s3-orchestrator >/dev/null 2>&1; then
    groupadd --system s3-orchestrator
fi

if ! getent passwd s3-orchestrator >/dev/null 2>&1; then
    useradd --system \
        --gid s3-orchestrator \
        --home-dir /var/lib/s3-orchestrator \
        --shell /usr/sbin/nologin \
        --no-create-home \
        s3-orchestrator
fi

install -d -o s3-orchestrator -g s3-orchestrator -m 0750 /var/lib/s3-orchestrator
install -d -o root -g s3-orchestrator -m 0750 /etc/s3-orchestrator
