// -------------------------------------------------------------------------------
// S3 Orchestrator - k6 Burst / Admission Control Load Test
//
// Validates load shedding and max_concurrent_requests behavior under sudden
// traffic spikes. Ramps from 0 to a high VU count quickly, holds, then
// drops to zero. Expects 503 SlowDown responses when limits are hit.
//
// Usage:
//   k6 run burst.js
//   k6 run burst.js --env PEAK_VUS=200 --env OBJECT_SIZE=65536
// -------------------------------------------------------------------------------

import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Rate, Trend } from "k6/metrics";
import crypto from "k6/crypto";

// -- Configuration (override via --env) -----------------------------------

const ENDPOINT = __ENV.S3_ENDPOINT || "http://localhost:9000";
const BUCKET = __ENV.S3_BUCKET || "photos";
const ACCESS_KEY = __ENV.AWS_ACCESS_KEY_ID || "photoskey";
const SECRET_KEY = __ENV.AWS_SECRET_ACCESS_KEY || "photossecret";
const REGION = __ENV.AWS_REGION || "us-east-1";
const PEAK_VUS = parseInt(__ENV.PEAK_VUS || "100");
const OBJECT_SIZE = parseInt(__ENV.OBJECT_SIZE || "4096");

// -- Stages ---------------------------------------------------------------

export const options = {
  stages: [
    { duration: "2s", target: PEAK_VUS }, // sudden spike
    { duration: "20s", target: PEAK_VUS }, // hold at peak
    { duration: "2s", target: 0 }, // drop
  ],
  // No failure threshold — we expect 503s under load.
  thresholds: {
    http_req_duration: ["p(50)<5000"],
  },
};

// -- Custom metrics -------------------------------------------------------

const shedCount = new Counter("shed_503");
const putSuccess = new Rate("put_success");
const putLatency = new Trend("put_latency", true);

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

// -- Main scenario --------------------------------------------------------

export default function () {
  const key = `loadtest/burst-${__VU}-${__ITER}-${Date.now()}`;
  const path = `${BUCKET}/${key}`;
  const body = randomBytes(OBJECT_SIZE);
  const headers = { "content-type": "application/octet-stream" };
  const signed = signRequest("PUT", path, body, headers);

  const res = http.put(`${ENDPOINT}/${path}`, body, { headers: signed });

  putLatency.add(res.timings.duration);

  if (res.status === 503) {
    shedCount.add(1);
    putSuccess.add(false);
  } else {
    putSuccess.add(res.status === 200);
    check(res, { "PUT success": (r) => r.status === 200 });
  }

  // Minimal think time to maximize pressure.
  sleep(0.1);
}
