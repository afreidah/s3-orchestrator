// -------------------------------------------------------------------------------
// S3 Orchestrator - k6 Read Burst Load Test
//
// Seeds a pool of objects during setup, then hammers them with GET requests at
// high concurrency. Useful for testing read-heavy workloads, object data cache
// effectiveness, and backend egress/API quota pressure.
//
// Usage:
//   k6 run burst-read.js
//   k6 run burst-read.js --env PEAK_VUS=200 --env SEED_COUNT=50
//   k6 run burst-read.js --env HOLD_DURATION=60s --env OBJECT_SIZE=65536
// -------------------------------------------------------------------------------

import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Rate, Trend } from "k6/metrics";
import { SharedArray } from "k6/data";
import crypto from "k6/crypto";
import exec from "k6/execution";

// -- Configuration (override via --env) -----------------------------------

const ENDPOINT = __ENV.S3_ENDPOINT || "http://localhost:9000";
const BUCKET = __ENV.S3_BUCKET || "photos";
const ACCESS_KEY = __ENV.AWS_ACCESS_KEY_ID || "photoskey";
const SECRET_KEY = __ENV.AWS_SECRET_ACCESS_KEY || "photossecret";
const REGION = __ENV.AWS_REGION || "us-east-1";
const PEAK_VUS = parseInt(__ENV.PEAK_VUS || "100");
const SEED_COUNT = parseInt(__ENV.SEED_COUNT || "20");
const OBJECT_SIZE = parseInt(__ENV.OBJECT_SIZE || "4096");
const HOLD_DURATION = __ENV.HOLD_DURATION || "30s";

// -- Stages ---------------------------------------------------------------

export const options = {
  scenarios: {
    burst_reads: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: "2s", target: PEAK_VUS },
        { duration: HOLD_DURATION, target: PEAK_VUS },
        { duration: "2s", target: 0 },
      ],
    },
  },
  thresholds: {
    http_req_duration: ["p(50)<5000"],
  },
};

// -- Custom metrics -------------------------------------------------------

const getSuccess = new Rate("get_success");
const getLatency = new Trend("get_latency", true);
const shedCount = new Counter("shed_503");
const notFound = new Counter("not_found_404");

// -- SigV4 signing --------------------------------------------------------

function hmacSHA256(key, msg) {
  return crypto.hmac("sha256", key, msg, "binary");
}

function sha256Hex(data) {
  return crypto.sha256(data, "hex");
}

function getSignatureKey(secretKey, dateStamp, region, service) {
  const kDate = hmacSHA256("AWS4" + secretKey, dateStamp);
  const kRegion = hmacSHA256(kDate, region);
  const kService = hmacSHA256(kRegion, service);
  return hmacSHA256(kService, "aws4_request");
}

function signRequest(method, path, body, headers) {
  const now = new Date();
  const amzDate = now.toISOString().replace(/[:-]|\.\d{3}/g, "");
  const dateStamp = amzDate.substring(0, 8);

  const payloadHash = "UNSIGNED-PAYLOAD";

  headers["x-amz-date"] = amzDate;
  headers["x-amz-content-sha256"] = payloadHash;
  headers["host"] = ENDPOINT.replace(/^https?:\/\//, "");

  const signedHeaderKeys = Object.keys(headers).sort();
  const canonicalHeaders = signedHeaderKeys
    .map((k) => k.toLowerCase() + ":" + headers[k].trim())
    .join("\n");
  const signedHeaders = signedHeaderKeys.map((k) => k.toLowerCase()).join(";");

  const canonicalRequest = [
    method,
    "/" + path,
    "",
    canonicalHeaders + "\n",
    signedHeaders,
    payloadHash,
  ].join("\n");

  const credentialScope = [dateStamp, REGION, "s3", "aws4_request"].join("/");
  const stringToSign = [
    "AWS4-HMAC-SHA256",
    amzDate,
    credentialScope,
    sha256Hex(canonicalRequest),
  ].join("\n");

  const signingKey = getSignatureKey(SECRET_KEY, dateStamp, REGION, "s3");
  const signature = crypto.hmac("sha256", signingKey, stringToSign, "hex");

  headers[
    "Authorization"
  ] = `AWS4-HMAC-SHA256 Credential=${ACCESS_KEY}/${credentialScope}, SignedHeaders=${signedHeaders}, Signature=${signature}`;

  delete headers["host"];
  return headers;
}

// -- Helpers --------------------------------------------------------------

function randomBytes(n) {
  const chars = "abcdefghijklmnopqrstuvwxyz0123456789";
  let s = "";
  for (let i = 0; i < n; i++) {
    s += chars.charAt(Math.floor(Math.random() * chars.length));
  }
  return s;
}

// Pre-generate the list of seed keys so all VUs read from the same pool.
const seedKeys = new SharedArray("seed-keys", function () {
  const keys = [];
  for (let i = 0; i < SEED_COUNT; i++) {
    keys.push(`loadtest/burst-read/obj-${String(i).padStart(4, "0")}`);
  }
  return keys;
});

// -- Setup: seed objects --------------------------------------------------

export function setup() {
  console.log(
    `Seeding ${SEED_COUNT} objects (${OBJECT_SIZE}B each) into ${BUCKET}...`
  );
  const body = randomBytes(OBJECT_SIZE);

  for (let i = 0; i < SEED_COUNT; i++) {
    const key = seedKeys[i];
    const path = `${BUCKET}/${key}`;
    const headers = { "content-type": "application/octet-stream" };
    const signed = signRequest("PUT", path, body, headers);
    const res = http.put(`${ENDPOINT}/${path}`, body, { headers: signed });
    if (res.status !== 200) {
      console.error(`Seed PUT failed for ${key}: ${res.status}`);
    }
  }
  console.log("Seeding complete.");
}

// -- Main scenario: burst reads -------------------------------------------

export default function () {
  // Pick a random seeded object
  const key = seedKeys[Math.floor(Math.random() * seedKeys.length)];
  const path = `${BUCKET}/${key}`;
  const headers = {};
  const signed = signRequest("GET", path, null, headers);

  const res = http.get(`${ENDPOINT}/${path}`, { headers: signed });

  getLatency.add(res.timings.duration);

  if (res.status === 503) {
    shedCount.add(1);
    getSuccess.add(false);
  } else if (res.status === 404) {
    notFound.add(1);
    getSuccess.add(false);
  } else {
    getSuccess.add(res.status === 200);
    check(res, { "GET 200": (r) => r.status === 200 });
  }

  sleep(0.05);
}

// -- Teardown: clean up seeded objects ------------------------------------

export function teardown() {
  console.log("Cleaning up seeded objects...");
  for (let i = 0; i < SEED_COUNT; i++) {
    const key = seedKeys[i];
    const path = `${BUCKET}/${key}`;
    const headers = {};
    const signed = signRequest("DELETE", path, null, headers);
    http.del(`${ENDPOINT}/${path}`, null, { headers: signed });
  }
  console.log("Cleanup complete.");
}
