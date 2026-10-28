// -------------------------------------------------------------------------------
// Topic - A Message Every Instance Applies
//
// Author: Alex Freidah
//
// A change made on one instance that the others have to apply to their own
// state, such as a rebuilt credential registry or an emptied cache. Pub/sub
// keeps nothing for an instance that was not subscribed when a message went
// out, so a watcher is told to resync whenever it may have missed one, and
// each topic decides what resyncing means for the state it carries.
// -------------------------------------------------------------------------------

package sharedstate

import (
	"context"
	"encoding/json"
	"fmt"
)

// Topic carries messages of type T on one channel.
type Topic[T any] struct {
	shared  *Shared
	channel string
}

// NewTopic names the channel a topic's messages travel on.
func NewTopic[T any](s *Shared, channel string) Topic[T] {
	return Topic[T]{shared: s, channel: channel}
}

// Publish sends msg to every instance watching the topic, the sender included,
// so applying a message must be safe to repeat. A no-op on a single instance.
func (t Topic[T]) Publish(ctx context.Context, msg T) error {
	store := t.shared.active()
	if store == nil {
		return nil
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode %s message: %w", t.channel, err)
	}
	return store.PublishShared(ctx, t.channel, payload)
}

// Watch calls apply for every message, and resync whenever this instance may
// have missed one: each time the subscription is established, and for a
// message it cannot read, such as one an older version sent. Blocks until ctx
// is done, on a single instance too, so a supervisor does not restart it.
func (t Topic[T]) Watch(ctx context.Context, apply func(context.Context, T), resync func(context.Context)) {
	store := t.shared.active()
	if store == nil {
		<-ctx.Done()
		return
	}
	store.WatchShared(ctx, t.channel, func(ctx context.Context, payload []byte) {
		var msg T
		if payload == nil || json.Unmarshal(payload, &msg) != nil {
			resync(ctx)
			return
		}
		apply(ctx, msg)
	})
}
