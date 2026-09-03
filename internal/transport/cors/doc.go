// Package cors answers browser preflight requests for the S3 surface and
// attaches the matching access-control headers to the responses that follow.
//
// The rules are per virtual bucket and come from config, compiled once into a
// Registry the Handler swaps atomically on reload. A preflight carries no
// credentials by design, so it is answered from the rule set alone, ahead of
// the authentication the request that follows still performs: the preflight
// grants nothing on its own, it only tells the browser whether to proceed.
//
// An empty rule set refuses every cross-origin preflight, which is the
// behavior a bucket reached only by server-side clients wants.
package cors
