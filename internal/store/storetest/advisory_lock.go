// -------------------------------------------------------------------------------
// Advisory Lock Check
//
// Author: Alex Freidah
//
// An advisory lock admits one holder per ID: a second caller asking for the
// held ID is refused without waiting, a caller asking for another ID is not
// blocked by it, and the ID is free again once the holder returns. Both
// engines run these cases against a real store.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// CheckAdvisoryLock runs the advisory lock cases against l, holding lockID
// and asking for otherID alongside it. The two IDs must differ and be held by
// nothing else while the check runs.
func CheckAdvisoryLock(t *testing.T, l core.AdvisoryLocker, lockID, otherID int64) {
	t.Helper()
	ctx := context.Background()
	acquired, err := l.WithAdvisoryLock(ctx, lockID, func(ctx context.Context) error {
		if got, err := l.WithAdvisoryLock(ctx, lockID, refuseRun(t)); got || err != nil {
			t.Errorf("same ID while held: acquired = %v, err = %v; want refused", got, err)
		}
		ran := false
		got, err := l.WithAdvisoryLock(ctx, otherID, func(context.Context) error { ran = true; return nil })
		if !got || err != nil || !ran {
			t.Errorf("other ID while held: acquired = %v, err = %v, ran = %v; want it to run", got, err, ran)
		}
		return nil
	})
	if !acquired || err != nil {
		t.Fatalf("first hold: acquired = %v, err = %v", acquired, err)
	}
	if got, err := l.WithAdvisoryLock(ctx, lockID, func(context.Context) error { return nil }); !got || err != nil {
		t.Errorf("after release: acquired = %v, err = %v; want the ID free again", got, err)
	}
}

// refuseRun is the body of a hold that must never be granted.
func refuseRun(t *testing.T) func(context.Context) error {
	return func(context.Context) error {
		t.Error("a refused hold ran its body")
		return nil
	}
}
