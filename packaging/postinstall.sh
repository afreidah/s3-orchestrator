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

systemctl daemon-reload
systemctl enable s3-orchestrator
