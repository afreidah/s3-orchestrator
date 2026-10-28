// -------------------------------------------------------------------------------
// Shared - This Instance's Handle on Shared State
//
// Author: Alex Freidah
//
// Pairs the store every instance reads and writes with this instance's id, so a
// report can be filed under the instance that wrote it. A Shared with no store
// is a single instance, and the helpers built on it act locally rather than
// each checking for Redis themselves.
// -------------------------------------------------------------------------------

package sharedstate

import (
	"context"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/instanceid"
)

// Store is where shared state lives: named values with an expiry, and channels
// that carry a message to every subscribed instance. WatchShared calls
// onMessage with a nil payload each time its subscription is established.
// *counter.RedisCounterBackend satisfies it.
type Store interface {
	PutShared(ctx context.Context, name string, value []byte, ttl time.Duration) error
	GetShared(ctx context.Context, name string) ([]byte, error)
	ListShared(ctx context.Context, prefix string) (map[string][]byte, error)
	PublishShared(ctx context.Context, channel string, payload []byte) error
	WatchShared(ctx context.Context, channel string, onMessage func(ctx context.Context, payload []byte))
}

// Shared is this instance's handle on the state it shares with the others. A
// nil Shared, or one built without a store, is a single instance.
type Shared struct {
	store Store
	self  instanceid.ID
}

// New builds the handle over store for the instance self. Pass a nil store
// when there is no Redis, rather than a typed nil, so the instance runs alone.
func New(store Store, self instanceid.ID) *Shared {
	return &Shared{store: store, self: self}
}

// Self returns this instance's id, empty on a nil Shared.
func (s *Shared) Self() instanceid.ID {
	if s == nil {
		return ""
	}
	return s.self
}

// active returns the store, or nil on a single instance.
func (s *Shared) active() Store {
	if s == nil {
		return nil
	}
	return s.store
}
