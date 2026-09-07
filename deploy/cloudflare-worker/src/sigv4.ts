// -------------------------------------------------------------------------------
// AWS Signature Version 4 - Canonicalization and Signing
//
// Author: Alex Freidah
//
// Builds the canonical request, string to sign, and derived signing key that
// SigV4 defines, for both directions the edge proxy needs: verifying the
// signature a caller presents, and producing the signature the origin accepts.
//
// Both sides of a SigV4 exchange render the request as text independently and
// compare hashes, so any disagreement about normalization surfaces only as
// SignatureDoesNotMatch. Nothing here is cryptographically interesting; the
// weight is in making two renderings agree byte for byte.
// -------------------------------------------------------------------------------

import { ProxyError } from "./errors";

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

export const SERVICE = "s3";
export const ALGORITHM = "AWS4-HMAC-SHA256";
export const UNSIGNED_PAYLOAD = "UNSIGNED-PAYLOAD";

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

/**
 * Everything a signature is computed over.
 *
 * Signing and verifying take an identical set, which is what keeps the two
 * paths from drifting apart: a change to the canonical form is applied once
 * and both directions move together.
 */
export interface SignParams {
  method: string;
  rawPath: string;
  canonicalQuery: string;
  headers: Headers;
  signedNames: string[];
  payloadHash: string;
  datetime: string;
  region: string;
  secretAccessKey: string;
}

/** The four fields an Authorization header carries. */
export interface ParsedAuthorization {
  accessKeyId: string;
  credentialScope: string;
  signedHeaders: string[];
  signature: string;
}

// -------------------------------------------------------------------------
// SIGNING
// -------------------------------------------------------------------------

/**
 * Produces the hex signature for a request.
 *
 * The canonical request joins the method, path, query, the chosen headers as
 * `name:value` lines, those header names again as a semicolon list, and the
 * payload hash, separated by newlines. The header block ends with its own
 * trailing newline, so the join leaves a blank line before the name list; that
 * blank line is part of the format.
 *
 * The string to sign wraps a hash of that text with the algorithm, timestamp,
 * and credential scope. The signing key is HMAC applied four times, each step
 * keyed by the previous result, starting from `AWS4` prefixed to the secret.
 * Chaining through date, region, and service is what scopes a derived key to a
 * single day and endpoint rather than to the account.
 */
export async function computeSignature(p: SignParams): Promise<string> {
  const canonicalHeaders = p.signedNames
    .map((name) => `${name}:${collapse(p.headers.get(name) ?? "")}\n`)
    .join("");

  const canonicalRequest = [
    p.method,
    p.rawPath,
    p.canonicalQuery,
    canonicalHeaders,
    p.signedNames.join(";"),
    p.payloadHash,
  ].join("\n");

  const scopeDate = p.datetime.slice(0, 8);
  const stringToSign = [
    ALGORITHM,
    p.datetime,
    credentialScope(scopeDate, p.region),
    await sha256Hex(canonicalRequest),
  ].join("\n");

  const kDate = await hmac(`AWS4${p.secretAccessKey}`, scopeDate);
  const kRegion = await hmac(kDate, p.region);
  const kService = await hmac(kRegion, SERVICE);
  const kSigning = await hmac(kService, "aws4_request");
  return hmacHex(kSigning, stringToSign);
}

/**
 * Wraps a computed signature in the Authorization header an origin expects.
 *
 * The parameter type is an intersection: an object satisfying SignParams and
 * also carrying an access key id. Signing needs the key id to name itself in
 * the header, while verification reads it from the caller's header instead,
 * which is why it sits here rather than in SignParams.
 */
export async function authorizationHeader(
  p: SignParams & { accessKeyId: string },
): Promise<string> {
  const signature = await computeSignature(p);
  const scope = credentialScope(p.datetime.slice(0, 8), p.region);
  return `${ALGORITHM} Credential=${p.accessKeyId}/${scope}, ` +
    `SignedHeaders=${p.signedNames.join(";")}, Signature=${signature}`;
}

/** Joins the four scope components a credential and string to sign both carry. */
export function credentialScope(scopeDate: string, region: string): string {
  return `${scopeDate}/${region}/${SERVICE}/aws4_request`;
}

// -------------------------------------------------------------------------
// AUTHORIZATION HEADER
// -------------------------------------------------------------------------

/**
 * Pulls the credential, header list, and signature out of an Authorization
 * header shaped like:
 *
 *   AWS4-HMAC-SHA256 Credential=KEYID/20260907/us-west-004/s3/aws4_request,
 *   SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=abc123...
 *
 * The credential runs the key id and the scope together, separated by the
 * first slash, so it splits on index rather than on every slash: the scope
 * contains three more.
 *
 * Every malformed input raises rather than returning a partial result, so an
 * unparseable header cannot be mistaken for an empty but valid one.
 */
export function parseAuthorization(header: string): ParsedAuthorization {
  if (!header.startsWith(`${ALGORITHM} `)) {
    throw new ProxyError(403, "AccessDenied", "Unsupported authorization scheme.");
  }

  const parts = new Map<string, string>();
  for (const chunk of header.slice(ALGORITHM.length + 1).split(",")) {
    // Split on the first separator only: a signature is hex, but a scope and
    // any future field need not be.
    const idx = chunk.indexOf("=");
    if (idx > 0) parts.set(chunk.slice(0, idx).trim(), chunk.slice(idx + 1).trim());
  }

  const credential = parts.get("Credential");
  const signedHeaders = parts.get("SignedHeaders");
  const signature = parts.get("Signature");
  if (!credential || !signedHeaders || !signature) {
    throw new ProxyError(403, "AccessDenied", "Malformed Authorization header.");
  }

  const slash = credential.indexOf("/");
  if (slash <= 0) throw new ProxyError(403, "AccessDenied", "Malformed credential.");

  return {
    accessKeyId: credential.slice(0, slash),
    credentialScope: credential.slice(slash + 1),
    signedHeaders: signedHeaders.split(";").map((h) => h.toLowerCase()),
    signature,
  };
}

/**
 * Rejects a timestamp outside the replay window.
 *
 * A signature stays valid as long as its inputs do, so without a freshness
 * bound a captured request replays indefinitely. The window is two-sided: a
 * clock running ahead is as suspect as one running behind.
 *
 * The format is ISO 8601 basic, 20260907T044132Z, the compact form with
 * separators removed. `Date.UTC` takes a zero-based month, hence the offset,
 * and the unary plus coerces each captured group to a number.
 */
export function assertFresh(datetime: string, maxSkewSeconds: number, now = Date.now()): void {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/.exec(datetime);
  if (!m) throw new ProxyError(403, "AccessDenied", "Malformed x-amz-date.");

  const signed = Date.UTC(+m[1]!, +m[2]! - 1, +m[3]!, +m[4]!, +m[5]!, +m[6]!);
  if (Math.abs(now - signed) > maxSkewSeconds * 1000) {
    throw new ProxyError(403, "RequestTimeTooSkewed", "Request time is outside the accepted window.");
  }
}

// -------------------------------------------------------------------------
// CANONICALIZATION
// -------------------------------------------------------------------------

/**
 * Splits a URL into the path and canonical query a signature covers.
 *
 * Both come off the request line by string index rather than through the URL
 * class. The path arrives already percent-encoded and is canonical as sent, so
 * running it through an encoder turns `%20` into `%2520`; URL additionally
 * normalizes away the dot segments a caller may have signed literally.
 */
export function splitTarget(requestUrl: string): { rawPath: string; canonicalQuery: string } {
  const schemeEnd = requestUrl.indexOf("://");
  const pathStart = requestUrl.indexOf("/", schemeEnd + 3);
  if (pathStart < 0) return { rawPath: "/", canonicalQuery: "" };

  // A fragment never reaches the origin, so it bounds the scan rather than
  // being carried into either component.
  const hashIdx = requestUrl.indexOf("#", pathStart);
  const end = hashIdx < 0 ? requestUrl.length : hashIdx;
  const queryIdx = requestUrl.indexOf("?", pathStart);
  const hasQuery = queryIdx >= 0 && queryIdx < end;

  const rawPath = requestUrl.slice(pathStart, hasQuery ? queryIdx : end);
  const rawQuery = hasQuery ? requestUrl.slice(queryIdx + 1, end) : "";
  return { rawPath: rawPath || "/", canonicalQuery: canonicalizeQuery(rawQuery) };
}

/**
 * Renders a query string in the one form SigV4 accepts: parameters sorted by
 * encoded name and then encoded value, each component re-encoded per RFC 3986,
 * joined with an ampersand, and a valueless parameter written as `name=`.
 *
 * `URLSearchParams.toString` does none of that. It preserves insertion order
 * and renders a space as a plus, so a multipart upload carrying `uploadId` and
 * `partNumber` canonicalizes differently on each side and fails to verify.
 *
 * Each component is decoded before being re-encoded, so input that arrives
 * encoded and input that does not converge on the same output.
 */
export function canonicalizeQuery(rawQuery: string): string {
  if (!rawQuery) return "";

  const pairs: Array<[string, string]> = [];
  for (const part of rawQuery.split("&")) {
    if (!part) continue;
    const eq = part.indexOf("=");
    const name = eq < 0 ? part : part.slice(0, eq);
    const value = eq < 0 ? "" : part.slice(eq + 1);
    pairs.push([rfc3986(decodeURIComponent(name)), rfc3986(decodeURIComponent(value))]);
  }

  // Ordering is on the encoded forms, which is what the specification sorts
  // by. Value is the tiebreak for a name that repeats.
  pairs.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0));
  return pairs.map(([n, v]) => `${n}=${v}`).join("&");
}

/**
 * Percent-encodes a string to RFC 3986, which leaves only the unreserved set
 * `A-Z a-z 0-9 - _ . ~` untouched.
 *
 * `encodeURIComponent` is close but passes `!'()*` through, a holdover from an
 * older URI grammar. All six are legal in an object key, so the gap is
 * reachable rather than theoretical.
 */
export function rfc3986(value: string): string {
  return encodeURIComponent(value).replace(
    /[!'()*]/g,
    (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`,
  );
}

/**
 * Normalizes a header value for the canonical block: outer whitespace removed,
 * internal runs collapsed to a single space.
 *
 * HTTP considers `a:  b` and `a: b` the same header, so without this the two
 * sides can hash different text for a request they both read identically.
 */
export function collapse(value: string): string {
  return value.trim().replace(/\s+/g, " ");
}

/**
 * Formats an instant as ISO 8601 basic, 20260907T044132Z.
 *
 * `toISOString` yields the extended form with separators and milliseconds,
 * neither of which SigV4 carries. The result is always UTC, since
 * `toISOString` has no local-time variant.
 */
export function amzDate(now: Date): string {
  return now.toISOString().replace(/[:-]|\.\d{3}/g, "");
}

// -------------------------------------------------------------------------
// CRYPTO
// -------------------------------------------------------------------------

/**
 * One shared encoder. `TextEncoder` converts a string to UTF-8 bytes, the only
 * input Web Crypto accepts. The instance holds no state, so it is built once
 * rather than per call.
 */
const encoder = new TextEncoder();

/**
 * HMAC-SHA256 over `data` keyed by `key`, returning raw bytes.
 *
 * The key is a string only for the first link of the derivation chain, where
 * it is `AWS4` prefixed to the secret; every later link keys on the previous
 * link's bytes, which is why the parameter is a union.
 *
 * `importKey` is required because Web Crypto does not accept raw bytes as a
 * key directly: it wants the algorithm and the permitted operations stated up
 * front. The `false` marks the key non-extractable, so it cannot be read back.
 */
export async function hmac(key: string | ArrayBuffer, data: string): Promise<ArrayBuffer> {
  const raw = typeof key === "string" ? encoder.encode(key) : key;
  const cryptoKey = await crypto.subtle.importKey(
    "raw",
    raw,
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  return crypto.subtle.sign("HMAC", cryptoKey, encoder.encode(data));
}

/** HMAC-SHA256 rendered as lowercase hex, the form the Signature field takes. */
export async function hmacHex(key: ArrayBuffer, data: string): Promise<string> {
  return toHex(await hmac(key, data));
}

/** SHA-256 of a string as lowercase hex, used for the canonical request hash. */
export async function sha256Hex(data: string): Promise<string> {
  return toHex(await crypto.subtle.digest("SHA-256", encoder.encode(data)));
}

/**
 * Renders bytes as lowercase hex.
 *
 * An ArrayBuffer is not indexable on its own; Uint8Array is the view that
 * exposes it as bytes. The pad is load-bearing: without it a byte below 0x10
 * emits a single digit and shifts every character after it.
 */
export function toHex(buffer: ArrayBuffer): string {
  return Array.from(new Uint8Array(buffer))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}

/**
 * Compares two strings in time independent of how many leading characters
 * match.
 *
 * `===` returns at the first difference, so response time leaks how much of a
 * guess was correct, which is enough to recover a signature one character at a
 * time. Accumulating the XOR of every character and testing once takes the
 * same path regardless of the input.
 *
 * Comparing length first short-circuits, which is safe: a signature's length
 * is fixed and public.
 */
export function timingSafeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}
