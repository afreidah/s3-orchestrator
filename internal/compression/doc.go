// Package compression stores objects as chunked zstd in the Zstandard seekable
// format: one independently decodable frame per chunk, with a seek table in a
// trailing skippable frame.
//
// The chunking is what makes a partial read cheap. A single-frame object has
// one entry point, byte zero, so serving any range means fetching the whole
// stored object and discarding the prefix - a cost proportional to object size
// rather than to the bytes asked for, which is wrong for a proxy whose backends
// meter egress. Independently decodable frames give one entry point per chunk.
//
// The frame layout and seek table come from
// github.com/SaveTheRbtz/zstd-seekable-format-go, which implements the format
// published as facebook/zstd contrib/seekable_format. Chunk size is not part of
// that library's API: it writes one frame per Write call, so the chunk boundary
// is chosen here.
//
// Compression composes with encryption in one order only - compress, then
// encrypt - because ciphertext does not compress. Reads run it backwards:
// decrypt, decompress, then slice.
package compression
