// -------------------------------------------------------------------------------
// Buffer Pool - Shared Reusable Byte Buffers for Streaming I/O
//
// Author: Alex Freidah
//
// Provides a sync.Pool of 32 KB byte buffers for use with io.CopyBuffer at all
// streaming call sites (GET proxy, PUT body buffering, CopyObject pipe, multipart
// assembly, UI download). Eliminates per-call buffer allocations that add GC
// pressure under high concurrency.
// -------------------------------------------------------------------------------

// Package bufpool provides a shared pool of reusable byte buffers for streaming
// I/O, reducing GC pressure by replacing per-call allocations in io.Copy with
// pooled buffers via io.CopyBuffer.
package bufpool

import (
	"io"
	"sync"
)

// bufSize matches the default buffer size used by io.Copy (32 KB). Using the
// same size ensures identical copy behavior while enabling buffer reuse.
const bufSize = 32 * 1024

var pool = sync.Pool{
	New: func() any {
		b := make([]byte, bufSize)
		return &b
	},
}

// Get returns a pooled buffer. The caller must call Put when done.
func Get() *[]byte { return pool.Get().(*[]byte) }

// Put returns a buffer to the pool for reuse.
func Put(b *[]byte) { pool.Put(b) }

// Copy works like io.Copy but uses a pooled buffer to avoid per-call
// allocations. Safe for concurrent use.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	buf := Get()
	defer Put(buf)
	return io.CopyBuffer(dst, src, *buf)
}
