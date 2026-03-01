#!/bin/bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Pre-Remove Script
#
# Author: Alex Freidah
#
# Stops and disables the service before package files are removed to prevent
# the binary from being deleted while the service is still running.
# -------------------------------------------------------------------------------

set -eu

systemctl stop s3-orchestrator || true
systemctl disable s3-orchestrator || true
