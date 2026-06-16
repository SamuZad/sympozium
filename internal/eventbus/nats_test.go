package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestIsStreamGoneErr exhaustively covers the error classification used
// by Publish and the Subscribe fetch loop to decide when to resync the
// JetStream stream. Misclassifying here is exactly the bug we just
// fixed — silent breakage after a NATS pod restart.
func TestIsStreamGoneErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("something else"), false},
		{"context canceled", context.Canceled, false},
		{"jetstream.ErrConsumerNotFound", jetstream.ErrConsumerNotFound, false},
		{"jetstream.ErrStreamNotFound", jetstream.ErrStreamNotFound, true},
		{"jetstream.ErrNoStreamResponse", jetstream.ErrNoStreamResponse, true},
		{"nats.ErrNoResponders", nats.ErrNoResponders, true},
		{"wrapped ErrStreamNotFound", fmt.Errorf("ctx: %w", jetstream.ErrStreamNotFound), true},
		{"wrapped ErrNoResponders", fmt.Errorf("publish: %w", nats.ErrNoResponders), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStreamGoneErr(tc.err); got != tc.want {
				t.Errorf("isStreamGoneErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStreamConfig pins the stream config so accidental edits (wrong
// subject pattern, missing retention, etc.) fail loudly in CI rather
// than silently breaking message routing.
func TestStreamConfig(t *testing.T) {
	cfg := streamConfig()
	if cfg.Name != streamName {
		t.Errorf("Name = %q, want %q", cfg.Name, streamName)
	}
	if len(cfg.Subjects) != 1 || cfg.Subjects[0] != "sympozium.>" {
		t.Errorf("Subjects = %v, want [sympozium.>]", cfg.Subjects)
	}
	if cfg.Retention != jetstream.LimitsPolicy {
		t.Errorf("Retention = %v, want LimitsPolicy", cfg.Retention)
	}
	if cfg.MaxAge != 24*time.Hour {
		t.Errorf("MaxAge = %v, want 24h", cfg.MaxAge)
	}
	if cfg.Storage != jetstream.FileStorage {
		t.Errorf("Storage = %v, want FileStorage", cfg.Storage)
	}
}

// busWithStubCreator returns a NATSEventBus whose createStream is a
// caller-supplied stub. This lets us drive resyncStream and friends
// without a live NATS server. The bus is intentionally not "complete":
// js / conn / stream are nil. Tests that don't call methods on those
// fields are fine.
func busWithStubCreator(t *testing.T, create streamCreator) *NATSEventBus {
	t.Helper()
	bus := &NATSEventBus{
		createStream: create,
	}
	bus.streamGen.Store(1)
	return bus
}

// TestResyncStream_DedupesConcurrent verifies that when N goroutines
// observe a stream-gone error at the same generation, only one
// CreateOrUpdateStream round trip actually happens. Without this dedup,
// every subscriber + every in-flight publisher would each pile a
// redundant request onto a recovering NATS server.
func TestResyncStream_DedupesConcurrent(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})

	bus := busWithStubCreator(t, func(_ context.Context, _ jetstream.StreamConfig) (jetstream.Stream, error) {
		// Block until the gate opens so all goroutines pile up on the
		// mutex; the winner does the real work, the losers see the
		// bumped generation and short-circuit.
		<-gate
		calls.Add(1)
		return nil, nil
	})

	const goroutines = 16
	startGen := bus.streamGen.Load()

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := bus.resyncStream(context.Background(), startGen); err != nil {
				t.Errorf("resyncStream returned error: %v", err)
			}
		}()
	}

	// Give the goroutines time to all queue up on streamMu before we
	// release the createStream stub. Without this, some goroutines may
	// not have reached the lock yet when the first call completes, and
	// they would each see a bumped gen and skip — which is the right
	// behavior but doesn't actually exercise the contention path.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("createStream was called %d times, want exactly 1", got)
	}
	if got := bus.streamGen.Load(); got != startGen+1 {
		t.Errorf("streamGen = %d, want %d", got, startGen+1)
	}
}

// TestResyncStream_ForcesWhenKnownGenZero ensures the bootstrap-style
// caller (knownGen=0) always actually runs CreateOrUpdateStream — used
// by NewNATSEventBus's retry loop where we have no prior generation to
// compare against.
func TestResyncStream_ForcesWhenKnownGenZero(t *testing.T) {
	var calls atomic.Int32
	bus := busWithStubCreator(t, func(_ context.Context, _ jetstream.StreamConfig) (jetstream.Stream, error) {
		calls.Add(1)
		return nil, nil
	})

	// Bump the generation a couple of times to simulate prior resyncs.
	bus.streamGen.Store(5)

	if _, _, err := bus.resyncStream(context.Background(), 0); err != nil {
		t.Fatalf("first resync: %v", err)
	}
	if _, _, err := bus.resyncStream(context.Background(), 0); err != nil {
		t.Fatalf("second resync: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("createStream calls = %d, want 2 (knownGen=0 must always force)", got)
	}
}

// TestResyncStream_PropagatesError verifies that when CreateOrUpdateStream
// fails, the generation is NOT bumped (so the next caller will retry)
// and the wrapped error is returned.
func TestResyncStream_PropagatesError(t *testing.T) {
	sentinel := errors.New("nats unreachable")
	bus := busWithStubCreator(t, func(_ context.Context, _ jetstream.StreamConfig) (jetstream.Stream, error) {
		return nil, sentinel
	})

	startGen := bus.streamGen.Load()

	_, _, err := bus.resyncStream(context.Background(), startGen)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}
	if got := bus.streamGen.Load(); got != startGen {
		t.Errorf("streamGen bumped on failure: got %d, want %d", got, startGen)
	}
}

// TestResyncStream_SkipsWhenAlreadyAdvanced exercises the explicit
// dedup branch: a caller that observed an older generation than the
// current one must not trigger a fresh CreateOrUpdateStream.
func TestResyncStream_SkipsWhenAlreadyAdvanced(t *testing.T) {
	var calls atomic.Int32
	bus := busWithStubCreator(t, func(_ context.Context, _ jetstream.StreamConfig) (jetstream.Stream, error) {
		calls.Add(1)
		return nil, nil
	})

	bus.streamGen.Store(10)

	// We claim to have last seen generation 3 — current is 10, so
	// resync should be a no-op and return the cached state.
	stream, gen, err := bus.resyncStream(context.Background(), 3)
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	if gen != 10 {
		t.Errorf("returned gen = %d, want 10", gen)
	}
	if stream != nil {
		// stream is nil in this fake bus; we just assert we didn't
		// somehow synthesize a non-nil value.
		t.Errorf("returned stream = %v, want nil (cached)", stream)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("createStream called %d times, want 0 (skip path)", got)
	}
}

// TestTopicToSubject pins the subject-namespacing scheme; channels and
// subscribers on different processes must agree on this mapping.
func TestTopicToSubject(t *testing.T) {
	cases := []struct {
		topic string
		want  string
	}{
		{"agent.run.completed", "sympozium.agent.run.completed"},
		{"channel.message.received", "sympozium.channel.message.received"},
		{"", "sympozium."},
	}
	for _, tc := range cases {
		if got := topicToSubject(tc.topic); got != tc.want {
			t.Errorf("topicToSubject(%q) = %q, want %q", tc.topic, got, tc.want)
		}
	}
}
