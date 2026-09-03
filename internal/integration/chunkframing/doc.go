// Package chunkframing classifies the head of an object body as AWS chunked
// encoding framing.
//
// Used by the unit tests here and by the build-tagged live-cluster diagnostic
// that flags objects which stored the on-the-wire framing of a streaming SigV4
// PUT instead of the decoded payload.
package chunkframing
