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

func TestLocalCounterBackend_Add_Accumulates(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.Add("b1", FieldAPIRequests, 3)
	cb.Add("b1", FieldAPIRequests, 7)

	if got := cb.Load("b1", FieldAPIRequests); got != 10 {
		t.Errorf("apiRequests = %d, want 10", got)
	}
}

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

func TestLocalCounterBackend_AddAll_SkipsZero(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	cb.AddAll("b1", 0, 0, 0)

	result := cb.LoadAll("b1")
	if result.APIRequests != 0 || result.EgressBytes != 0 || result.IngressBytes != 0 {
		t.Errorf("expected all zeros, got %+v", result)
	}
}

func TestLocalCounterBackend_LoadAll_UnknownBackend(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	result := cb.LoadAll("unknown")
	if result.APIRequests != 0 || result.EgressBytes != 0 || result.IngressBytes != 0 {
		t.Errorf("expected zero result for unknown backend, got %+v", result)
	}
}

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

func TestLocalCounterBackend_SwapAll_UnknownBackend(t *testing.T) {
	t.Parallel()
	cb := NewLocalCounterBackend([]string{"b1"})

	result := cb.SwapAll("unknown")
	if result.APIRequests != 0 || result.EgressBytes != 0 || result.IngressBytes != 0 {
		t.Errorf("SwapAll on unknown should return zeros, got %+v", result)
	}
}

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
