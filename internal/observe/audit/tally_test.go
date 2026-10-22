// -------------------------------------------------------------------------------
// Audit - Usage Tally Tests
//
// Author: Alex Freidah
//
// Covers the context-scoped tally: concurrent charges, nesting into enclosing
// tallies, the copy it hands out, summing usages, and the log group it renders.
// -------------------------------------------------------------------------------

package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"sync"
	"testing"
)

// TestTally_ConcurrentAdds verifies concurrent charges all land.
func TestTally_ConcurrentAdds(t *testing.T) {
	t.Parallel()
	ctx, tally := WithTally(context.Background())
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			TallyOf(ctx).Add("b1", BackendUsage{APIRequests: 1, EgressBytes: 10})
		})
	}
	wg.Wait()
	want := Usage{"b1": {APIRequests: 50, EgressBytes: 500}}
	if got := tally.Usage(); !reflect.DeepEqual(got, want) {
		t.Errorf("Usage = %+v, want %+v", got, want)
	}
}

// TestTally_NestedAddsReachEnclosing verifies a charge under an inner tally
// also counts on the outer one, and an outer charge stays out of the inner.
func TestTally_NestedAddsReachEnclosing(t *testing.T) {
	t.Parallel()
	ctx, outer := WithTally(context.Background())
	inner, in := WithTally(ctx)

	TallyOf(inner).Add("b1", BackendUsage{IngressBytes: 7})
	TallyOf(ctx).Add("b2", BackendUsage{APIRequests: 1})

	if got, want := in.Usage(), (Usage{"b1": {IngressBytes: 7}}); !reflect.DeepEqual(got, want) {
		t.Errorf("inner = %+v, want %+v", got, want)
	}
	want := Usage{"b1": {IngressBytes: 7}, "b2": {APIRequests: 1}}
	if got := outer.Usage(); !reflect.DeepEqual(got, want) {
		t.Errorf("outer = %+v, want %+v", got, want)
	}
}

// TestTally_NoneOpen verifies a context without a tally reports none, so the
// usage tracker charges nothing extra.
func TestTally_NoneOpen(t *testing.T) {
	t.Parallel()
	if got := TallyOf(context.Background()); got != nil {
		t.Errorf("TallyOf = %v, want nil", got)
	}
}

// TestTally_UsageIsACopy verifies the returned usage does not change when the
// tally keeps counting.
func TestTally_UsageIsACopy(t *testing.T) {
	t.Parallel()
	_, tally := WithTally(context.Background())
	tally.Add("b1", BackendUsage{APIRequests: 1})
	snap := tally.Usage()
	tally.Add("b1", BackendUsage{APIRequests: 1})
	if snap["b1"].APIRequests != 1 {
		t.Errorf("snapshot changed to %+v", snap)
	}
}

// TestUsage_Plus verifies per-backend sums and that neither operand changes.
func TestUsage_Plus(t *testing.T) {
	t.Parallel()
	a := Usage{"b1": {APIRequests: 1, EgressBytes: 5}}
	b := Usage{"b1": {APIRequests: 2}, "b2": {IngressBytes: 3}}
	want := Usage{"b1": {APIRequests: 3, EgressBytes: 5}, "b2": {IngressBytes: 3}}
	if got := a.Plus(b); !reflect.DeepEqual(got, want) {
		t.Errorf("Plus = %+v, want %+v", got, want)
	}
	if a["b1"].APIRequests != 1 || len(a) != 1 {
		t.Errorf("Plus modified its receiver: %+v", a)
	}
	if got := Usage(nil).Plus(nil); got != nil {
		t.Errorf("nil + nil = %+v, want nil", got)
	}
	if got := Usage(nil).Plus(b); !reflect.DeepEqual(got, b) {
		t.Errorf("nil + b = %+v, want %+v", got, b)
	}
}

// TestUsage_Attr verifies the log group shape, and that an empty usage adds
// nothing to the line.
func TestUsage_Attr(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	log.InfoContext(t.Context(), "line", Usage{"b2": {APIRequests: 1}, "b1": {APIRequests: 2, EgressBytes: 9, IngressBytes: 4}}.Attr())
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"b1": map[string]any{"api_requests": 2.0, "egress_bytes": 9.0, "ingress_bytes": 4.0},
		"b2": map[string]any{"api_requests": 1.0, "egress_bytes": 0.0, "ingress_bytes": 0.0},
	}
	if !reflect.DeepEqual(entry["usage"], want) {
		t.Errorf("usage = %v, want %v", entry["usage"], want)
	}

	buf.Reset()
	log.InfoContext(t.Context(), "line", Usage(nil).Attr())
	entry = nil
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := entry["usage"]; ok {
		t.Errorf("empty usage rendered a group: %v", entry)
	}
}
