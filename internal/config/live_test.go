// -------------------------------------------------------------------------------
// Running Configuration Tests
//
// Author: Alex Freidah
//
// Covers Live: a section reader follows every Store, and Update keeps the
// sections its edit did not touch.
// -------------------------------------------------------------------------------

package config

import "testing"

// TestLive_SectionsFollowEveryStore verifies a section read before a reload
// reports the reloaded value afterwards, which is what lets components hold a
// reader instead of a copy.
func TestLive_SectionsFollowEveryStore(t *testing.T) {
	t.Parallel()
	live := NewLive(&Config{})
	integrity, rebalance, replication := live.Integrity(), live.Rebalance(), live.Replication()
	lifecycle, flush := live.Lifecycle(), live.UsageFlush()

	live.Store(&Config{
		Integrity:   IntegrityConfig{Enabled: true},
		Rebalance:   RebalanceConfig{Enabled: true},
		Replication: ReplicationConfig{Factor: 3},
		Lifecycle:   LifecycleConfig{BatchSize: 7},
		UsageFlush:  UsageFlushConfig{AdaptiveEnabled: true},
	})
	if !integrity.Config().Enabled || !rebalance.Config().Enabled || replication.Config().Factor != 3 ||
		lifecycle.Config().BatchSize != 7 || !flush.Config().AdaptiveEnabled {
		t.Errorf("sections did not follow the store: %+v", live.Load())
	}
}

// TestLive_UpdateKeepsOtherSections verifies Update stores an edited copy,
// leaving the sections the edit did not touch and the previous value intact.
func TestLive_UpdateKeepsOtherSections(t *testing.T) {
	t.Parallel()
	live := NewLive(&Config{})
	before := live.Load()

	live.Update(func(c *Config) { c.Replication.Factor = 2 })
	live.Update(func(c *Config) { c.Integrity.Enabled = true })
	if got := live.Load(); got.Replication.Factor != 2 || !got.Integrity.Enabled {
		t.Errorf("config = %+v, want factor 2 and integrity enabled", got)
	}
	if before.Replication.Factor != 0 {
		t.Error("Update changed the value it replaced")
	}
}
