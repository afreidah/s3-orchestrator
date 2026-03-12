// -------------------------------------------------------------------------------
// S3 Orchestrator - k6 Mixed Workflow Load Test
//
// Simulates realistic user traffic: upload objects, list bucket contents,
// download random objects, then clean up. Uses staged VU ramp-up/down.
//
// Usage:
//   k6 run mixed.js
//   k6 run mixed.js --env S3_ENDPOINT=http://s3.example.com:9000
//   k6 run mixed.js --env OBJECT_COUNT=50 --env OBJECT_SIZE=8192
// -------------------------------------------------------------------------------

import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend } from "k6/metrics";
import { SharedArray } from "k6/data";
import crypto from "k6/crypto";

// -- Configuration (override via --env) -----------------------------------

const ENDPOINT = __ENV.S3_ENDPOINT || "http://localhost:9000";
const BUCKET = __ENV.S3_BUCKET || "photos";
const ACCESS_KEY = __ENV.AWS_ACCESS_KEY_ID || "photoskey";
const SECRET_KEY = __ENV.AWS_SECRET_ACCESS_KEY || "photossecret";
const REGION = __ENV.AWS_REGION || "us-east-1";
const OBJECT_COUNT = parseInt(__ENV.OBJECT_COUNT || "20");
const OBJECT_SIZE = parseInt(__ENV.OBJECT_SIZE || "1024");

// -- Stages ---------------------------------------------------------------

export const options = {
  stages: [
    { duration: "10s", target: 10 }, // ramp up
    { duration: "30s", target: 10 }, // hold steady
    { duration: "10s", target: 0 }, // ramp down
  ],
  thresholds: {
    http_req_failed: ["rate<0.05"],
    http_req_duration: ["p(95)<2000"],
  },
};

// -- Custom metrics -------------------------------------------------------

const putSuccess = new Rate("put_success");
const getSuccess = new Rate("get_success");
const putLatency = new Trend("put_latency", true);
const getLatency = new Trend("get_latency", true);

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

  // Canonical headers (sorted)
  const signedHeaderKeys = Object.keys(headers).sort();
  const canonicalHeaders = signedHeaderKeys
    .map((k) => k.toLowerCase() + ":" + headers[k].trim())
    .join("\n");
  const signedHeaders = signedHeaderKeys.map((k) => k.toLowerCase()).join(";");

  const canonicalRequest = [
    method,
    "/" + path,
    "", // query string
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

  // Remove host header — k6 sets it from the URL.
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

function putObject(key, body) {
  const path = `${BUCKET}/${key}`;
  const headers = { "content-type": "application/octet-stream" };
  const signed = signRequest("PUT", path, body, headers);
  return http.put(`${ENDPOINT}/${path}`, body, { headers: signed });
}

function getObject(key) {
  const path = `${BUCKET}/${key}`;
  const headers = {};
  const signed = signRequest("GET", path, null, headers);
  return http.get(`${ENDPOINT}/${path}`, { headers: signed });
}

function deleteObject(key) {
  const path = `${BUCKET}/${key}`;
  const headers = {};
  const signed = signRequest("DELETE", path, null, headers);
  return http.del(`${ENDPOINT}/${path}`, null, { headers: signed });
}

// -- Main scenario --------------------------------------------------------

export default function () {
  const vuId = __VU;
  const iter = __ITER;
  const prefix = `loadtest/k6-${vuId}-${iter}`;
  const body = randomBytes(OBJECT_SIZE);
  const keys = [];

  // Upload objects
  for (let i = 0; i < OBJECT_COUNT; i++) {
    const key = `${prefix}/obj-${String(i).padStart(4, "0")}`;
    const res = putObject(key, body);
    putSuccess.add(res.status === 200);
    putLatency.add(res.timings.duration);
    check(res, { "PUT 200": (r) => r.status === 200 });
    keys.push(key);
  }

  sleep(0.5);

  // Download random objects
  const downloads = Math.min(OBJECT_COUNT, 10);
  for (let i = 0; i < downloads; i++) {
    const key = keys[Math.floor(Math.random() * keys.length)];
    const res = getObject(key);
    getSuccess.add(res.status === 200);
    getLatency.add(res.timings.duration);
    check(res, { "GET 200": (r) => r.status === 200 });
  }

  sleep(0.5);

  // Clean up
  for (const key of keys) {
    deleteObject(key);
  }
}
