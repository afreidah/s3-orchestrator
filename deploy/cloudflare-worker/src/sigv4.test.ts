// -------------------------------------------------------------------------------
// SigV4 Canonicalization and Signing Tests
//
// Author: Alex Freidah
//
// Covers the canonical forms the signature is computed over and the signature
// itself. The expected signatures come from an independent implementation of
// the specification rather than from this code, so a normalization mistake
// made in both directions of the proxy still fails the suite.
// -------------------------------------------------------------------------------

import { describe, expect, it } from "vitest";

import { ProxyError } from "./errors";
import {
  ALGORITHM,
  amzDate,
  assertFresh,
  authorizationHeader,
  canonicalizeQuery,
  collapse,
  computeSignature,
  credentialScope,
  parseAuthorization,
  rfc3986,
  sha256Hex,
  splitTarget,
  timingSafeEqual,
  toHex,
} from "./sigv4";

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

const SECRET = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY";
const DATETIME = "20260907T044132Z";
const REGION = "us-west-004";
const SIGNED_NAMES = ["host", "x-amz-content-sha256", "x-amz-date"];

function fixtureHeaders(): Headers {
  return new Headers({
    host: "b2-proxy.example.com",
    "x-amz-content-sha256": "UNSIGNED-PAYLOAD",
    "x-amz-date": DATETIME,
  });
}

function fixtureParams(method: string, rawPath: string, rawQuery: string) {
  return {
    method,
    rawPath,
    canonicalQuery: canonicalizeQuery(rawQuery),
    headers: fixtureHeaders(),
    signedNames: SIGNED_NAMES,
    payloadHash: "UNSIGNED-PAYLOAD",
    datetime: DATETIME,
    region: REGION,
    secretAccessKey: SECRET,
  };
}

// -------------------------------------------------------------------------
// PERCENT ENCODING
// -------------------------------------------------------------------------

describe("rfc3986", () => {
  it("encodes the characters encodeURIComponent leaves alone", () => {
    expect(rfc3986("a!b")).toBe("a%21b");
    expect(rfc3986("a'b")).toBe("a%27b");
    expect(rfc3986("a(b)c")).toBe("a%28b%29c");
    expect(rfc3986("a*b")).toBe("a%2Ab");
  });

  it("leaves the unreserved set untouched", () => {
    expect(rfc3986("a~b")).toBe("a~b");
    expect(rfc3986("key.txt")).toBe("key.txt");
    expect(rfc3986("A-Z_a-z.0-9~")).toBe("A-Z_a-z.0-9~");
  });

  it("encodes a space as %20 rather than a plus", () => {
    expect(rfc3986("a b")).toBe("a%20b");
  });

  it("encodes a slash, which a query value may legally contain", () => {
    expect(rfc3986("a/b")).toBe("a%2Fb");
  });
});

// -------------------------------------------------------------------------
// QUERY CANONICALIZATION
// -------------------------------------------------------------------------

describe("canonicalizeQuery", () => {
  it("sorts parameters by name", () => {
    expect(canonicalizeQuery("b=2&a=1")).toBe("a=1&b=2");
  });

  it("sorts a repeated name by value", () => {
    expect(canonicalizeQuery("a=1&a=0")).toBe("a=0&a=1");
  });

  it("renders a valueless parameter with a trailing separator", () => {
    expect(canonicalizeQuery("flag")).toBe("flag=");
  });

  it("re-encodes a decoded space as %20", () => {
    expect(canonicalizeQuery("x=a b")).toBe("x=a%20b");
  });

  it("preserves an encoded slash through the decode and re-encode round trip", () => {
    expect(canonicalizeQuery("uploadId=a%2Fb&partNumber=10")).toBe("partNumber=10&uploadId=a%2Fb");
  });

  it("returns an empty string for an empty query", () => {
    expect(canonicalizeQuery("")).toBe("");
  });

  it("ignores empty segments left by a stray separator", () => {
    expect(canonicalizeQuery("a=1&&b=2")).toBe("a=1&b=2");
  });
});

// -------------------------------------------------------------------------
// TARGET SPLITTING
// -------------------------------------------------------------------------

describe("splitTarget", () => {
  it("separates the path from the query", () => {
    const { rawPath, canonicalQuery } = splitTarget("https://h.example.com/bucket/key?b=2&a=1");
    expect(rawPath).toBe("/bucket/key");
    expect(canonicalQuery).toBe("a=1&b=2");
  });

  it("leaves an already-encoded path untouched rather than double-encoding it", () => {
    const { rawPath } = splitTarget("https://h.example.com/bucket/my%20key%21.txt");
    expect(rawPath).toBe("/bucket/my%20key%21.txt");
  });

  it("preserves dot segments the caller signed literally", () => {
    const { rawPath } = splitTarget("https://h.example.com/bucket/a/../b");
    expect(rawPath).toBe("/bucket/a/../b");
  });

  it("excludes a fragment from both components", () => {
    const { rawPath, canonicalQuery } = splitTarget("https://h.example.com/bucket/key?a=1#frag");
    expect(rawPath).toBe("/bucket/key");
    expect(canonicalQuery).toBe("a=1");
  });

  it("returns a root path when the URL carries none", () => {
    expect(splitTarget("https://h.example.com").rawPath).toBe("/");
  });

  it("returns an empty query when the URL carries none", () => {
    expect(splitTarget("https://h.example.com/bucket").canonicalQuery).toBe("");
  });
});

// -------------------------------------------------------------------------
// HEADER AND TIMESTAMP NORMALIZATION
// -------------------------------------------------------------------------

describe("collapse", () => {
  it("trims and collapses internal whitespace runs", () => {
    expect(collapse("  a   b  ")).toBe("a b");
    expect(collapse("a\t\tb")).toBe("a b");
  });
});

describe("amzDate", () => {
  it("renders ISO 8601 basic form in UTC", () => {
    expect(amzDate(new Date(Date.UTC(2026, 8, 7, 4, 41, 32, 123)))).toBe("20260907T044132Z");
  });
});

// -------------------------------------------------------------------------
// PRIMITIVES
// -------------------------------------------------------------------------

describe("sha256Hex", () => {
  it("matches the published digest of the empty string", async () => {
    expect(await sha256Hex("")).toBe(
      "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    );
  });
});

describe("toHex", () => {
  it("pads a byte below 0x10 to two digits", () => {
    expect(toHex(new Uint8Array([0x00, 0x0f, 0xff]).buffer)).toBe("000fff");
  });
});

describe("timingSafeEqual", () => {
  it("accepts identical strings", () => {
    expect(timingSafeEqual("abc123", "abc123")).toBe(true);
  });

  it("rejects a difference in the final character", () => {
    expect(timingSafeEqual("abc123", "abc124")).toBe(false);
  });

  it("rejects a length mismatch", () => {
    expect(timingSafeEqual("abc", "abcd")).toBe(false);
  });

  it("accepts two empty strings", () => {
    expect(timingSafeEqual("", "")).toBe(true);
  });
});

describe("credentialScope", () => {
  it("joins date, region, service and terminator", () => {
    expect(credentialScope("20260907", "us-west-004")).toBe("20260907/us-west-004/s3/aws4_request");
  });
});

// -------------------------------------------------------------------------
// SIGNING
// -------------------------------------------------------------------------

describe("computeSignature", () => {
  it("matches the reference implementation for a multipart-style query", async () => {
    const params = fixtureParams(
      "GET",
      "/munchbox-backups/tempo-traces/data%20set%21.parquet",
      "x-id=GetObject&partNumber=10&uploadId=a%2Fb",
    );
    expect(params.canonicalQuery).toBe("partNumber=10&uploadId=a%2Fb&x-id=GetObject");
    expect(await computeSignature(params)).toBe(
      "565ce89c2d4fe9c385521b7c1b30b6a763c9a74631ab9c2b86dc4c982fd38298",
    );
  });

  it("matches the reference implementation for a request with no query", async () => {
    expect(await computeSignature(fixtureParams("GET", "/munchbox-backups/plain.txt", ""))).toBe(
      "6aac8b26311ac6e0aa7436f1d17a4c36b892adddfa616b4a9977404c0ca8aa36",
    );
  });

  it("matches the reference implementation for a PUT", async () => {
    expect(await computeSignature(fixtureParams("PUT", "/munchbox-backups/nested/key.bin", ""))).toBe(
      "e9fa1fe86f14da019c2c7ad583a627fbcfb8e20a488780930e059e95a6fbbcac",
    );
  });

  it("changes when the method changes", async () => {
    const get = await computeSignature(fixtureParams("GET", "/bucket/key", ""));
    const put = await computeSignature(fixtureParams("PUT", "/bucket/key", ""));
    expect(get).not.toBe(put);
  });

  it("changes when a signed header value changes", async () => {
    const base = fixtureParams("GET", "/bucket/key", "");
    const tampered = fixtureParams("GET", "/bucket/key", "");
    tampered.headers.set("host", "elsewhere.example.com");
    expect(await computeSignature(base)).not.toBe(await computeSignature(tampered));
  });
});

describe("authorizationHeader", () => {
  it("renders a header the parser reads back unchanged", async () => {
    const params = { ...fixtureParams("GET", "/bucket/key", ""), accessKeyId: "AKIDEXAMPLE" };
    const header = await authorizationHeader(params);
    const parsed = parseAuthorization(header);

    expect(header.startsWith(`${ALGORITHM} `)).toBe(true);
    expect(parsed.accessKeyId).toBe("AKIDEXAMPLE");
    expect(parsed.credentialScope).toBe("20260907/us-west-004/s3/aws4_request");
    expect(parsed.signedHeaders).toEqual(SIGNED_NAMES);
    expect(parsed.signature).toBe(await computeSignature(params));
  });
});

// -------------------------------------------------------------------------
// AUTHORIZATION PARSING
// -------------------------------------------------------------------------

describe("parseAuthorization", () => {
  const valid = `${ALGORITHM} Credential=AKID/20260907/us-west-004/s3/aws4_request, ` +
    "SignedHeaders=host;x-amz-date, Signature=deadbeef";

  it("splits the credential on the first separator only", () => {
    const parsed = parseAuthorization(valid);
    expect(parsed.accessKeyId).toBe("AKID");
    expect(parsed.credentialScope).toBe("20260907/us-west-004/s3/aws4_request");
  });

  it("lowercases the signed header names", () => {
    const header = valid.replace("SignedHeaders=host;x-amz-date", "SignedHeaders=Host;X-Amz-Date");
    expect(parseAuthorization(header).signedHeaders).toEqual(["host", "x-amz-date"]);
  });

  it("rejects an unsupported scheme", () => {
    expect(() => parseAuthorization("Basic dXNlcjpwYXNz")).toThrow(ProxyError);
  });

  it("rejects a header missing the signature", () => {
    const header = `${ALGORITHM} Credential=AKID/20260907/us-west-004/s3/aws4_request, ` +
      "SignedHeaders=host";
    expect(() => parseAuthorization(header)).toThrow(/Malformed Authorization header/);
  });

  it("rejects a credential with no scope", () => {
    const header = `${ALGORITHM} Credential=AKID, SignedHeaders=host, Signature=deadbeef`;
    expect(() => parseAuthorization(header)).toThrow(/Malformed credential/);
  });
});

// -------------------------------------------------------------------------
// REPLAY WINDOW
// -------------------------------------------------------------------------

describe("assertFresh", () => {
  const now = Date.UTC(2026, 8, 7, 4, 41, 32);

  it("accepts a timestamp inside the window", () => {
    expect(() => assertFresh("20260907T044000Z", 300, now)).not.toThrow();
  });

  it("rejects a timestamp older than the window", () => {
    expect(() => assertFresh("20260907T042000Z", 300, now)).toThrow(/outside the accepted window/);
  });

  it("rejects a timestamp ahead of the window", () => {
    expect(() => assertFresh("20260907T050000Z", 300, now)).toThrow(/outside the accepted window/);
  });

  it("rejects a malformed timestamp", () => {
    expect(() => assertFresh("2026-09-07T04:41:32Z", 300, now)).toThrow(/Malformed x-amz-date/);
  });
});
