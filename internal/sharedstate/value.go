// -------------------------------------------------------------------------------
// Value - One Fleet-Wide Value Written by One Instance at a Time
//
// Author: Alex Freidah
//
// A value every instance reads and, normally, one instance writes: the one
// holding the lock for the work that computes it. Stored as JSON under a fixed
// name and expiring a while after its last write, so a value nothing refreshes
// any more goes away on its own.
// -------------------------------------------------------------------------------

package sharedstate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Value is one fleet-wide value of type T under a fixed name.
type Value[T any] struct {
	shared *Shared
	name   string
	ttl    time.Duration
}

// NewValue names a shared value that expires ttl after its last Put.
func NewValue[T any](s *Shared, name string, ttl time.Duration) Value[T] {
	return Value[T]{shared: s, name: name, ttl: ttl}
}

// Put stores val for every instance to read. A no-op on a single instance,
// which has no one to share it with.
func (v Value[T]) Put(ctx context.Context, val *T) error {
	store := v.shared.active()
	if store == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return fmt.Errorf("encode %s: %w", v.name, err)
	}
	return store.PutShared(ctx, v.name, data, v.ttl)
}

// Get returns the stored value. It returns nil on a single instance, and when
// nothing is stored or the value has expired.
func (v Value[T]) Get(ctx context.Context) (*T, error) {
	store := v.shared.active()
	if store == nil {
		return nil, nil
	}
	data, err := store.GetShared(ctx, v.name)
	if err != nil || data == nil {
		return nil, err
	}
	val := new(T)
	if err := json.Unmarshal(data, val); err != nil {
		return nil, fmt.Errorf("decode %s: %w", v.name, err)
	}
	return val, nil
}
