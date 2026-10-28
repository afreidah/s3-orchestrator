// -------------------------------------------------------------------------------
// RedisCounterBackend Shared Messages and Listing Tests
//
// Author: Alex Freidah
//
// Covers the shared-state calls beyond a single value: publishing a message, a
// watcher that hears every message and every (re)subscription, since pub/sub
// keeps nothing for a subscriber that was away, and listing values by prefix.
// -------------------------------------------------------------------------------

package counter

import (
	"context"
	"errors"
	"slices"
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
// resubscription after it. A subscription arrives as a nil payload and a
// message as its own; an unsubscribe confirmation and a pong arrive as nothing.
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
	var payloads []string
	done := make(chan struct{})
	go func() {
		r.WatchShared(ctx, "prov", func(_ context.Context, payload []byte) {
			if payload == nil {
				payloads = append(payloads, "<nil>")
			} else {
				payloads = append(payloads, string(payload))
			}
			if len(payloads) == 3 {
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
	if want := []string{"<nil>", "changed", "<nil>"}; !slices.Equal(payloads, want) {
		t.Errorf("payloads = %q, want %q", payloads, want)
	}
	if !sub.closed.Load() {
		t.Error("subscription not closed when the watcher returned")
	}
}

// TestPublishShared_SendsPayloadAndDeclinesInFallback publishes the payload on
// the shared channel, surfaces a Redis error, and declines outright while the
// backend is in fallback.
func TestPublishShared_SendsPayloadAndDeclinesInFallback(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	newBackend := func() *RedisCounterBackend {
		return &RedisCounterBackend{client: mock, prefix: "p", cb: newTestCB()}
	}
	ctx := context.Background()

	mock.EXPECT().Publish(gomock.Any(), "p:shared:prov", []byte("m")).Return(redis.NewIntResult(2, nil))
	if err := newBackend().PublishShared(ctx, "prov", []byte("m")); err != nil {
		t.Fatalf("PublishShared: %v", err)
	}

	boom := errors.New("redis down")
	mock.EXPECT().Publish(gomock.Any(), "p:shared:prov", []byte("m")).Return(redis.NewIntResult(0, boom))
	if err := newBackend().PublishShared(ctx, "prov", []byte("m")); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}

	fallback := newBackend()
	fallback.setFallback(true)
	if err := fallback.PublishShared(ctx, "prov", []byte("m")); !errors.Is(err, ErrSharedStateUnavailable) {
		t.Errorf("err = %v in fallback, want ErrSharedStateUnavailable", err)
	}
}

// TestListShared_PagesScanAndSkipsExpired pages through the scan, reads each
// value back under its name, leaves out one that expired between the two, and
// escapes the prefix.
func TestListShared_PagesScanAndSkipsExpired(t *testing.T) {
	ctrl := gomock.NewController(t)
	mock := NewMockRedisClient(ctrl)
	r := &RedisCounterBackend{client: mock, prefix: "p*", cb: newTestCB()}
	ctx := context.Background()

	match := `p\*:shared:inst:*`
	mock.EXPECT().Scan(gomock.Any(), uint64(0), match, int64(scanCount)).
		Return(redis.NewScanCmdResult([]string{"p*:shared:inst:a", "p*:shared:inst:gone"}, 7, nil))
	mock.EXPECT().Scan(gomock.Any(), uint64(7), match, int64(scanCount)).
		Return(redis.NewScanCmdResult([]string{"p*:shared:inst:b"}, 0, nil))
	mock.EXPECT().Get(gomock.Any(), "p*:shared:inst:a").Return(redis.NewStringResult("A", nil))
	mock.EXPECT().Get(gomock.Any(), "p*:shared:inst:gone").Return(redis.NewStringResult("", redis.Nil))
	mock.EXPECT().Get(gomock.Any(), "p*:shared:inst:b").Return(redis.NewStringResult("B", nil))

	got, err := r.ListShared(ctx, "inst:")
	if err != nil {
		t.Fatalf("ListShared: %v", err)
	}
	if len(got) != 2 || string(got["inst:a"]) != "A" || string(got["inst:b"]) != "B" {
		t.Errorf("ListShared = %q", got)
	}

	boom := errors.New("redis down")
	mock.EXPECT().Scan(gomock.Any(), uint64(0), match, int64(scanCount)).Return(redis.NewScanCmdResult(nil, 0, boom))
	if _, err := r.ListShared(ctx, "inst:"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}

	r.setFallback(true)
	if _, err := r.ListShared(ctx, "inst:"); !errors.Is(err, ErrSharedStateUnavailable) {
		t.Errorf("err = %v in fallback, want ErrSharedStateUnavailable", err)
	}
}
