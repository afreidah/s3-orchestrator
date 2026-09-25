// -------------------------------------------------------------------------------
// RedisCounterBackend Change Notification Tests
//
// Author: Alex Freidah
//
// Covers NotifyShared and WatchShared: publishing a change, and a watcher that
// reacts to every message and to every (re)subscription, since pub/sub keeps
// nothing for a subscriber that was away.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/mock/gomock"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// scriptedSubscription replays a fixed sequence of Receive results, then
// blocks until the context ends. closed records that the watcher released it.
type scriptedSubscription struct {
	steps  []receiveStep
	next   int
	closed atomic.Bool
}

// receiveStep is one Receive result.
type receiveStep struct {
	msg any
	err error
}

func (s *scriptedSubscription) Receive(ctx context.Context) (any, error) {
	if s.next < len(s.steps) {
		step := s.steps[s.next]
		s.next++
		return step.msg, step.err
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *scriptedSubscription) Close() error {
	s.closed.Store(true)
	return nil
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestWatchShared_ReactsToSubscribeMessagesAndReconnects drives a watcher
// through its first subscription, a message, a dropped connection, and the
// resubscription after it. Every subscription and every message is a change;
// an unsubscribe confirmation and a pong are not.
func TestWatchShared_ReactsToSubscribeMessagesAndReconnects(t *testing.T) {
	sub := &scriptedSubscription{steps: []receiveStep{
		{msg: &redis.Subscription{Kind: "subscribe", Channel: "p:shared:prov"}},
		{msg: &redis.Message{Channel: "p:shared:prov", Payload: "changed"}},
		{msg: &redis.Pong{}},
		{err: errors.New("connection reset")},
		{msg: &redis.Subscription{Kind: "subscribe", Channel: "p:shared:prov"}},
		{msg: &redis.Subscription{Kind: "unsubscribe", Channel: "p:shared:prov"}},
	}}
	var subscribedTo string
	r := &RedisCounterBackend{
		prefix: "p",
		subscribe: func(_ context.Context, channel string) subscription {
			subscribedTo = channel
			return sub
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var changes atomic.Int32
	done := make(chan struct{})
	go func() {
		r.WatchShared(ctx, "prov", func(context.Context) {
			if changes.Add(1) == 3 {
				cancel()
			}
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("WatchShared did not return after its context ended")
	}
	if subscribedTo != "p:shared:prov" {
		t.Errorf("subscribed to %q, want p:shared:prov", subscribedTo)
	}
	if got := changes.Load(); got != 3 {
		t.Errorf("onChange ran %d times, want 3: subscribe, message, resubscribe", got)
	}
	if !sub.closed.Load() {
		t.Error("subscription not closed when the watcher returned")
	}
}

// TestNotifyShared publishes on the shared channel, surfaces a Redis error,
// and declines outright while the backend is in fallback.
func TestNotifyShared(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	newBackend := func() *RedisCounterBackend {
		return &RedisCounterBackend{client: mock, prefix: "p", cb: newTestCB()}
	}
	ctx := context.Background()

	mock.EXPECT().Publish(gomock.Any(), "p:shared:prov", "changed").Return(redis.NewIntResult(2, nil))
	if err := newBackend().NotifyShared(ctx, "prov"); err != nil {
		t.Fatalf("NotifyShared: %v", err)
	}

	boom := errors.New("redis down")
	mock.EXPECT().Publish(gomock.Any(), "p:shared:prov", "changed").Return(redis.NewIntResult(0, boom))
	if err := newBackend().NotifyShared(ctx, "prov"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}

	fallback := newBackend()
	fallback.setFallback(true)
	if err := fallback.NotifyShared(ctx, "prov"); !errors.Is(err, ErrSharedStateUnavailable) {
		t.Errorf("err = %v in fallback, want ErrSharedStateUnavailable", err)
	}
}
