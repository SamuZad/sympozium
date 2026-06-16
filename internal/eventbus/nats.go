package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	streamName    = "sympozium"
	consumerGroup = "sympozium-workers"
)

// streamCreator is the JetStream operation used to (re)create the stream.
// Stored as a func field so tests can drive resyncStream without a live
// NATS server. In production it is bound to jetstream.JetStream.CreateOrUpdateStream.
type streamCreator func(ctx context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error)

// NATSEventBus implements EventBus using NATS JetStream.
type NATSEventBus struct {
	conn *nats.Conn
	js   jetstream.JetStream

	createStream streamCreator

	// streamMu guards stream and serializes resyncStream so concurrent
	// callers don't race to recreate the JetStream stream after the
	// NATS server loses it (e.g. ephemeral storage + pod restart).
	streamMu sync.Mutex
	stream   jetstream.Stream

	// streamGen is bumped on every successful resyncStream. Stored as
	// an atomic so the hot Publish path can snapshot it without taking
	// streamMu. Concurrent callers that observed the same generation as
	// the resync-winner skip the redundant CreateOrUpdateStream round
	// trip.
	streamGen atomic.Uint64
}

// streamConfig is the single source of truth for the JetStream stream we
// own. Used at initial bootstrap and by every resyncStream call.
func streamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"sympozium.>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  1,
	}
}

// NewNATSEventBus creates a new NATS JetStream event bus.
func NewNATSEventBus(url string) (*NATSEventBus, error) {
	// MaxReconnects=-1 keeps the underlying NATS connection trying
	// forever rather than giving up after a fixed number of attempts.
	// Combined with resyncStream, this lets the bus survive arbitrary
	// NATS outages (pod restart, network blip, etc.) without leaving
	// subscribers silently dead.
	nc, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}

	bus := &NATSEventBus{
		conn:         nc,
		js:           js,
		createStream: js.CreateOrUpdateStream,
	}

	// Retry stream creation — NATS may not be fully ready yet.
	cfg := streamConfig()
	var stream jetstream.Stream
	for attempt := 0; attempt < 10; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stream, err = bus.createStream(ctx, cfg)
		cancel()
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("creating JetStream stream after retries: %w", err)
	}
	bus.stream = stream
	bus.streamGen.Store(1)

	return bus, nil
}

// isStreamGoneErr reports whether err indicates the server-side stream
// no longer exists (e.g. NATS pod restarted with ephemeral storage) and
// the caller should resync via resyncStream before retrying.
//
// ErrNoResponders is included because when JetStream has reloaded but
// doesn't yet know about our stream, requests come back with no
// responders rather than a typed not-found error.
func isStreamGoneErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		errors.Is(err, nats.ErrNoResponders)
}

// snapshotStream returns the current stream handle and its generation
// without holding the lock past the snapshot. Callers pass the
// generation back to resyncStream so concurrent recoveries collapse to
// a single CreateOrUpdateStream call.
func (n *NATSEventBus) snapshotStream() (jetstream.Stream, uint64) {
	n.streamMu.Lock()
	defer n.streamMu.Unlock()
	return n.stream, n.streamGen.Load()
}

// resyncStream re-creates the JetStream stream handle against the
// current server. If another goroutine has already resynced past
// knownGen by the time we acquire the lock, we return the current
// handle without a redundant network round-trip. Pass knownGen=0 to
// force a resync (e.g. from tests).
func (n *NATSEventBus) resyncStream(ctx context.Context, knownGen uint64) (jetstream.Stream, uint64, error) {
	n.streamMu.Lock()
	defer n.streamMu.Unlock()

	if knownGen != 0 && n.streamGen.Load() > knownGen {
		return n.stream, n.streamGen.Load(), nil
	}

	stream, err := n.createStream(ctx, streamConfig())
	if err != nil {
		return nil, n.streamGen.Load(), fmt.Errorf("resyncing JetStream stream %q: %w", streamName, err)
	}
	n.stream = stream
	newGen := n.streamGen.Add(1)
	log.Printf("eventbus: JetStream stream %q resynced (generation=%d; server-side state was lost)", streamName, newGen)
	return stream, newGen, nil
}

// Publish sends an event to the NATS JetStream stream.
// Trace context from ctx is automatically injected into NATS message headers.
//
// On the happy path Publish takes no locks. If the server-side stream
// has gone missing (NATS pod restarted with ephemeral storage, etc.)
// we resync the stream and retry once before returning the error.
func (n *NATSEventBus) Publish(ctx context.Context, topic string, event *Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}

	subject := topicToSubject(topic)
	msg := &nats.Msg{
		Subject: subject,
		Data:    data,
		Header:  nats.Header{},
	}
	InjectTraceContext(ctx, msg.Header)

	// Snapshot the generation before publishing so a concurrent resync
	// triggered by another failed publisher is observable to us and we
	// don't redundantly resync ourselves.
	gen := n.streamGen.Load()

	if _, err := n.js.PublishMsg(ctx, msg); err != nil {
		if !isStreamGoneErr(err) {
			return fmt.Errorf("publishing to %s: %w", subject, err)
		}
		log.Printf("eventbus: publish to %q failed (%v); resyncing stream and retrying", subject, err)
		if _, _, resyncErr := n.resyncStream(ctx, gen); resyncErr != nil {
			return fmt.Errorf("publishing to %s: stream gone (%v) and resync failed: %w", subject, err, resyncErr)
		}
		if _, retryErr := n.js.PublishMsg(ctx, msg); retryErr != nil {
			return fmt.Errorf("publishing to %s after resync: %w", subject, retryErr)
		}
	}
	return nil
}

// Subscribe returns a channel that receives events for the given topic.
//
// The subscription uses an ephemeral JetStream consumer with a long
// InactiveThreshold so transient NATS disconnects do not cause the
// server to reap the consumer. The fetch loop is resilient to two
// separate "not found" failure modes on the server side:
//
//   - Consumer gone (e.g. liveness lapsed past InactiveThreshold) —
//     ErrConsumerNotFound. We just recreate the consumer.
//   - Stream gone (e.g. NATS pod restarted with ephemeral storage) —
//     ErrStreamNotFound / ErrNoStreamResponse / ErrNoResponders. We
//     resync the stream first, then recreate the consumer.
//
// Without these recovery paths, a brief NATS hiccup would silently and
// permanently break the subscription — every fetch would either get
// ErrConsumerNotFound forever (consumer-only loss) or, worse, the
// goroutine would tight-loop on ErrStreamNotFound with no log line so
// nothing would tell an operator the bus had gone deaf.
func (n *NATSEventBus) Subscribe(ctx context.Context, topic string) (<-chan *Event, error) {
	subject := topicToSubject(topic)

	consumer, gen, err := n.createSubscribeConsumer(ctx, subject)
	if err != nil {
		return nil, fmt.Errorf("creating consumer for %s: %w", subject, err)
	}

	ch := make(chan *Event, 64)

	go func() {
		defer close(ch)
		for {
			msgs, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}

				switch {
				case isStreamGoneErr(err):
					// Stream is gone server-side — recreate it, then
					// the consumer. Logged loudly so operators see why
					// messages stopped flowing.
					log.Printf("eventbus: subscribe to %q fetch failed (%v); resyncing stream and consumer", subject, err)
					recreateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					if _, newGen, rerr := n.resyncStream(recreateCtx, gen); rerr != nil {
						log.Printf("eventbus: stream resync for %q failed: %v", subject, rerr)
					} else if newConsumer, cgen, cerr := n.createSubscribeConsumer(recreateCtx, subject); cerr != nil {
						log.Printf("eventbus: consumer recreate for %q failed after stream resync: %v", subject, cerr)
					} else {
						consumer = newConsumer
						gen = cgen
						log.Printf("eventbus: subscribe to %q recovered (stream gen=%d)", subject, newGen)
					}
					cancel()
				case errors.Is(err, jetstream.ErrConsumerNotFound):
					// Just the consumer was reaped (likely because our
					// liveness lapsed past InactiveThreshold) — recreate
					// it in place. If recreate itself reports the stream
					// is gone, the next iteration's fetch will take the
					// stream-gone branch above.
					recreateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					newConsumer, cgen, cerr := n.createSubscribeConsumer(recreateCtx, subject)
					cancel()
					switch {
					case cerr == nil:
						consumer = newConsumer
						gen = cgen
					case isStreamGoneErr(cerr):
						log.Printf("eventbus: consumer recreate for %q reports stream gone; will resync on next attempt", subject)
					default:
						log.Printf("eventbus: consumer recreate for %q failed: %v", subject, cerr)
					}
				default:
					// Transient fetch errors (timeouts, etc.) — back off
					// silently and retry. These are normal.
				}

				// Back off briefly so we don't tight-loop while NATS
				// recovers, regardless of the branch above.
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}

			for msg := range msgs.Messages() {
				var event Event
				if err := json.Unmarshal(msg.Data(), &event); err != nil {
					msg.Nak()
					continue
				}

				// Extract trace context from NATS message headers so consumers
				// can continue the distributed trace started by the publisher.
				event.Ctx = ExtractTraceContext(ctx, msg.Headers())

				select {
				case ch <- &event:
					msg.Ack()
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// createSubscribeConsumer creates the ephemeral JetStream consumer used
// by Subscribe. Returns the stream generation observed when the
// consumer was created so the caller can later dedupe resyncs against
// it.
//
// InactiveThreshold is set well above the longest disconnect we expect
// (NATS rolling restart, brief network blip) so the server keeps the
// consumer alive even if Fetch stops for a few minutes.
func (n *NATSEventBus) createSubscribeConsumer(ctx context.Context, subject string) (jetstream.Consumer, uint64, error) {
	stream, gen := n.snapshotStream()
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject:     subject,
		AckPolicy:         jetstream.AckExplicitPolicy,
		DeliverPolicy:     jetstream.DeliverNewPolicy,
		InactiveThreshold: time.Hour,
	})
	if err != nil {
		return nil, gen, err
	}
	return cons, gen, nil
}

// Close shuts down the NATS connection.
func (n *NATSEventBus) Close() error {
	n.conn.Close()
	return nil
}

// topicToSubject converts a dotted topic (e.g. "agent.run.completed")
// to a NATS subject under the sympozium namespace (e.g. "sympozium.agent.run.completed").
func topicToSubject(topic string) string {
	return "sympozium." + topic
}
