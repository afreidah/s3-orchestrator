#!/usr/bin/env bash
# -------------------------------------------------------------------------------
# S3 Orchestrator - Performance Suite Runner
#
# Runs the full perf-envelope scenario matrix end to end. Reads profile
# settings from this script (no env-var gymnastics required from the
# caller), writes per-scenario JSON to a timestamped directory, prints
# a final PASS/FAIL summary.
#
# Usage:
#   ./loadtest/run-suite.sh                  # smoke profile (default)
#   ./loadtest/run-suite.sh baseline         # baseline profile
#   ./loadtest/run-suite.sh saturation       # saturation-find profile
# -------------------------------------------------------------------------------

set -u
set -o pipefail

PROFILE="${1:-smoke}"
RESULTS_DIR="${PERF_RESULTS:-perf-results/$(date -u +%Y%m%dT%H%M%SZ)}"
ENDPOINT="${PERF_ENDPOINT:-http://localhost:9000}"
BUCKET="${PERF_BUCKET:-photos}"
BINARY="${PERF_BINARY:-./loadtest/s3-loadtest}"

# Profile-driven knobs. Edit these to retune; no caller env vars needed.
case "$PROFILE" in
  smoke)
    DURATION=60s; RATE=200; SEED=500; COLD_SEED=2000
    SIZES="1024,65536"
    RAMP_FROM=100; RAMP_TO=500; RAMP_STEP=100
    MPU_CONCURRENCY=5; MPU_PART_COUNT=3; MPU_PART_SIZE=5242880
    MAX_ERROR_RATE=0.01
    COMPRESSIBLE=0.8; OVERWRITE_KEYS=200
    ;;
  baseline)
    DURATION=60s; RATE=500; SEED=1000; COLD_SEED=3000
    SIZES="1024,1048576,104857600"
    RAMP_FROM=200; RAMP_TO=2000; RAMP_STEP=200
    MPU_CONCURRENCY=10; MPU_PART_COUNT=5; MPU_PART_SIZE=5242880
    MAX_ERROR_RATE=0.01
    COMPRESSIBLE=0.8; OVERWRITE_KEYS=1000
    ;;
  saturation)
    DURATION=30s; RATE=200; SEED=1000; COLD_SEED=3000
    SIZES="1048576"
    RAMP_FROM=200; RAMP_TO=5000; RAMP_STEP=200
    MPU_CONCURRENCY=20; MPU_PART_COUNT=5; MPU_PART_SIZE=5242880
    MAX_ERROR_RATE=0.01
    COMPRESSIBLE=0.8; OVERWRITE_KEYS=1000
    ;;
  *)
    echo "unknown profile: $PROFILE (expected smoke|baseline|saturation)" >&2
    exit 2
    ;;
esac

# Auto-resolve the keypair the scenarios sign with. The demo writes one when it
# provisions its perf identity, so a suite run against it goes through a stored
# user and a stored grant rather than the config credential, which carries full
# access and would skip the permission check the request path is being measured
# with.
#
# Falls back to the config credential so a run against a deployment that has not
# provisioned one still works.
#
# The same file carries the root keypair, which the cold-read scenario signs its
# cache-flush call with. Whatever the caller states in the environment wins over
# what was found lying around, so the overrides are captured before the file is
# sourced and put back after.
PERF_CREDENTIALS="${PERF_CREDENTIALS:-deploy/local/.perf-credentials.env}"
override_access="${PERF_ACCESS_KEY:-}"
override_secret="${PERF_SECRET_KEY:-}"
override_admin_key="${S3O_ACCESS_KEY_ID:-}"
override_admin_secret="${S3O_SECRET_ACCESS_KEY:-}"
if [[ -f "$PERF_CREDENTIALS" ]]; then
  # shellcheck source=/dev/null
  source "$PERF_CREDENTIALS"
fi
ACCESS_KEY="${override_access:-${PERF_ACCESS_KEY:-photoskey}}"
SECRET_KEY="${override_secret:-${PERF_SECRET_KEY:-photossecret}}"
ADMIN_ACCESS_KEY="${override_admin_key:-${S3O_ACCESS_KEY_ID:-}}"
ADMIN_SECRET_KEY="${override_admin_secret:-${S3O_SECRET_ACCESS_KEY:-}}"

# Say where the admin keypair came from. A shell that exports S3O_* for another
# deployment silently overrides the demo's file, and the only symptom is a 401
# on the cold-read scenario's cache flush.
if [[ -n "$override_admin_key" ]]; then
  ADMIN_SOURCE="environment"
elif [[ -n "$ADMIN_ACCESS_KEY" ]]; then
  ADMIN_SOURCE="$PERF_CREDENTIALS"
else
  ADMIN_SOURCE="none"
fi

mkdir -p "$RESULTS_DIR"
echo "profile=$PROFILE results=$RESULTS_DIR endpoint=$ENDPOINT bucket=$BUCKET access_key=$ACCESS_KEY"
echo "admin_key=${ADMIN_ACCESS_KEY:-none} (from $ADMIN_SOURCE)"
if [[ -n "$override_admin_key" && -n "${S3O_ACCESS_KEY_ID:-}" && "$override_admin_key" != "$S3O_ACCESS_KEY_ID" ]]; then
  echo "warning: S3O_ACCESS_KEY_ID in the environment overrides the admin keypair in"
  echo "         $PERF_CREDENTIALS; unset S3O_ACCESS_KEY_ID and S3O_SECRET_ACCESS_KEY"
  echo "         to sign as the demo's root keypair"
fi
echo

# Run each scenario, recording PASS/FAIL by exit code. A non-zero exit
# from the binary marks the scenario red and continues to the next so
# operators see the full failure surface in one run.
declare -A STATUS

# The credential flags every Go scenario signs with, spelled once so a scenario
# cannot be added that quietly falls back to the binary's built-in default.
CREDS=(-access-key "$ACCESS_KEY" -secret-key "$SECRET_KEY")

run_scenario() {
  local name="$1"; shift
  echo "=== $name ==="
  if "$@" 2>&1 | tee "$RESULTS_DIR/$name.log"; then
    STATUS[$name]=PASS
  else
    STATUS[$name]=FAIL
  fi
  echo
}

run_scenario "put-sweep" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" "${CREDS[@]}" \
  -op put -rate "$RATE" -duration "$DURATION" \
  -sizes "$SIZES" -max-error-rate "$MAX_ERROR_RATE" \
  -compressible "$COMPRESSIBLE" \
  -output-json "$RESULTS_DIR/put-sweep.json"

run_scenario "get-warm" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" "${CREDS[@]}" \
  -op get -rate "$RATE" -duration "$DURATION" \
  -sizes "$SIZES" -seed "$SEED" -max-error-rate "$MAX_ERROR_RATE" \
  -output-json "$RESULTS_DIR/get-warm.json"

if [[ -n "$ADMIN_ACCESS_KEY" && -n "$ADMIN_SECRET_KEY" ]]; then
  run_scenario "get-cold" "$BINARY" \
    -endpoint "$ENDPOINT" -bucket "$BUCKET" "${CREDS[@]}" \
    -op get -rate "$RATE" -duration "$DURATION" \
    -sizes "$SIZES" -seed "$COLD_SEED" -cold \
    -cache-flush-before \
    -admin-access-key "$ADMIN_ACCESS_KEY" -admin-secret-key "$ADMIN_SECRET_KEY" \
    -max-error-rate "$MAX_ERROR_RATE" \
    -output-json "$RESULTS_DIR/get-cold.json"
else
  echo "=== get-cold ==="
  echo "skipped: no admin keypair (set S3O_ACCESS_KEY_ID and S3O_SECRET_ACCESS_KEY)"
  STATUS["get-cold"]=SKIP
  echo
fi

run_scenario "mixed-ramp" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" "${CREDS[@]}" \
  -op mixed -rate "$RAMP_FROM" -ramp-to "$RAMP_TO" -ramp-step "$RAMP_STEP" \
  -duration "$DURATION" -seed "$SEED" -compressible "$COMPRESSIBLE" \
  -output-json "$RESULTS_DIR/mixed-ramp.json"

# Rewrites a bounded key set, so the run exercises overwrite displacement, the
# intent supersession it triggers, and - with write fan-out on - a new write
# arriving for a key whose previous copies are still being placed. Every other
# scenario writes unique keys and never reaches any of it.
run_scenario "overwrite" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" "${CREDS[@]}" \
  -op overwrite -rate "$RATE" -duration "$DURATION" \
  -sizes "$SIZES" -overwrite-keys "$OVERWRITE_KEYS" \
  -compressible "$COMPRESSIBLE" -max-error-rate "$MAX_ERROR_RATE" \
  -output-json "$RESULTS_DIR/overwrite.json"

run_scenario "list" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" "${CREDS[@]}" \
  -op listobjects -rate "$RATE" -duration "$DURATION" \
  -seed "$SEED" -max-error-rate "$MAX_ERROR_RATE" \
  -output-json "$RESULTS_DIR/list.json"

if command -v k6 >/dev/null; then
  # Pin the creds explicitly: multipart.js reads AWS_* from __ENV, so without
  # these it would inherit whatever AWS_ACCESS_KEY_ID/SECRET are exported in the
  # caller's shell and sign with the wrong key (-> 403). The Go scenarios don't
  # hit this because they take creds as flags, not from the environment.
  # k6 --env wins over inherited OS env, so these override any ambient AWS_*.
  run_scenario "multipart" k6 run loadtest/k6/multipart.js \
    --env "S3_ENDPOINT=$ENDPOINT" --env "S3_BUCKET=$BUCKET" \
    --env "AWS_ACCESS_KEY_ID=$ACCESS_KEY" \
    --env "AWS_SECRET_ACCESS_KEY=$SECRET_KEY" \
    --env "AWS_REGION=us-east-1" \
    --env "CONCURRENCY=$MPU_CONCURRENCY" \
    --env "PART_COUNT=$MPU_PART_COUNT" \
    --env "PART_SIZE=$MPU_PART_SIZE"
  # k6 exits 0 when its thresholds pass, even if zero iterations completed
  # (a previous bug: O(n^2) body builder deadlocked every VU before any
  # request fired). Force a FAIL when the iteration count is zero so the
  # summary reflects reality.
  if grep -qE "0 complete and [0-9]+ interrupted iterations" "$RESULTS_DIR/multipart.log" \
      && ! grep -qE "[1-9][0-9]* complete and" "$RESULTS_DIR/multipart.log"; then
    echo "multipart: zero iterations completed -- marking FAIL"
    STATUS["multipart"]=FAIL
  fi
else
  echo "=== multipart ==="
  echo "skipped: k6 is not installed (https://grafana.com/docs/k6/latest/set-up/install-k6/)"
  STATUS["multipart"]=SKIP
  echo
fi

echo "============================================================"
echo "Summary [$PROFILE profile, $RESULTS_DIR]"
echo "============================================================"
fail=0
for s in put-sweep get-warm get-cold mixed-ramp overwrite list multipart; do
  v="${STATUS[$s]:-MISSING}"
  printf "  %-12s %s\n" "$s" "$v"
  [[ "$v" == FAIL ]] && fail=1
done
echo

if [[ "$fail" -eq 1 ]]; then
  echo "one or more scenarios FAILED - check $RESULTS_DIR/*.log"
  exit 1
fi
echo "all scenarios passed (skipped = optional dependencies missing)"
