// -------------------------------------------------------------------------------
// Cloudflare Bandwidth Alliance Edge Proxy
//
// Author: Alex Freidah
//
// Fronts a Bandwidth Alliance backend so object egress leaves the provider to
// Cloudflare rather than to the client, which the alliance zero-rates. Verifies
// the caller against a proxy credential, then re-signs the request against the
// origin's own host with the account credential.
//
// Cloudflare forwards the client's Host, so an origin behind a proxied CNAME
// sees the proxy hostname instead of its own endpoint. That breaks bucket
// routing, and because host is a signed header under SigV4, the signature with
// it. Re-signing at the edge is what makes the hop transparent to the origin.
// -------------------------------------------------------------------------------

import { ProxyError, errorResponse } from "./errors";
import {
  UNSIGNED_PAYLOAD,
  amzDate,
  assertFresh,
  authorizationHeader,
  computeSignature,
  parseAuthorization,
  splitTarget,
  timingSafeEqual,
} from "./sigv4";

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

/**
 * Bindings the worker reads at runtime, supplied by wrangler as vars and
 * secrets rather than compiled in.
 *
 * A TypeScript `interface` is a compile-time shape that leaves no trace in the
 * emitted JavaScript, so it constrains nothing at runtime: a binding left
 * unset surfaces as `undefined` during a request, not as a startup failure.
 *
 * The proxy credential is deliberately separate from the account credential.
 * It can be rotated without touching the backend, and a caller that obtains it
 * still cannot reach the origin directly.
 */
export interface Env {
  ORIGIN_HOST: string;
  ORIGIN_REGION: string;
  ORIGIN_ACCESS_KEY_ID: string;
  ORIGIN_SECRET_ACCESS_KEY: string;
  PROXY_ACCESS_KEY_ID: string;
  PROXY_SECRET_ACCESS_KEY: string;
  MAX_CLOCK_SKEW_SECONDS?: string; // optional, defaults to 300
}

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

const DEFAULT_MAX_CLOCK_SKEW_SECONDS = 300;

/**
 * Headers describing a single network hop, plus the metadata Cloudflare
 * attaches inbound. Forwarding them is either meaningless to the origin or
 * actively wrong: `transfer-encoding` describes the framing of a connection
 * that terminates at this worker.
 *
 * A Set gives constant-time membership rather than a scan per header.
 */
const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
  "cf-connecting-ip",
  "cf-ipcountry",
  "cf-ray",
  "cf-visitor",
  "cf-worker",
  "x-forwarded-for",
  "x-forwarded-proto",
  "x-real-ip",
]);

/**
 * Headers the origin is expected to honor, and so must sign. A fixed list
 * would leave `x-amz-tagging`, `x-amz-meta-*`, ACL, and server-side encryption
 * headers unsigned, which lets them be altered in flight without invalidating
 * the signature.
 */
function isSignedHeader(name: string): boolean {
  return name === "host" ||
    name === "content-type" ||
    name === "content-md5" ||
    name.startsWith("x-amz-");
}

// -------------------------------------------------------------------------
// ENTRY POINT
// -------------------------------------------------------------------------

/**
 * The worker.
 *
 * A Workers module exports a default object whose `fetch` method runs once per
 * inbound request. Every Web Crypto primitive is asynchronous, which is why
 * the handler and most of the signing path are too.
 *
 * Verification runs first, so a caller that fails it never reaches the code
 * holding the account credential.
 */
export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    try {
      await verifyCaller(request, env);
      return await forward(request, env);
    } catch (err) {
      return errorResponse(err);
    }
  },
};

// -------------------------------------------------------------------------
// VERIFICATION
// -------------------------------------------------------------------------

/**
 * Recomputes the caller's signature under the proxy credential and compares it
 * against the one presented.
 *
 * Without this the worker signs whatever reaches it with the account key,
 * which makes the bucket readable and writable by anyone who can resolve the
 * hostname.
 *
 * Verification mirrors signing: the canonical request is rebuilt from the
 * caller's own inputs, over the host they addressed, and the results compared.
 * The body is never read, since the payload hash arrives as a header the
 * caller already signed.
 */
export async function verifyCaller(request: Request, env: Env): Promise<void> {
  const header = request.headers.get("authorization");
  if (!header) {
    // A presigned URL carries its signature in the query string and needs a
    // separate verification path. Naming the gap beats a misleading
    // AccessDenied on a request that is well formed.
    if (new URL(request.url).searchParams.has("X-Amz-Signature")) {
      throw new ProxyError(501, "NotImplemented", "Presigned URL authentication is not supported.");
    }
    throw new ProxyError(403, "AccessDenied", "Missing Authorization header.");
  }

  const parsed = parseAuthorization(header);
  if (!timingSafeEqual(parsed.accessKeyId, env.PROXY_ACCESS_KEY_ID)) {
    throw new ProxyError(403, "InvalidAccessKeyId", "Unknown access key.");
  }

  const datetime = request.headers.get("x-amz-date");
  if (!datetime) throw new ProxyError(403, "AccessDenied", "Missing x-amz-date header.");
  assertFresh(datetime, skewWindow(env));

  // The scope is covered by the signature, but the region within it also
  // steers key derivation, so it is reconciled against the timestamp before
  // any of it is used.
  const [scopeDate, scopeRegion, scopeService] = parsed.credentialScope.split("/");
  if (scopeDate !== datetime.slice(0, 8) || scopeService !== "s3" || !scopeRegion) {
    throw new ProxyError(403, "AccessDenied", "Credential scope does not match the request.");
  }

  const { rawPath, canonicalQuery } = splitTarget(request.url);
  const expected = await computeSignature({
    method: request.method,
    rawPath,
    canonicalQuery,
    headers: request.headers,
    signedNames: parsed.signedHeaders,
    payloadHash: request.headers.get("x-amz-content-sha256") ?? UNSIGNED_PAYLOAD,
    datetime,
    region: scopeRegion,
    secretAccessKey: env.PROXY_SECRET_ACCESS_KEY,
  });

  if (!timingSafeEqual(expected, parsed.signature)) {
    throw new ProxyError(403, "SignatureDoesNotMatch", "Signature does not match the request.");
  }
}

/** Replay window in seconds, falling back when the binding is unset or junk. */
function skewWindow(env: Env): number {
  const parsed = Number(env.MAX_CLOCK_SKEW_SECONDS);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_MAX_CLOCK_SKEW_SECONDS;
}

// -------------------------------------------------------------------------
// FORWARDING
// -------------------------------------------------------------------------

/**
 * Retargets a verified request at the origin and signs it with the account
 * credential.
 *
 * Path, query, and body carry over untouched. The body stays a ReadableStream
 * so an object passes through without being buffered in the worker, which
 * matters both for memory and for the request size ceiling.
 */
export async function forward(request: Request, env: Env): Promise<Response> {
  // The caller's hash covers a body that is forwarded unmodified, so the same
  // value describes the outbound request.
  const payloadHash = request.headers.get("x-amz-content-sha256") ?? UNSIGNED_PAYLOAD;
  if (payloadHash.startsWith("STREAMING-")) {
    throw new ProxyError(
      501,
      "NotImplemented",
      "Chunked payload signatures embed per-chunk signatures under the caller's key, " +
        "which cannot be re-signed without buffering. Send an unsigned or fully hashed payload.",
    );
  }

  const originUrl = new URL(request.url);
  originUrl.protocol = "https:";
  originUrl.hostname = env.ORIGIN_HOST;
  originUrl.port = "";

  // Rebuilt rather than mutated, dropping the caller's Authorization: it is
  // scoped to the proxy credential and the proxy hostname, so the origin
  // rejects it.
  const headers = new Headers();
  for (const [name, value] of request.headers) {
    const lower = name.toLowerCase();
    if (HOP_BY_HOP.has(lower) || lower === "authorization" || lower === "host") continue;
    headers.set(name, value);
  }
  headers.set("host", env.ORIGIN_HOST);

  // A fresh timestamp, because the outbound request is a new one as far as the
  // origin's own replay window is concerned.
  const datetime = amzDate(new Date());
  headers.set("x-amz-date", datetime);
  headers.set("x-amz-content-sha256", payloadHash);

  // SigV4 requires this list sorted, and it must match the canonical header
  // block the signature is computed over.
  const signedNames = [...headers.keys()]
    .map((n) => n.toLowerCase())
    .filter(isSignedHeader)
    .sort();

  const { rawPath, canonicalQuery } = splitTarget(request.url);
  headers.set(
    "authorization",
    await authorizationHeader({
      method: request.method,
      rawPath,
      canonicalQuery,
      headers,
      signedNames,
      payloadHash,
      datetime,
      region: env.ORIGIN_REGION,
      accessKeyId: env.ORIGIN_ACCESS_KEY_ID,
      secretAccessKey: env.ORIGIN_SECRET_ACCESS_KEY,
    }),
  );

  return fetch(originUrl.toString(), {
    method: request.method,
    headers,
    body: request.body,
    // Following a redirect replays a request signed for this host against a
    // different one.
    redirect: "manual",
  });
}
