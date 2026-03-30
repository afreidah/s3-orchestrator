// -------------------------------------------------------------------------------
// Admission Controller Benchmarks - Semaphore Acquire/Release Throughput
//
// Author: Alex Freidah
//
// Measures the per-request cost of admission control acquire and release.
// Every HTTP request passes through this path, so overhead directly impacts
// P99 latency. Includes uncontended and concurrent scenarios.
// -------------------------------------------------------------------------------

package s3api

import (
	"testing"
)

// BenchmarkAdmission_AcquireRelease measures the cost of a non-blocking
// acquire + release round-trip on an uncontended semaphore.
func BenchmarkAdmission_AcquireRelease(b *testing.B) {
	ac := NewAdmissionController(1000)
	sem := ac.sem

	for b.Loop() {
		sem <- struct{}{}
		<-sem
	}
}

// BenchmarkAdmission_AcquireRelease_Concurrent measures acquire/release
// throughput under concurrent load where the semaphore is partially filled.
func BenchmarkAdmission_AcquireRelease_Concurrent(b *testing.B) {
	ac := NewAdmissionController(1000)
	sem := ac.sem

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sem <- struct{}{}
			<-sem
		}
	})
}

// BenchmarkAdmission_SplitPool_AcquireRelease measures the split read/write
// pool acquire + release cost.
func BenchmarkAdmission_SplitPool_AcquireRelease(b *testing.B) {
	ac := NewSplitAdmissionController(500, 500)

	b.Run("read", func(b *testing.B) {
		for b.Loop() {
			ac.readSem <- struct{}{}
			<-ac.readSem
		}
	})

	b.Run("write", func(b *testing.B) {
		for b.Loop() {
			ac.writeSem <- struct{}{}
			<-ac.writeSem
		}
	})
}
