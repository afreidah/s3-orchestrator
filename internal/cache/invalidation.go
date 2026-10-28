// -------------------------------------------------------------------------------
// Object Cache - Invalidations Carried to Every Instance
//
// Author: Alex Freidah
//
// Each instance holds its own object cache, so an operator's flush or
// invalidation reaches every cache only when it is announced to the other
// instances. The instance that took the request applies it and announces it;
// the others apply it as it arrives, and empty their whole cache when they may
// have missed one.
// -------------------------------------------------------------------------------

package cache

import "github.com/afreidah/s3-orchestrator/internal/sharedstate"

// invalidationChannel is the shared-state channel invalidations travel on.
const invalidationChannel = "cache"

// Invalidation is one operator change to the cache. All empties it; otherwise
// Key drops every cached copy of one object key and Prefix every entry under a
// key prefix.
type Invalidation struct {
	All    bool   `json:"all,omitempty"`
	Key    string `json:"key,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

// Apply drops what inv names from c and returns how many entries went. A key
// invalidation reports none, since the cache does not count them.
func (inv Invalidation) Apply(c ObjectCache) int {
	switch {
	case inv.All:
		return c.Clear()
	case inv.Key != "":
		c.Invalidate(inv.Key)
		return 0
	case inv.Prefix != "":
		return c.InvalidatePrefix(inv.Prefix)
	default:
		return 0
	}
}

// InvalidationTopic is the topic invalidations are announced on.
func InvalidationTopic(s *sharedstate.Shared) sharedstate.Topic[Invalidation] {
	return sharedstate.NewTopic[Invalidation](s, invalidationChannel)
}
