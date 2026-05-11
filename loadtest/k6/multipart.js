// -------------------------------------------------------------------------------
// S3 Orchestrator - k6 Concurrent Multipart Upload Load Test
//
// Drives concurrent multipart uploads at a configurable concurrency
// level. Each VU iteration performs CreateMultipartUpload, N
// UploadPart calls, then CompleteMultipartUpload, recording success
// rate and per-stage latency. Used to characterise multipart throughput
// at increasing concurrency for the perf-envelope writeup (#367).
//
// Usage:
//   k6 run multipart.js
//   k6 run multipart.js --env CONCURRENCY=50 --env PART_COUNT=5 --env PART_SIZE=5242880
// -------------------------------------------------------------------------------

import http from "k6/http";
import { check, sleep } from "k6";
import { Rate, Trend, Counter } from "k6/metrics";
import crypto from "k6/crypto";

// -- Configuration (override via --env) -----------------------------------

const ENDPOINT = __ENV.S3_ENDPOINT || "http://localhost:9000";
const BUCKET = __ENV.S3_BUCKET || "photos";
const ACCESS_KEY = __ENV.AWS_ACCESS_KEY_ID || "photoskey";
const SECRET_KEY = __ENV.AWS_SECRET_ACCESS_KEY || "photossecret";
const REGION = __ENV.AWS_REGION || "us-east-1";
const CONCURRENCY = Number.parseInt(__ENV.CONCURRENCY || "10");
const PART_COUNT = Number.parseInt(__ENV.PART_COUNT || "5");
const PART_SIZE = Number.parseInt(__ENV.PART_SIZE || "5242880"); // 5 MiB minimum for multipart
const HOLD_DURATION = __ENV.HOLD_DURATION || "30s";

// -- Stages ---------------------------------------------------------------

export const options = {
  stages: [
    { duration: "10s", target: CONCURRENCY }, // ramp up to target concurrency
    { duration: HOLD_DURATION, target: CONCURRENCY }, // hold steady
    { duration: "10s", target: 0 }, // ramp down
  ],
  thresholds: {
    http_req_failed: ["rate<0.05"],
    iterations: ["count>0"],
  },
};

// -- Custom metrics -------------------------------------------------------

const createSuccess = new Rate("create_mpu_success");
const partSuccess = new Rate("part_upload_success");
const completeSuccess = new Rate("complete_mpu_success");
const createLatency = new Trend("create_mpu_latency", true);
const partLatency = new Trend("part_upload_latency", true);
const completeLatency = new Trend("complete_mpu_latency", true);
const completedUploads = new Counter("completed_uploads");

// -- SigV4 signing with query-string canonicalisation ---------------------

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

// canonicalQuery returns the SigV4 canonical query string: sorted by
// name, RFC3986-encoded values, "name=value" pairs joined by "&". Each
// pair has exactly one "=" even when the value is empty (matches
// `uploads` -> "uploads=").
function canonicalQuery(params) {
  const keys = Object.keys(params).sort((a, b) => a.localeCompare(b));
  return keys
    .map((k) => `${encodeURIComponent(k)}=${encodeURIComponent(params[k])}`)
    .join("&");
}

// signRequest signs a request with SigV4. queryParams is an object of
// {name: value} pairs; pass empty {} for paths without a query string.
function signRequest(method, path, queryParams, headers) {
  const now = new Date();
  const amzDate = now.toISOString().replace(/[:-]|\.\d{3}/g, "");
  const dateStamp = amzDate.substring(0, 8);
  const payloadHash = "UNSIGNED-PAYLOAD";

  headers["x-amz-date"] = amzDate;
  headers["x-amz-content-sha256"] = payloadHash;
  headers["host"] = ENDPOINT.replace(/^https?:\/\//, "");

  const signedHeaderKeys = Object.keys(headers).sort((a, b) => a.localeCompare(b));
  const canonicalHeaders = signedHeaderKeys
    .map((k) => k.toLowerCase() + ":" + headers[k].trim())
    .join("\n");
  const signedHeaders = signedHeaderKeys.map((k) => k.toLowerCase()).join(";");

  const canonicalRequest = [
    method,
    "/" + path,
    canonicalQuery(queryParams),
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

// PART_BODY is built once per process at module init. UNSIGNED-PAYLOAD
// means the bytes do not need to be random; "x".repeat is O(n) in V8 vs
// the O(n^2) of += concatenation, which previously deadlocked VUs at
// 5 MiB parts (zero iterations completed in 80s).
const PART_BODY = "x".repeat(PART_SIZE);

function buildQueryString(params) {
  return Object.keys(params)
    .map((k) => `${encodeURIComponent(k)}=${encodeURIComponent(params[k])}`)
    .join("&");
}

function createMultipart(key) {
  const path = `${BUCKET}/${key}`;
  const params = { uploads: "" };
  const headers = { "content-type": "application/octet-stream" };
  const signed = signRequest("POST", path, params, headers);
  const url = `${ENDPOINT}/${path}?${buildQueryString(params)}`;
  return http.post(url, "", { headers: signed });
}

function uploadPart(key, uploadId, partNumber, body) {
  const path = `${BUCKET}/${key}`;
  const params = { partNumber: String(partNumber), uploadId };
  const headers = { "content-type": "application/octet-stream" };
  const signed = signRequest("PUT", path, params, headers);
  const url = `${ENDPOINT}/${path}?${buildQueryString(params)}`;
  return http.put(url, body, { headers: signed });
}

function completeMultipart(key, uploadId, parts) {
  const path = `${BUCKET}/${key}`;
  const params = { uploadId };
  const xml =
    "<CompleteMultipartUpload>" +
    parts
      .map(
        (p) =>
          `<Part><PartNumber>${p.partNumber}</PartNumber><ETag>${p.etag}</ETag></Part>`,
      )
      .join("") +
    "</CompleteMultipartUpload>";
  const headers = { "content-type": "application/xml" };
  const signed = signRequest("POST", path, params, headers);
  const url = `${ENDPOINT}/${path}?${buildQueryString(params)}`;
  return http.post(url, xml, { headers: signed });
}

// extractUploadId pulls UploadId out of the XML response body.
function extractUploadId(body) {
  const m = body.match(/<UploadId>([^<]+)<\/UploadId>/);
  return m ? m[1] : null;
}

// extractETag pulls the ETag from the response headers (preferred) or
// the XML body (fallback for completion responses).
function extractETag(res) {
  return res.headers["Etag"] || res.headers["ETag"] || "";
}

// -- Main scenario --------------------------------------------------------

export default function () {
  const vuId = __VU;
  const iter = __ITER;
  const key = `loadtest/multipart-${vuId}-${iter}-${Date.now()}`;

  // 1. Create
  const createRes = createMultipart(key);
  createSuccess.add(createRes.status === 200);
  createLatency.add(createRes.timings.duration);
  if (
    !check(createRes, { "create 200": (r) => r.status === 200 })
  ) {
    return;
  }
  const uploadId = extractUploadId(createRes.body);
  if (!uploadId) {
    return;
  }

  // 2. Upload parts (sequential within a VU; concurrency comes from
  //    parallel VUs configured by --env CONCURRENCY).
  const parts = [];
  for (let i = 1; i <= PART_COUNT; i++) {
    const res = uploadPart(key, uploadId, i, PART_BODY);
    partSuccess.add(res.status === 200);
    partLatency.add(res.timings.duration);
    if (!check(res, { "part 200": (r) => r.status === 200 })) {
      return;
    }
    parts.push({ partNumber: i, etag: extractETag(res) });
  }

  // 3. Complete
  const completeRes = completeMultipart(key, uploadId, parts);
  completeSuccess.add(completeRes.status === 200);
  completeLatency.add(completeRes.timings.duration);
  if (check(completeRes, { "complete 200": (r) => r.status === 200 })) {
    completedUploads.add(1);
  }
}
