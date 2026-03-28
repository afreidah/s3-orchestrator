package bufpool

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// BenchmarkCopy measures streaming I/O throughput through the shared buffer
// pool at various object sizes. This path is used on every GET and PUT.
func BenchmarkCopy(b *testing.B) {
	sizes := []int{4 << 10, 64 << 10, 1 << 20, 16 << 20}
	for _, sz := range sizes {
		data := make([]byte, sz)
		b.Run(fmt.Sprintf("%dKB", sz/1024), func(b *testing.B) {
			b.SetBytes(int64(sz))
			for b.Loop() {
				_, _ = Copy(io.Discard, bytes.NewReader(data))
			}
		})
	}
}
