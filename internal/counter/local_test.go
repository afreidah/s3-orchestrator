// -------------------------------------------------------------------------------
// LocalCounterBackend Tests
//
// Author: Alex Freidah
//
// Tests for the in-memory atomic counter backend. Validates add/load/swap
// operations, batch methods, unknown backend handling, and nil initialization.
// -------------------------------------------------------------------------------

package counter

import "testing"

// TestLocalCounterBackend_Add_And_Load verifies the local counter backend add and load contract.
// Asserts that apiRequests = , want 5.
func TestLocalCounterBackend_Add_And_Load(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.Add("b1", FieldAPIRequests, 5)
	cb.Add("b1", FieldEgressBytes, 1024)
	cb.Add("b1", FieldIngressBytes, 2048)

	if got := cb.Load("b1", FieldAPIRequests); got != 5 {
		t.Errorf("apiRequests = %d, want 5", got)
	}
	if got := cb.Load("b1", FieldEgressBytes); got != 1024 {
		t.Errorf("egressBytes = %d, want 1024", got)
	}
	if got := cb.Load("b1", FieldIngressBytes); got != 2048 {
		t.Errorf("ingressBytes = %d, want 2048", got)
	}
}

// TestLocalCounterBackend_Add_Accumulates verifies the local counter backend add accumulates contract.
// Asserts that apiRequests = , want 10.
func TestLocalCounterBackend_Add_Accumulates(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.Add("b1", FieldAPIRequests, 3)
	cb.Add("b1", FieldAPIRequests, 7)

	if got := cb.Load("b1", FieldAPIRequests); got != 10 {
		t.Errorf("apiRequests = %d, want 10", got)
	}
}

// TestLocalCounterBackend_Swap_ReturnsAndResets verifies the local counter backend swap returns and resets contract.
// Asserts that Swap returned , want 42.
func TestLocalCounterBackend_Swap_ReturnsAndResets(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.Add("b1", FieldAPIRequests, 42)

	swapped := cb.Swap("b1", FieldAPIRequests)
	if swapped != 42 {
		t.Errorf("Swap returned %d, want 42", swapped)
	}
	if got := cb.Load("b1", FieldAPIRequests); got != 0 {
		t.Errorf("apiRequests after swap = %d, want 0", got)
	}
}

// TestLocalCounterBackend_AddAll verifies the local counter backend add all contract.
// Asserts that apiRequests = , want 3.
func TestLocalCounterBackend_AddAll(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.AddAll("b1", 3, 1024, 2048)

	result := cb.LoadAll("b1")
	if result.APIRequests != 3 {
		t.Errorf("apiRequests = %d, want 3", result.APIRequests)
	}
	if result.EgressBytes != 1024 {
		t.Errorf("egressBytes = %d, want 1024", result.EgressBytes)
	}
	if result.IngressBytes != 2048 {
		t.Errorf("ingressBytes = %d, want 2048", result.IngressBytes)
	}
}

// TestLocalCounterBackend_AddAll_SkipsZero verifies the local counter backend add all skips zero contract.
// Asserts that expected all zeros, got v.
func TestLocalCounterBackend_AddAll_SkipsZero(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.AddAll("b1", 0, 0, 0)

	result := cb.LoadAll("b1")
	if result.APIRequests != 0 || result.EgressBytes != 0 || result.IngressBytes != 0 {
		t.Errorf("expected all zeros, got %+v", result)
	}
}

// TestLocalCounterBackend_LoadAll_UnknownBackend verifies the local counter backend load all unknown backend contract.
// Asserts that expected zero result for unknown backend, got v.
func TestLocalCounterBackend_LoadAll_UnknownBackend(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	result := cb.LoadAll("unknown")
	if result.APIRequests != 0 || result.EgressBytes != 0 || result.IngressBytes != 0 {
		t.Errorf("expected zero result for unknown backend, got %+v", result)
	}
}

// TestLocalCounterBackend_UnknownBackend_NoOp verifies the local counter backend unknown backend no op contract.
// Asserts that Load on unknown = , want 0.
func TestLocalCounterBackend_UnknownBackend_NoOp(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	// Should not panic
	cb.Add("unknown", FieldAPIRequests, 100)
	cb.AddAll("unknown", 1, 2, 3)

	if got := cb.Load("unknown", FieldAPIRequests); got != 0 {
		t.Errorf("Load on unknown = %d, want 0", got)
	}
	if got := cb.Swap("unknown", FieldAPIRequests); got != 0 {
		t.Errorf("Swap on unknown = %d, want 0", got)
	}
}

// TestLocalCounterBackend_Pools covers the per-pool counters: they are created
// on first charge, accumulate, and are read and reset independently of the
// fixed dimensions.
func TestLocalCounterBackend_Pools(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.AddPools("b1", map[string]int64{"class_a": 2, "class_b": 5})
	cb.AddPools("b1", map[string]int64{"class_a": 3})
	// A zero delta must not create a counter: a pool nothing charged should
	// not appear in the flush at all.
	cb.AddPools("b1", map[string]int64{"class_c": 0})

	if got := cb.LoadPool("b1", "class_a"); got != 5 {
		t.Errorf("class_a = %d, want 5", got)
	}
	if got := cb.LoadPool("b1", "class_b"); got != 5 {
		t.Errorf("class_b = %d, want 5", got)
	}
	if got := cb.LoadPool("b1", "never-charged"); got != 0 {
		t.Errorf("uncharged pool = %d, want 0", got)
	}

	swapped := cb.SwapPools("b1")
	if swapped["class_a"] != 5 || swapped["class_b"] != 5 {
		t.Errorf("SwapPools = %v, want class_a and class_b at 5", swapped)
	}
	if _, ok := swapped["class_c"]; ok {
		t.Errorf("SwapPools = %v, want no counter for a zero delta", swapped)
	}
	if got := cb.LoadPool("b1", "class_a"); got != 0 {
		t.Errorf("class_a after swap = %d, want 0", got)
	}
}

// TestLocalCounterBackend_Pools_UnknownBackend pins the nil-safe paths. A
// backend can be charged after a drain removed it, and a panic there would
// take down the request rather than dropping a counter.
func TestLocalCounterBackend_Pools_UnknownBackend(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.AddPools("unknown", map[string]int64{"class_a": 1})

	if got := cb.LoadPool("unknown", "class_a"); got != 0 {
		t.Errorf("LoadPool on unknown = %d, want 0", got)
	}
	if got := cb.SwapPools("unknown"); got != nil {
		t.Errorf("SwapPools on unknown = %v, want nil", got)
	}
	if got := cb.SwapPools("b1"); got != nil {
		t.Errorf("SwapPools on an uncharged backend = %v, want nil", got)
	}
}

// TestLocalCounterBackend_SwapAllBackends_CarriesPools is the Redis recovery
// contract: the whole-map swap has to carry pool deltas too, or a budget spent
// during an outage is replayed as bytes and requests with no pool behind them.
func TestLocalCounterBackend_SwapAllBackends_CarriesPools(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})
	cb.AddAll("b1", 3, 100, 200)
	cb.AddPools("b1", map[string]int64{"class_a": 3})

	got := cb.SwapAllBackends()["b1"]
	if got.APIRequests != 3 || got.Pools["class_a"] != 3 {
		t.Errorf("snapshot = %+v, want 3 requests and 3 against class_a", got)
	}
	if left := cb.LoadPool("b1", "class_a"); left != 0 {
		t.Errorf("pool counter after swap = %d, want 0", left)
	}
}

// TestLocalCounterBackend_UnknownField verifies the local counter backend unknown field contract.
// Asserts that Load on bogus field = , want 0.
func TestLocalCounterBackend_UnknownField(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.Add("b1", "bogus_field", 100)
	if got := cb.Load("b1", "bogus_field"); got != 0 {
		t.Errorf("Load on bogus field = %d, want 0", got)
	}
	if got := cb.Swap("b1", "bogus_field"); got != 0 {
		t.Errorf("Swap on bogus field = %d, want 0", got)
	}
}

// TestLocalCounterBackend_SwapAll_ReturnsAndResets verifies the local counter backend swap all returns and resets contract.
// Asserts that SwapAll apiRequests = , want 10.
func TestLocalCounterBackend_SwapAll_ReturnsAndResets(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.AddAll("b1", 10, 2048, 4096)

	result := cb.SwapAll("b1")
	if result.APIRequests != 10 {
		t.Errorf("SwapAll apiRequests = %d, want 10", result.APIRequests)
	}
	if result.EgressBytes != 2048 {
		t.Errorf("SwapAll egressBytes = %d, want 2048", result.EgressBytes)
	}
	if result.IngressBytes != 4096 {
		t.Errorf("SwapAll ingressBytes = %d, want 4096", result.IngressBytes)
	}

	// After SwapAll, all counters should be zero
	after := cb.LoadAll("b1")
	if after.APIRequests != 0 || after.EgressBytes != 0 || after.IngressBytes != 0 {
		t.Errorf("counters should be zero after SwapAll, got %+v", after)
	}
}

// TestLocalCounterBackend_SwapAll_UnknownBackend verifies the local counter backend swap all unknown backend contract.
// Asserts that SwapAll on unknown should return zeros, got v.
func TestLocalCounterBackend_SwapAll_UnknownBackend(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	result := cb.SwapAll("unknown")
	if result.APIRequests != 0 || result.EgressBytes != 0 || result.IngressBytes != 0 {
		t.Errorf("SwapAll on unknown should return zeros, got %+v", result)
	}
}

// TestLocalCounterBackend_MultipleBackends verifies the local counter backend multiple backends contract.
// Asserts that b1 apiRequests = , want 10.
func TestLocalCounterBackend_MultipleBackends(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1", "b2"})

	cb.Add("b1", FieldAPIRequests, 10)
	cb.Add("b2", FieldAPIRequests, 20)

	if got := cb.Load("b1", FieldAPIRequests); got != 10 {
		t.Errorf("b1 apiRequests = %d, want 10", got)
	}
	if got := cb.Load("b2", FieldAPIRequests); got != 20 {
		t.Errorf("b2 apiRequests = %d, want 20", got)
	}
}

// TestLocalCounterBackend_Backends verifies the local counter backend backends contract.
// Asserts that Backends() returned names, want 2.
func TestLocalCounterBackend_Backends(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"alpha", "beta"})

	names := cb.Backends()
	if len(names) != 2 {
		t.Fatalf("Backends() returned %d names, want 2", len(names))
	}

	seen := make(map[string]bool)
	for _, n := range names {
		seen[n] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Errorf("Backends() = %v, want alpha and beta", names)
	}
}

// TestLocalCounterBackend_NilInit verifies the local counter backend nil init contract.
// Asserts that Load after nil init = , want 0.
func TestLocalCounterBackend_NilInit(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend(nil)

	// Should not panic
	cb.Add("b1", FieldAPIRequests, 1)
	if got := cb.Load("b1", FieldAPIRequests); got != 0 {
		t.Errorf("Load after nil init = %d, want 0", got)
	}
	if got := len(cb.Backends()); got != 0 {
		t.Errorf("Backends() length = %d, want 0", got)
	}
}

// TestSwapAllBackends_ReturnsAllDeltas verifies that SwapAllBackends returns
// accumulated deltas for all backends and resets them to zero atomically.
func TestSwapAllBackends_ReturnsAllDeltas(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1", "b2"})

	cb.AddAll("b1", 10, 100, 200)
	cb.AddAll("b2", 20, 300, 400)

	deltas := cb.SwapAllBackends()

	if len(deltas) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(deltas))
	}
	if d := deltas["b1"]; d.APIRequests != 10 || d.EgressBytes != 100 || d.IngressBytes != 200 {
		t.Errorf("b1 deltas = %+v, want {10, 100, 200}", d)
	}
	if d := deltas["b2"]; d.APIRequests != 20 || d.EgressBytes != 300 || d.IngressBytes != 400 {
		t.Errorf("b2 deltas = %+v, want {20, 300, 400}", d)
	}

	// Counters should be zeroed after swap.
	if got := cb.Load("b1", FieldAPIRequests); got != 0 {
		t.Errorf("b1 apiRequests after swap = %d, want 0", got)
	}
	if got := cb.Load("b2", FieldAPIRequests); got != 0 {
		t.Errorf("b2 apiRequests after swap = %d, want 0", got)
	}
}

// TestSwapAllBackends_ConcurrentAdds verifies that Add calls concurrent with
// SwapAllBackends do not lose deltas. The delta is either captured in the
// swap result or remains in the new counters  -  never dropped.
func TestSwapAllBackends_ConcurrentAdds(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	const iterations = 10000
	done := make(chan struct{})

	// Goroutine adds 1 per iteration.
	go func() {
		defer close(done)
		for range iterations {
			cb.Add("b1", FieldAPIRequests, 1)
		}
	}()

	// Wait for some adds to happen, then swap.
	<-done

	deltas := cb.SwapAllBackends()
	remaining := cb.Load("b1", FieldAPIRequests)
	total := deltas["b1"].APIRequests + remaining

	if total != iterations {
		t.Errorf("total = %d, want %d (swap got %d, remaining %d)",
			total, iterations, deltas["b1"].APIRequests, remaining)
	}
}

// TestSwapAllBackends_Empty verifies that SwapAllBackends works on a backend
// with no accumulated deltas.
func TestSwapAllBackends_Empty(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	deltas := cb.SwapAllBackends()
	if d := deltas["b1"]; d.APIRequests != 0 || d.EgressBytes != 0 || d.IngressBytes != 0 {
		t.Errorf("expected zero deltas, got %+v", d)
	}
}
