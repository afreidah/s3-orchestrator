// -------------------------------------------------------------------------------
// Admin Stream - Event Contract Tests
//
// Author: Alex Freidah
//
// Locks the JSON field names of the streamed Event, which the adminctl client
// decodes by name. A rename here is a wire-contract break and must fail loudly.
// -------------------------------------------------------------------------------

package adminstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvent_JSONFieldNames(t *testing.T) {
	t.Parallel()
	e := Event{
		Kind:       KindResult,
		Op:         "backfill-checksums",
		Message:    "msg",
		Processed:  42,
		Outcome:    OutcomeOK,
		Error:      "boom",
		DurationMs: 1500,
		Fields:     map[string]any{"done": true},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"event":"result"`, `"op":"backfill-checksums"`, `"message":"msg"`,
		`"processed":42`, `"outcome":"ok"`, `"error":"boom"`,
		`"duration_ms":1500`, `"fields":{"done":true}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled event missing %s:\n%s", want, got)
		}
	}
}

func TestEvent_OmitsEmptyFields(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Event{Kind: KindProgress, Processed: 10})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if got != `{"event":"progress","processed":10}` {
		t.Errorf("progress event = %s, want only event+processed", got)
	}
}

func TestEvent_RoundTrip(t *testing.T) {
	t.Parallel()
	in := Event{Kind: KindStart, Op: "reconcile"}
	b, _ := json.Marshal(in)
	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Kind != KindStart || out.Op != "reconcile" {
		t.Errorf("round-trip = %+v, want start/reconcile", out)
	}
}
