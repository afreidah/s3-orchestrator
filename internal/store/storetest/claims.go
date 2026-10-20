// -------------------------------------------------------------------------------
// Copy Claim Stub
//
// Author: Alex Freidah
//
// ClaimPendingCopies walks the ranked candidates and asks the conditional
// insert about each one. A test that already stubs that insert can claim
// copies through the same decision, so a backend it declares full declines
// the same way whether one copy is claimed or several.
// -------------------------------------------------------------------------------

package storetest

import (
	"context"
	"slices"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// CopyClaimer is the slice of a store CheckCopyClaims drives.
type CopyClaimer interface {
	ClaimPendingCopies(ctx context.Context, intents []*core.PendingObject, ranked []string, accepts core.BackendFlags) (core.CopyClaims, error)
	DeletePending(ctx context.Context, intentID string) error
}

// CheckCopyClaims claims two copies over full, roomy and spare, in that
// order, and fails unless full declines and the copies land on roomy and
// spare. full must have a ceiling too small for 100 bytes; the other two must
// have room. The intents are removed when the test ends.
func CheckCopyClaims(t *testing.T, s CopyClaimer, full, roomy, spare string) {
	t.Helper()
	intents := []*core.PendingObject{
		{IntentID: t.Name() + "-primary", ObjectKey: t.Name(), StorageKey: t.Name() + "!primary", SizeBytes: 100},
		{IntentID: t.Name() + "-companion", ObjectKey: t.Name(), StorageKey: t.Name() + "!companion", SizeBytes: 100, Role: core.PendingRoleCompanion},
	}
	claims, err := s.ClaimPendingCopies(context.Background(), intents, []string{full, roomy, spare}, core.AcceptForWrite)
	if err != nil {
		t.Fatalf("ClaimPendingCopies: %v", err)
	}
	for _, p := range claims.Claimed {
		t.Cleanup(func() { _ = s.DeletePending(context.Background(), p.IntentID) })
	}
	got := make([]string, len(claims.Claimed))
	for i, p := range claims.Claimed {
		got[i] = p.BackendName
	}
	if want := []string{roomy, spare}; !slices.Equal(got, want) {
		t.Errorf("claimed on %v, want %v", got, want)
	}
	if want := []string{full}; !slices.Equal(claims.Declined, want) {
		t.Errorf("declined %v, want %v", claims.Declined, want)
	}
}

// ClaimCopiesVia returns a ClaimPendingCopies stub that walks ranked the way
// the store does, asking insert whether each candidate takes the next intent.
func ClaimCopiesVia(insert func(context.Context, *core.PendingObject, core.BackendFlags) (bool, error)) func(context.Context, []*core.PendingObject, []string, core.BackendFlags) (core.CopyClaims, error) {
	return func(ctx context.Context, intents []*core.PendingObject, ranked []string, accepts core.BackendFlags) (core.CopyClaims, error) {
		var out core.CopyClaims
		for _, name := range ranked {
			if len(out.Claimed) == len(intents) {
				break
			}
			p := intents[len(out.Claimed)]
			p.BackendName = name
			fits, err := insert(ctx, p, accepts)
			if err != nil {
				return core.CopyClaims{}, err
			}
			if fits {
				out.Claimed = append(out.Claimed, p)
			} else {
				out.Declined = append(out.Declined, name)
			}
		}
		return out, nil
	}
}
