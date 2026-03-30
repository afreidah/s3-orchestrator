// -------------------------------------------------------------------------------
// Audit Logging Benchmarks - Per-Request Audit Overhead
//
// Author: Alex Freidah
//
// Measures the cost of audit.Log, which is called on every S3 API request
// and storage operation. Includes the atomic OnEvent callback load and slog
// attribute assembly.
// -------------------------------------------------------------------------------

package audit

import (
	"context"
	"log/slog"
	"testing"
)

// BenchmarkLog_WithCallback measures audit.Log cost with a registered OnEvent
// callback (the production configuration).
func BenchmarkLog_WithCallback(b *testing.B) {
	SetOnEvent(func(string) {})
	defer SetOnEvent(nil)

	ctx := WithRequestID(context.Background(), "bench-req-id")

	for b.Loop() {
		Log(ctx, "s3.PutObject",
			slog.String("key", "photos/image.jpg"),
			slog.Int64("size", 12345),
		)
	}
}

// BenchmarkLog_WithoutCallback measures audit.Log cost when no OnEvent
// callback is registered (atomic pointer load returns nil).
func BenchmarkLog_WithoutCallback(b *testing.B) {
	SetOnEvent(nil)

	ctx := WithRequestID(context.Background(), "bench-req-id")

	for b.Loop() {
		Log(ctx, "s3.GetObject",
			slog.String("key", "photos/image.jpg"),
		)
	}
}

// BenchmarkLog_Concurrent measures audit.Log throughput under concurrent
// request load from multiple goroutines.
func BenchmarkLog_Concurrent(b *testing.B) {
	SetOnEvent(func(string) {})
	defer SetOnEvent(nil)

	ctx := WithRequestID(context.Background(), "bench-req-id")

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			Log(ctx, "s3.GetObject",
				slog.String("key", "photos/image.jpg"),
			)
		}
	})
}
