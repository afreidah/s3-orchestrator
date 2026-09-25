// -------------------------------------------------------------------------------
// Provisioning Watch Service Tests
//
// Author: Alex Freidah
//
// Covers the service that rebuilds this instance's provisioning view when the
// shared channel reports a change.
// -------------------------------------------------------------------------------

package di

import (
	"context"
	"errors"
	"testing"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// firingChannel is a sharedChannelWatcher that reports a fixed number of
// changes, then returns as the real watcher does once ctx ends.
type firingChannel struct {
	changes int
	channel string
}

func (f *firingChannel) WatchShared(ctx context.Context, channel string, onChange func(context.Context)) {
	f.channel = channel
	for range f.changes {
		onChange(ctx)
	}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestProvisioningWatcher_AppliesOnEveryChange rebuilds once per reported
// change on the provisioning channel, and keeps watching when a rebuild fails.
func TestProvisioningWatcher_AppliesOnEveryChange(t *testing.T) {
	t.Parallel()
	channel := &firingChannel{changes: 3}
	applied := 0
	svc := newProvisioningWatcher(channel, func(context.Context) error {
		applied++
		if applied == 2 {
			return errors.New("store unavailable")
		}
		return nil
	})

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if channel.channel != provisioningChannel {
		t.Errorf("watched %q, want %q", channel.channel, provisioningChannel)
	}
	if applied != 3 {
		t.Errorf("applied %d times, want 3", applied)
	}
}
