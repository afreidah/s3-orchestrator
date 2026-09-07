// -------------------------------------------------------------------------------
// Proxy Errors - S3-Shaped Failure Responses
//
// Author: Alex Freidah
//
// Carries an HTTP status and an S3 error code alongside a message, and renders
// the pair as the XML error document an S3 client expects. Clients branch on
// the Code element to decide whether a failure is retryable, so a plain-text
// body degrades every rejection into an opaque error.
// -------------------------------------------------------------------------------

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

/**
 * An error carrying the HTTP status and S3 error code to answer with.
 *
 * The `readonly status: number` form in the constructor signature is a
 * TypeScript parameter property: it declares the field, assigns it from the
 * argument, and marks it immutable, standing in for a `this.status = status`
 * body.
 */
export class ProxyError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "ProxyError";
  }
}

// -------------------------------------------------------------------------
// RENDERING
// -------------------------------------------------------------------------

/**
 * Renders a ProxyError as an S3-style XML error document.
 *
 * Anything that is not a ProxyError becomes a generic InternalError, so an
 * unexpected throw cannot leak a stack trace or an internal message to the
 * caller.
 */
export function errorResponse(err: unknown): Response {
  const proxy = err instanceof ProxyError
    ? err
    : new ProxyError(500, "InternalError", "The request could not be completed.");

  const body = `<?xml version="1.0" encoding="UTF-8"?>\n` +
    `<Error><Code>${proxy.code}</Code><Message>${escapeXml(proxy.message)}</Message></Error>`;

  return new Response(body, {
    status: proxy.status,
    headers: { "content-type": "application/xml" },
  });
}

/**
 * Escapes the five characters XML reserves, so a message containing them
 * cannot terminate an element early or inject one of its own.
 *
 * The trailing `!` is a non-null assertion. The lookup is total over the
 * characters the pattern matches, but the index signature is
 * `string | undefined` as far as the compiler can tell.
 */
export function escapeXml(value: string): string {
  return value.replace(
    /[<>&"']/g,
    (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&apos;" })[c]!,
  );
}
