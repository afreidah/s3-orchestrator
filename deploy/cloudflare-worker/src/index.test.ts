// -------------------------------------------------------------------------------
// Edge Proxy Worker Tests
//
// Author: Alex Freidah
//
// Covers the two halves the worker is responsible for: refusing a request that
// does not carry a valid proxy signature, and re-signing a request that does
// against the origin's own host.
//
// The outbound leg is exercised against a captured fetch rather than a live
// origin, so the assertions are about what the origin would receive.
// -------------------------------------------------------------------------------

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import worker, { forward, verifyCaller } from "./index";
import type { Env } from "./index";
import { authorizationHeader, amzDate, canonicalizeQuery, parseAuthorization, splitTarget } from "./sigv4";

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

const ENV: Env = {
  ORIGIN_HOST: "s3.us-west-004.backblazeb2.com",
  ORIGIN_REGION: "us-west-004",
  ORIGIN_ACCESS_KEY_ID: "ORIGINKEYID",
  ORIGIN_SECRET_ACCESS_KEY: "origin-secret",
  PROXY_ACCESS_KEY_ID: "PROXYKEYID",
  PROXY_SECRET_ACCESS_KEY: "proxy-secret",
  MAX_CLOCK_SKEW_SECONDS: "300",
};

const PROXY_HOST = "b2-proxy.munchbox.cc";

/**
 * Builds a request signed with the proxy credential, the way s3-orchestrator
 * addresses the worker.
 */
async function signedRequest(options: {
  method?: string;
  path?: string;
  query?: string;
  headers?: Record<string, string>;
  body?: string;
  datetime?: string;
  secret?: string;
  accessKeyId?: string;
} = {}): Promise<Request> {
  const method = options.method ?? "GET";
  const path = options.path ?? "/munchbox-backups/key.bin";
  const query = options.query ?? "";
  const datetime = options.datetime ?? amzDate(new Date());
  const url = `https://${PROXY_HOST}${path}${query ? `?${query}` : ""}`;

  const headers = new Headers({
    host: PROXY_HOST,
    "x-amz-content-sha256": "UNSIGNED-PAYLOAD",
    "x-amz-date": datetime,
    ...options.headers,
  });

  const signedNames = [...headers.keys()]
    .map((n) => n.toLowerCase())
    .filter((n) => n === "host" || n === "content-type" || n.startsWith("x-amz-"))
    .sort();

  const { rawPath } = splitTarget(url);
  headers.set(
    "authorization",
    await authorizationHeader({
      method,
      rawPath,
      canonicalQuery: canonicalizeQuery(query),
      headers,
      signedNames,
      payloadHash: headers.get("x-amz-content-sha256") ?? "UNSIGNED-PAYLOAD",
      datetime,
      region: ENV.ORIGIN_REGION,
      accessKeyId: options.accessKeyId ?? ENV.PROXY_ACCESS_KEY_ID,
      secretAccessKey: options.secret ?? ENV.PROXY_SECRET_ACCESS_KEY,
    }),
  );

  return new Request(url, { method, headers, body: options.body });
}

// -------------------------------------------------------------------------
// VERIFICATION
// -------------------------------------------------------------------------

describe("verifyCaller", () => {
  it("accepts a request signed with the proxy credential", async () => {
    await expect(verifyCaller(await signedRequest(), ENV)).resolves.toBeUndefined();
  });

  it("accepts a signed request carrying a query string", async () => {
    const request = await signedRequest({ query: "partNumber=10&uploadId=a%2Fb" });
    await expect(verifyCaller(request, ENV)).resolves.toBeUndefined();
  });

  it("accepts a signed request carrying extra x-amz headers", async () => {
    const request = await signedRequest({ headers: { "x-amz-tagging": "class=backup" } });
    await expect(verifyCaller(request, ENV)).resolves.toBeUndefined();
  });

  it("rejects a request with no Authorization header", async () => {
    const request = new Request(`https://${PROXY_HOST}/bucket/key`);
    await expect(verifyCaller(request, ENV)).rejects.toThrow(/Missing Authorization header/);
  });

  it("rejects a presigned URL as unimplemented rather than as denied", async () => {
    const request = new Request(`https://${PROXY_HOST}/bucket/key?X-Amz-Signature=abc`);
    await expect(verifyCaller(request, ENV)).rejects.toThrow(/Presigned URL authentication/);
  });

  it("rejects an unknown access key", async () => {
    const request = await signedRequest({ accessKeyId: "SOMEONEELSE" });
    await expect(verifyCaller(request, ENV)).rejects.toThrow(/Unknown access key/);
  });

  it("rejects a signature produced with the wrong secret", async () => {
    const request = await signedRequest({ secret: "not-the-proxy-secret" });
    await expect(verifyCaller(request, ENV)).rejects.toThrow(/Signature does not match/);
  });

  it("rejects a request whose path was altered after signing", async () => {
    const original = await signedRequest({ path: "/munchbox-backups/key.bin" });
    const tampered = new Request(`https://${PROXY_HOST}/munchbox-backups/other.bin`, {
      headers: original.headers,
    });
    await expect(verifyCaller(tampered, ENV)).rejects.toThrow(/Signature does not match/);
  });

  it("rejects a timestamp outside the replay window", async () => {
    const stale = amzDate(new Date(Date.now() - 3600 * 1000));
    const request = await signedRequest({ datetime: stale });
    await expect(verifyCaller(request, ENV)).rejects.toThrow(/outside the accepted window/);
  });

  it("rejects a credential scope naming another service", async () => {
    const request = await signedRequest();
    const header = request.headers.get("authorization")!.replace("/s3/", "/execute-api/");
    const forged = new Request(request.url, { headers: { ...headerRecord(request), authorization: header } });
    await expect(verifyCaller(forged, ENV)).rejects.toThrow(/Credential scope does not match/);
  });
});

/** Flattens a request's headers so one of them can be replaced in a copy. */
function headerRecord(request: Request): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [name, value] of request.headers) out[name] = value;
  return out;
}

// -------------------------------------------------------------------------
// FORWARDING
// -------------------------------------------------------------------------

describe("forward", () => {
  let captured: { url: string; init: RequestInit } | undefined;
  const realFetch = globalThis.fetch;

  beforeEach(() => {
    captured = undefined;
    globalThis.fetch = ((url: string, init: RequestInit) => {
      captured = { url, init };
      return Promise.resolve(new Response("ok", { status: 200 }));
    }) as unknown as typeof globalThis.fetch;
  });

  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  it("retargets the request at the origin host over https", async () => {
    await forward(await signedRequest({ path: "/munchbox-backups/key.bin" }), ENV);
    expect(captured?.url).toBe(`https://${ENV.ORIGIN_HOST}/munchbox-backups/key.bin`);
  });

  it("preserves the query string", async () => {
    await forward(await signedRequest({ query: "partNumber=10&uploadId=abc" }), ENV);
    expect(captured?.url).toContain("?partNumber=10&uploadId=abc");
  });

  it("sets Host to the origin rather than the proxy", async () => {
    await forward(await signedRequest(), ENV);
    expect(headersOf(captured).get("host")).toBe(ENV.ORIGIN_HOST);
  });

  it("signs with the origin credential, not the proxy credential", async () => {
    await forward(await signedRequest(), ENV);
    const parsed = parseAuthorization(headersOf(captured).get("authorization")!);
    expect(parsed.accessKeyId).toBe(ENV.ORIGIN_ACCESS_KEY_ID);
    expect(parsed.credentialScope).toContain(`/${ENV.ORIGIN_REGION}/s3/aws4_request`);
  });

  it("covers host and every x-amz header in SignedHeaders", async () => {
    await forward(await signedRequest({ headers: { "x-amz-tagging": "class=backup" } }), ENV);
    const parsed = parseAuthorization(headersOf(captured).get("authorization")!);
    expect(parsed.signedHeaders).toContain("host");
    expect(parsed.signedHeaders).toContain("x-amz-tagging");
    expect(parsed.signedHeaders).toContain("x-amz-content-sha256");
    expect(parsed.signedHeaders).toContain("x-amz-date");
  });

  it("lists SignedHeaders in sorted order", async () => {
    await forward(await signedRequest({ headers: { "x-amz-tagging": "class=backup" } }), ENV);
    const { signedHeaders } = parseAuthorization(headersOf(captured).get("authorization")!);
    expect(signedHeaders).toEqual([...signedHeaders].sort());
  });

  it("drops the metadata Cloudflare adds inbound", async () => {
    const request = await signedRequest({ headers: { "x-amz-meta-keep": "yes" } });
    const withCfHeaders = new Request(request.url, {
      headers: { ...headerRecord(request), "cf-ray": "abc123", "x-forwarded-for": "10.0.0.1" },
    });
    await forward(withCfHeaders, ENV);
    expect(headersOf(captured).get("cf-ray")).toBeNull();
    expect(headersOf(captured).get("x-forwarded-for")).toBeNull();
    expect(headersOf(captured).get("x-amz-meta-keep")).toBe("yes");
  });

  it("does not follow redirects, which would replay the signature elsewhere", async () => {
    await forward(await signedRequest(), ENV);
    expect(captured?.init.redirect).toBe("manual");
  });

  it("refuses a chunked payload signature it cannot re-sign", async () => {
    const request = await signedRequest({
      headers: { "x-amz-content-sha256": "STREAMING-AWS4-HMAC-SHA256-PAYLOAD" },
    });
    await expect(forward(request, ENV)).rejects.toThrow(/cannot be re-signed without buffering/);
  });
});

/** Reads the headers off a captured fetch, whatever shape the init used. */
function headersOf(captured: { init: RequestInit } | undefined): Headers {
  return new Headers(captured?.init.headers as HeadersInit);
}

// -------------------------------------------------------------------------
// ENTRY POINT
// -------------------------------------------------------------------------

describe("fetch handler", () => {
  const realFetch = globalThis.fetch;

  afterEach(() => {
    globalThis.fetch = realFetch;
  });

  it("answers an unauthenticated request with an S3 XML error", async () => {
    const response = await worker.fetch(new Request(`https://${PROXY_HOST}/bucket/key`), ENV);
    expect(response.status).toBe(403);
    expect(response.headers.get("content-type")).toBe("application/xml");
    expect(await response.text()).toContain("<Code>AccessDenied</Code>");
  });

  it("answers a presigned request with NotImplemented", async () => {
    const url = `https://${PROXY_HOST}/bucket/key?X-Amz-Signature=abc`;
    const response = await worker.fetch(new Request(url), ENV);
    expect(response.status).toBe(501);
    expect(await response.text()).toContain("<Code>NotImplemented</Code>");
  });

  it("passes a verified request through to the origin", async () => {
    globalThis.fetch = (() =>
      Promise.resolve(new Response("payload", { status: 200 }))) as unknown as typeof globalThis.fetch;

    const response = await worker.fetch(await signedRequest(), ENV);
    expect(response.status).toBe(200);
    expect(await response.text()).toBe("payload");
  });
});
