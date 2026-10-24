// Package storedform turns the bytes a client wrote into the bytes a backend
// stores, and back. A copy's row records its stored form - compressed,
// encrypted, both or neither - and this package is where that form is applied
// on the way in and undone on the way out.
//
// The layers always stack in one order: compression runs on the client's
// bytes, and encryption on whatever compression produced, since ciphertext
// does not compress. A write uses ShouldCompress and Compress to decide on and
// produce an encoding, Describe to record it on the form, and Seal to wrap the
// result in an envelope. A whole-object read uses Open, which unseals and then
// decodes. Unseal and Decode are the two halves for callers that need them
// separately, and CheckEnvelope refuses a row whose encryption flag disagrees
// with its bytes, so ciphertext is never served or hashed as the object.
//
// Layers.Reform moves a copy from one stored form to another for the bulk
// rewrite passes, touching only the layers that change. An encoding that
// misses the configured ratio is reported as a *Declined error rather than
// stored.
//
// The client write path, multipart assembly, replication, the scrubber and the
// rewrite passes all go through these functions, so they cannot disagree about
// what a row's form columns mean. Ranged reads are the exception: they decode
// only the frames a range covers, through the seek table, and stay with the
// read path.
package storedform
