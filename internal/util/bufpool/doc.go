// Package bufpool provides a shared pool of reusable byte buffers for streaming
// I/O, reducing GC pressure by replacing per-call allocations in io.Copy with
// pooled buffers via io.CopyBuffer.
package bufpool
