// -------------------------------------------------------------------------------
// Circuit Breaker Benchmarks - PreCheck and PostCheck Throughput
//
// Author: Alex Freidah
//
// Measures the per-operation cost of circuit breaker state checks on the hot
// path. PreCheck is called before every store and backend operation; PostCheck
// is called after. Includes closed-state (happy path) and concurrent scenarios.
// -------------------------------------------------------------------------------

package breaker

import (
	"errors"
	"testing"
	"time"
)

// benchBreaker builds a closed breaker that counts only real errors, so a
// PostCheck(nil) is a success and the circuit stays closed.
func benchBreaker() *CircuitBreaker {
	return NewCircuitBreaker(Config{Name: "bench", Threshold: 5, Timeout: 15 * time.Second,
		IsError: func(err error) bool { return err != nil }, Sentinel: errors.New("open")})
}

// BenchmarkPreCheck_Closed measures the cost of PreCheck when the circuit is
// closed (healthy). This is the steady-state per-request overhead.
func BenchmarkPreCheck_Closed(b *testing.B) {
	cb := benchBreaker()

	for b.Loop() {
		_ = cb.PreCheck()
	}
}

// BenchmarkPostCheck_Success measures the cost of PostCheck with a nil error
// (successful operation). This is called after every store/backend call.
func BenchmarkPostCheck_Success(b *testing.B) {
	cb := benchBreaker()

	for b.Loop() {
		_ = cb.PostCheck(nil)
	}
}

// BenchmarkPrePostCheck_RoundTrip measures the combined cost of a
// PreCheck + PostCheck pair on the closed-state happy path.
func BenchmarkPrePostCheck_RoundTrip(b *testing.B) {
	cb := benchBreaker()

	for b.Loop() {
		_ = cb.PreCheck()
		_ = cb.PostCheck(nil)
	}
}

// BenchmarkPreCheck_Concurrent measures PreCheck throughput under concurrent
// load with the circuit closed.
func BenchmarkPreCheck_Concurrent(b *testing.B) {
	cb := benchBreaker()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = cb.PreCheck()
		}
	})
}
