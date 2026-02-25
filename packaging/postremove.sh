#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Post-Remove Script
#
# Author: Alex Freidah
#
# Cleans up the system user and data directory on package purge. Skips cleanup
# on plain remove to preserve data for potential reinstall.
# -------------------------------------------------------------------------------

set -eu

if [ "${1:-}" = "purge" ]; then
    if getent passwd s3-orchestrator >/dev/null 2>&1; then
        userdel s3-orchestrator
    fi

    if getent group s3-orchestrator >/dev/null 2>&1; then
        groupdel s3-orchestrator
    fi

    rm -rf /var/lib/s3-orchestrator
fi
