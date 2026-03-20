// -------------------------------------------------------------------------------
// Login Throttle Tests
//
// Author: Alex Freidah
//
// Tests for per-IP brute-force login protection. Validates lockout after
// repeated failures, reset on success, lockout expiry, and IP isolation.
// -------------------------------------------------------------------------------

package httputil

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLoginThrottle_LockoutAfterFailures(t *testing.T) {
	lt := NewLoginThrottle(3, 5*time.Minute)
	defer lt.Close()

	addr := "10.0.0.1:12345"

	for i := range 3 {
		if lt.IsLockedOut(addr) {
			t.Fatalf("should not be locked out after %d failures", i)
		}
		lt.RecordFailure(addr)
	}

	if !lt.IsLockedOut(addr) {
		t.Error("should be locked out after 3 failures")
	}
}

func TestLoginThrottle_SuccessResetsCounter(t *testing.T) {
	lt := NewLoginThrottle(3, 5*time.Minute)
	defer lt.Close()

	addr := "10.0.0.1:12345"

	lt.RecordFailure(addr)
	lt.RecordFailure(addr)
	lt.RecordSuccess(addr)

	// Should be able to fail again without lockout since counter was reset
	lt.RecordFailure(addr)
	lt.RecordFailure(addr)
	if lt.IsLockedOut(addr) {
		t.Error("should not be locked out after success reset and 2 new failures")
	}
}

func TestLoginThrottle_LockoutExpires(t *testing.T) {
	lt := NewLoginThrottle(3, 50*time.Millisecond)
	defer lt.Close()

	addr := "10.0.0.1:12345"

	for range 3 {
		lt.RecordFailure(addr)
	}

	if !lt.IsLockedOut(addr) {
		t.Fatal("should be locked out immediately after 3 failures")
	}

	time.Sleep(60 * time.Millisecond)

	if lt.IsLockedOut(addr) {
		t.Error("lockout should have expired")
	}
}

func TestLoginThrottle_IPIsolation(t *testing.T) {
	lt := NewLoginThrottle(3, 5*time.Minute)
	defer lt.Close()

	addr1 := "10.0.0.1:12345"
	addr2 := "10.0.0.2:12345"

	for range 3 {
		lt.RecordFailure(addr1)
	}

	if !lt.IsLockedOut(addr1) {
		t.Error("addr1 should be locked out")
	}
	if lt.IsLockedOut(addr2) {
		t.Error("addr2 should not be locked out")
	}
}

func TestLoginThrottle_StripPort(t *testing.T) {
	lt := NewLoginThrottle(3, 5*time.Minute)
	defer lt.Close()

	// Failures from different ports on same IP should accumulate
	lt.RecordFailure("10.0.0.1:11111")
	lt.RecordFailure("10.0.0.1:22222")
	lt.RecordFailure("10.0.0.1:33333")

	if !lt.IsLockedOut("10.0.0.1:44444") {
		t.Error("should be locked out regardless of port")
	}
}

func TestLoginThrottle_ConcurrentAccess(t *testing.T) {
	lt := NewLoginThrottle(100, 5*time.Minute)
	defer lt.Close()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			addr := fmt.Sprintf("10.0.0.%d:12345", i%10)
			lt.RecordFailure(addr)
			lt.IsLockedOut(addr)
			lt.RecordSuccess(addr)
		})
	}
	wg.Wait()
}

func FuzzLoginThrottle_RemoteAddr(f *testing.F) {
	f.Add("10.0.0.1:8080")
	f.Add("192.168.1.1:443")
	f.Add("[::1]:8080")
	f.Add("not-an-address")
	f.Add("")
	f.Add(":")
	f.Add("10.0.0.1:")
	f.Add(":8080")

	lt := NewLoginThrottle(3, time.Minute)
	defer lt.Close()

	f.Fuzz(func(t *testing.T, addr string) {
		// Must not panic on any input
		lt.RecordFailure(addr)
		lt.IsLockedOut(addr)
		lt.RecordSuccess(addr)
	})
}
