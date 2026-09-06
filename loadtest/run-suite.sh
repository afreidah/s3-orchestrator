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

# Auto-resolve admin token: env var wins, else look for a local config.
if [[ -z "${S3O_ADMIN_TOKEN:-}" ]]; then
  for cfg in config.yaml /etc/s3-orchestrator/config.yaml; do
    if [[ -f "$cfg" ]] && command -v yq >/dev/null; then
      tok=$(yq -r '.ui.admin_token // .ui.admin_key // ""' "$cfg" 2>/dev/null || echo "")
      if [[ -n "$tok" && "$tok" != "null" ]]; then
        S3O_ADMIN_TOKEN="$tok"
        break
      fi
    fi
  done
fi

mkdir -p "$RESULTS_DIR"
echo "profile=$PROFILE results=$RESULTS_DIR endpoint=$ENDPOINT bucket=$BUCKET"
echo

# Run each scenario, recording PASS/FAIL by exit code. A non-zero exit
# from the binary marks the scenario red and continues to the next so
# operators see the full failure surface in one run.
declare -A STATUS

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
  -endpoint "$ENDPOINT" -bucket "$BUCKET" \
  -op put -rate "$RATE" -duration "$DURATION" \
  -sizes "$SIZES" -max-error-rate "$MAX_ERROR_RATE" \
  -compressible "$COMPRESSIBLE" \
  -output-json "$RESULTS_DIR/put-sweep.json"

run_scenario "get-warm" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" \
  -op get -rate "$RATE" -duration "$DURATION" \
  -sizes "$SIZES" -seed "$SEED" -max-error-rate "$MAX_ERROR_RATE" \
  -output-json "$RESULTS_DIR/get-warm.json"

if [[ -n "${S3O_ADMIN_TOKEN:-}" ]]; then
  run_scenario "get-cold" "$BINARY" \
    -endpoint "$ENDPOINT" -bucket "$BUCKET" \
    -op get -rate "$RATE" -duration "$DURATION" \
    -sizes "$SIZES" -seed "$COLD_SEED" -cold \
    -cache-flush-before -admin-token "$S3O_ADMIN_TOKEN" \
    -max-error-rate "$MAX_ERROR_RATE" \
    -output-json "$RESULTS_DIR/get-cold.json"
else
  echo "=== get-cold ==="
  echo "skipped: S3O_ADMIN_TOKEN not set and no local config.yaml found"
  STATUS["get-cold"]=SKIP
  echo
fi

run_scenario "mixed-ramp" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" \
  -op mixed -rate "$RAMP_FROM" -ramp-to "$RAMP_TO" -ramp-step "$RAMP_STEP" \
  -duration "$DURATION" -seed "$SEED" -compressible "$COMPRESSIBLE" \
  -output-json "$RESULTS_DIR/mixed-ramp.json"

# Rewrites a bounded key set, so the run exercises overwrite displacement, the
# intent supersession it triggers, and - with write fan-out on - a new write
# arriving for a key whose previous copies are still being placed. Every other
# scenario writes unique keys and never reaches any of it.
run_scenario "overwrite" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" \
  -op overwrite -rate "$RATE" -duration "$DURATION" \
  -sizes "$SIZES" -overwrite-keys "$OVERWRITE_KEYS" \
  -compressible "$COMPRESSIBLE" -max-error-rate "$MAX_ERROR_RATE" \
  -output-json "$RESULTS_DIR/overwrite.json"

run_scenario "list" "$BINARY" \
  -endpoint "$ENDPOINT" -bucket "$BUCKET" \
  -op listobjects -rate "$RATE" -duration "$DURATION" \
  -seed "$SEED" -max-error-rate "$MAX_ERROR_RATE" \
  -output-json "$RESULTS_DIR/list.json"

if command -v k6 >/dev/null; then
  # Pin the demo creds explicitly: multipart.js reads AWS_* from __ENV, so
  # without these it would inherit whatever AWS_ACCESS_KEY_ID/SECRET are exported
  # in the caller's shell and sign with the wrong key (-> 403). The Go scenarios
  # don't hit this because they default creds via flags, not the environment.
  # k6 --env wins over inherited OS env, so these override any ambient AWS_*.
  run_scenario "multipart" k6 run loadtest/k6/multipart.js \
    --env "S3_ENDPOINT=$ENDPOINT" --env "S3_BUCKET=$BUCKET" \
    --env "AWS_ACCESS_KEY_ID=photoskey" \
    --env "AWS_SECRET_ACCESS_KEY=photossecret" \
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
