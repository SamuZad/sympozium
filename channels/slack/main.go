// Package main is the entry point for the Slack channel pod.
//
// It supports two modes of receiving messages:
//
//   - **Socket Mode** (preferred): The pod opens an outbound WebSocket to
//     Slack, so no public URL or ingress is needed. Requires SLACK_APP_TOKEN
//     (xapp-...) in addition to SLACK_BOT_TOKEN (xoxb-...).
//
//   - **Events API fallback**: If SLACK_APP_TOKEN is not set, the pod
//     starts an HTTP server on :3000 and expects Slack to POST events to
//     /slack/events. This requires a publicly reachable URL.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/sympozium-ai/sympozium/internal/channel"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
	"github.com/sympozium-ai/sympozium/pkg/telemetry"
)

var slackTracer = otel.Tracer("sympozium.ai/channel-slack")

// SlackChannel implements the Slack channel using Socket Mode or the Events API.
type SlackChannel struct {
	channel.BaseChannel
	BotToken string
	AppToken string // xapp-... token for Socket Mode (optional)
	BotID    string // resolved at startup via auth.test, used for @-mention detection
	log      logr.Logger
	client   *http.Client
	healthy  bool
	mu       sync.RWMutex
	cfg      *slackConfig
	threads  *threadEngagement
}

func main() {
	var instanceName string
	var eventBusURL string
	var botToken string
	var appToken string
	var listenAddr string

	flag.StringVar(&instanceName, "instance", os.Getenv("INSTANCE_NAME"), "Agent name")
	flag.StringVar(&eventBusURL, "event-bus-url", os.Getenv("EVENT_BUS_URL"), "Event bus URL")
	flag.StringVar(&botToken, "bot-token", os.Getenv("SLACK_BOT_TOKEN"), "Slack bot token (xoxb-...)")
	flag.StringVar(&appToken, "app-token", os.Getenv("SLACK_APP_TOKEN"), "Slack app token (xapp-...) for Socket Mode")
	flag.StringVar(&listenAddr, "addr", ":3000", "Listen address for Events API fallback")
	flag.Parse()

	if botToken == "" {
		fmt.Fprintln(os.Stderr, "SLACK_BOT_TOKEN is required")
		os.Exit(1)
	}

	log := zap.New(zap.UseDevMode(false)).WithName("channel-slack")

	bus, err := eventbus.NewNATSEventBus(eventBusURL)
	if err != nil {
		log.Error(err, "failed to connect to event bus")
		os.Exit(1)
	}
	defer bus.Close()

	ch := &SlackChannel{
		BaseChannel: channel.BaseChannel{
			ChannelType:  "slack",
			InstanceName: instanceName,
			EventBus:     bus,
		},
		BotToken: botToken,
		AppToken: appToken,
		log:      log,
		client:   &http.Client{Timeout: 30 * time.Second},
		cfg:      loadSlackConfig(log),
		threads:  newThreadEngagement(24 * time.Hour),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Periodically evict stale thread-engagement entries. Lighter than
	// scanning the map on every inbound message under load.
	go ch.threads.sweep(ctx, 5*time.Minute)

	// Resolve the bot's own user ID via auth.test so we can detect
	// @-mentions in inbound text. We retry a few times with backoff
	// because transient network/Slack errors at startup should not
	// silently disable mention detection.
	//
	// When the operator has configured SLACK_ALLOWED_TRIGGERS to
	// include "mention", a missing bot ID means *every* message gets
	// classified as "channel" and dropped — the bot would appear dead.
	// In that case we exit non-zero so Kubernetes restarts the pod
	// rather than running in a broken state.
	if id, err := resolveBotUserIDWithRetry(ctx, ch.client, botToken, 5, time.Second); err != nil {
		if ch.cfg.allowedTriggers[string(kindMention)] {
			log.Error(err, "failed to resolve bot user ID via auth.test; SLACK_ALLOWED_TRIGGERS includes \"mention\" so the bot cannot function — exiting for pod restart")
			os.Exit(1)
		}
		log.Error(err, "failed to resolve bot user ID via auth.test; @-mention detection disabled")
	} else {
		ch.BotID = id
		log.Info("Resolved Slack bot user ID", "botId", id)
	}

	// Initialize OpenTelemetry.
	tel, telErr := telemetry.Init(ctx, telemetry.Config{})
	if telErr != nil {
		log.Error(telErr, "failed to init telemetry, continuing without")
	} else {
		defer tel.Shutdown(context.Background())
	}

	// Health server (runs in all modes)
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			ch.mu.RLock()
			h := ch.healthy
			ch.mu.RUnlock()
			if h {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
		_ = http.ListenAndServe(":8080", mux)
	}()

	go ch.handleOutbound(ctx)

	if appToken != "" {
		log.Info("Starting Slack channel in Socket Mode", "instance", instanceName)
		if err := ch.runSocketMode(ctx); err != nil {
			log.Error(err, "socket mode failed")
		}
	} else {
		log.Info("Starting Slack channel in Events API mode (no SLACK_APP_TOKEN)",
			"instance", instanceName, "addr", listenAddr)
		ch.runEventsAPI(ctx, listenAddr)
	}
}

// ---------------------------------------------------------------------------
// Socket Mode — outbound WebSocket, no public URL needed
// ---------------------------------------------------------------------------

// openSocketModeConnection requests a WebSocket URL from Slack and dials it.
func (sc *SlackChannel) openSocketModeConnection(ctx context.Context) (*websocket.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://slack.com/api/apps.connections.open", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sc.AppToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := sc.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apps.connections.open: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
		Err string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding connection response: %w", err)
	}
	if !body.OK {
		return nil, fmt.Errorf("apps.connections.open: %s", body.Err)
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, body.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	return conn, nil
}

// runSocketMode connects via Socket Mode and reconnects on failure.
func (sc *SlackChannel) runSocketMode(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn, err := sc.openSocketModeConnection(ctx)
		if err != nil {
			sc.log.Error(err, "failed to open Socket Mode connection, retrying in 5s")
			sc.setHealthy(false, err.Error())
			time.Sleep(5 * time.Second)
			continue
		}

		sc.log.Info("Socket Mode connected")
		sc.setHealthy(true, "")

		if err := sc.readSocketMode(ctx, conn); err != nil {
			sc.log.Error(err, "socket mode read error, reconnecting")
			sc.setHealthy(false, err.Error())
		}
		_ = conn.Close()
	}
}

// socketEnvelope is the structure Slack sends over the Socket Mode WebSocket.
type socketEnvelope struct {
	EnvelopeID string          `json:"envelope_id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

// readSocketMode reads messages from the WebSocket until an error or ctx cancel.
func (sc *SlackChannel) readSocketMode(ctx context.Context, conn *websocket.Conn) error {
	// Handle WebSocket pings from Slack to keep the connection alive.
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})
	conn.SetPingHandler(func(msg string) error {
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(msg), time.Now().Add(10*time.Second))
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}

		var env socketEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}

		switch env.Type {
		case "hello":
			sc.log.Info("Received hello from Slack Socket Mode")

		case "disconnect":
			sc.log.Info("Slack requested disconnect, will reconnect")
			return nil

		case "events_api":
			// Acknowledge immediately.
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
			sc.handleSocketEvent(ctx, env.Payload)

		case "interactive", "slash_commands":
			// Acknowledge; we don't handle these yet.
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			_ = conn.WriteMessage(websocket.TextMessage, ack)
		}
	}
}

// gateAndBuildInbound runs the shared gating pipeline for one Slack
// message event and, on accept, returns the InboundMessage ready to
// publish to the event bus. Logging of accept/drop decisions happens
// here so both Socket Mode and Events API paths get consistent
// observability. Returns (msg, false) when the message must be
// dropped.
func (sc *SlackChannel) gateAndBuildInbound(
	user, channelID, threadTS, ts, channelType, text string,
) (channel.InboundMessage, bool) {
	decision, reason := evaluateInbound(sc.cfg, sc.threads,
		sc.BotID, user, channelID, threadTS, ts, channelType, text)

	kvs := []interface{}{
		"reason", reason,
		"sender", user,
		"chat", channelID,
		"channelType", channelType,
		"threadTs", threadTS,
	}
	if decision == gateDrop {
		sc.log.Info("dropped inbound", kvs...)
		return channel.InboundMessage{}, false
	}
	sc.log.Info("accepted inbound", kvs...)

	threadID := threadTS
	if sc.cfg.threading && threadID == "" {
		// Promote top-level message to a new thread anchored at its TS.
		threadID = ts
	}

	return channel.InboundMessage{
		SenderID: user,
		ChatID:   channelID,
		ThreadID: threadID,
		Text:     text,
		Metadata: map[string]string{
			"ts": ts,
		},
	}, true
}

// handleSocketEvent processes an events_api payload from Socket Mode.
// The payload wraps an Events API envelope with type "event_callback".
func (sc *SlackChannel) handleSocketEvent(ctx context.Context, payload json.RawMessage) {
	var inner struct {
		Type  string `json:"type"`
		Event struct {
			Type        string `json:"type"`
			User        string `json:"user"`
			Text        string `json:"text"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
			BotID       string `json:"bot_id"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &inner); err != nil {
		return
	}

	if inner.Event.Type != "message" || inner.Event.User == "" || inner.Event.Text == "" {
		return
	}
	// Ignore bot messages to avoid loops.
	if inner.Event.BotID != "" {
		return
	}

	msg, ok := sc.gateAndBuildInbound(
		inner.Event.User, inner.Event.Channel, inner.Event.ThreadTS,
		inner.Event.TS, inner.Event.ChannelType, inner.Event.Text,
	)
	if !ok {
		return
	}

	// Start the root span for the entire message processing trace.
	ctx, span := slackTracer.Start(ctx, "slack.message.received",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("sympozium.channel", "slack"),
			attribute.String("sympozium.sender.id", inner.Event.User),
			attribute.String("messaging.system", "slack"),
			attribute.String("messaging.destination.name", inner.Event.Channel),
		),
	)
	defer span.End()

	// PublishInbound propagates trace context through NATS headers.
	if err := sc.PublishInbound(ctx, msg); err != nil {
		span.RecordError(err)
		sc.log.Error(err, "failed to publish inbound from Socket Mode")
	}
}

// ---------------------------------------------------------------------------
// Events API fallback — HTTP server, requires public URL
// ---------------------------------------------------------------------------

// runEventsAPI starts an HTTP server for the Slack Events API.
func (sc *SlackChannel) runEventsAPI(ctx context.Context, addr string) {
	sc.setHealthy(true, "")

	mux := http.NewServeMux()
	mux.HandleFunc("/slack/events", sc.handleSlackEvents)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		sc.mu.RLock()
		h := sc.healthy
		sc.mu.RUnlock()
		if h {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		sc.log.Error(err, "slack events API server failed")
	}
}

// handleSlackEvents processes incoming Slack Events API payloads (webhook mode).
func (sc *SlackChannel) handleSlackEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var envelope struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Event     struct {
			Type        string `json:"type"`
			User        string `json:"user"`
			Text        string `json:"text"`
			Channel     string `json:"channel"`
			ChannelType string `json:"channel_type"`
			TS          string `json:"ts"`
			ThreadTS    string `json:"thread_ts"`
			BotID       string `json:"bot_id"`
		} `json:"event"`
	}

	if err := json.Unmarshal(body, &envelope); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Handle URL verification challenge
	if envelope.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, envelope.Challenge)
		return
	}

	// Process message events
	if envelope.Type == "event_callback" && envelope.Event.Type == "message" {
		if envelope.Event.User == "" || envelope.Event.Text == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Ignore bot messages to avoid loops.
		if envelope.Event.BotID != "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Slack-pod gating: enforce access + threading + sticky-threads.
		msg, ok := sc.gateAndBuildInbound(
			envelope.Event.User, envelope.Event.Channel, envelope.Event.ThreadTS,
			envelope.Event.TS, envelope.Event.ChannelType, envelope.Event.Text,
		)
		if !ok {
			w.WriteHeader(http.StatusOK)
			return
		}

		if err := sc.PublishInbound(r.Context(), msg); err != nil {
			fmt.Fprintf(os.Stderr, "failed to publish inbound: %v\n", err)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Outbound — shared by both modes
// ---------------------------------------------------------------------------

// handleOutbound subscribes to outbound messages and sends them via Slack API.
func (sc *SlackChannel) handleOutbound(ctx context.Context) {
	events, err := sc.SubscribeOutbound(ctx)
	if err != nil {
		sc.log.Error(err, "failed to subscribe to outbound messages")
		return
	}

	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-events:
			if !ok {
				sc.log.Info("outbound subscription closed")
				return
			}
			if event == nil {
				sc.log.Error(nil, "received nil outbound event")
				continue
			}

			var msg channel.OutboundMessage
			if err := json.Unmarshal(event.Data, &msg); err != nil {
				sc.log.Error(err, "failed to decode outbound message",
					"instance", sc.InstanceName,
					"targetInstance", event.Metadata["instanceName"],
					"payloadBytes", len(event.Data))
				continue
			}

			sc.log.Info("received outbound message",
				"instance", sc.InstanceName,
				"targetInstance", event.Metadata["instanceName"],
				"channel", msg.Channel,
				"chatId", msg.ChatID,
				"threadId", msg.ThreadID,
				"attachments", len(msg.Attachments))

			if msg.Channel != "slack" {
				continue
			}

			if msg.Reaction != "" {
				if err := sc.addReaction(ctx, msg); err != nil {
					sc.log.Error(err, "failed to add Slack reaction",
						"chatId", msg.ChatID,
						"targetMessageId", msg.TargetMessageID,
						"reaction", msg.Reaction)
				}
				continue
			}

			if err := sc.sendMessage(ctx, msg); err != nil {
				sc.log.Error(err, "failed to send Slack message",
					"chatId", msg.ChatID,
					"threadId", msg.ThreadID,
					"attachments", len(msg.Attachments))
				continue
			}

			sc.log.Info("sent outbound Slack message",
				"chatId", msg.ChatID,
				"threadId", msg.ThreadID,
				"attachments", len(msg.Attachments))
		}
	}
}

// slackAPIResponse is the common envelope returned by every Slack Web
// API method. We only need ok/error to distinguish success from
// soft-failure (HTTP 200 with ok:false).
type slackAPIResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// callSlackAPI performs a JSON POST to the given Slack Web API endpoint
// and returns an error when either the transport fails, the HTTP status
// is non-2xx, or Slack reports ok:false. Errors classified as benign
// (passed via okErrors) are treated as success.
func (sc *SlackChannel) callSlackAPI(ctx context.Context, endpoint string, payload interface{}, okErrors ...string) error {
	return sc.callSlackAPIInto(ctx, endpoint, payload, nil, okErrors...)
}

func (sc *SlackChannel) callSlackAPIInto(ctx context.Context, endpoint string, payload interface{}, out interface{}, okErrors ...string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sc.BotToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := sc.client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack %s returned HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed slackAPIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("decode slack response: %w (body=%q)", err, string(respBody))
	}
	if parsed.OK {
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decode slack response: %w (body=%q)", err, string(respBody))
			}
		}
		return nil
	}
	for _, ok := range okErrors {
		if parsed.Error == ok {
			return nil
		}
	}
	return fmt.Errorf("slack %s rejected request: %s", endpoint, parsed.Error)
}

// callSlackAPIFormInto performs an application/x-www-form-urlencoded POST to a
// Slack Web API endpoint. Some file-upload methods — notably
// files.getUploadURLExternal — do not accept a JSON body and reject the call
// with "invalid_arguments" unless the parameters arrive as form values.
func (sc *SlackChannel) callSlackAPIFormInto(ctx context.Context, endpoint string, form url.Values, out interface{}, okErrors ...string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+sc.BotToken)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := sc.client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack %s returned HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed slackAPIResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("decode slack response: %w (body=%q)", err, string(respBody))
	}
	if parsed.OK {
		if out != nil {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("decode slack response: %w (body=%q)", err, string(respBody))
			}
		}
		return nil
	}
	for _, ok := range okErrors {
		if parsed.Error == ok {
			return nil
		}
	}
	return fmt.Errorf("slack %s rejected request: %s", endpoint, parsed.Error)
}

// sendMessage sends a message via the Slack chat.postMessage API.
func (sc *SlackChannel) sendMessage(ctx context.Context, msg channel.OutboundMessage) error {
	threadTS := msg.ThreadID
	if threadTS == "" && sc.cfg != nil && sc.cfg.threading {
		threadTS = msg.Metadata["replyToTS"]
	}
	if hasEmbeddedSlackAttachments(msg.Attachments) {
		return sc.uploadFileAttachments(ctx, msg, threadTS)
	}

	payload := map[string]interface{}{
		"channel": msg.ChatID,
		"text":    msg.Text,
	}
	if len(msg.Attachments) > 0 {
		blocks, err := slackBlocksForMessage(msg)
		if err != nil {
			return err
		}
		payload["blocks"] = blocks
	}
	// Resolve the thread to post in:
	//  1. Explicit ThreadID set by the controller (message originally
	//     came from inside a thread) — always honoured.
	//  2. If threading is enabled and the original message has a known
	//     ts (replyToTS metadata), open a thread anchored at that ts.
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	return sc.callSlackAPI(ctx, "https://slack.com/api/chat.postMessage", payload)
}

func hasEmbeddedSlackAttachments(attachments []channel.Attachment) bool {
	for _, attachment := range attachments {
		if attachment.ContentBase64 != "" || attachment.ArtifactID != "" {
			return true
		}
	}
	return false
}

func (sc *SlackChannel) uploadFileAttachments(ctx context.Context, msg channel.OutboundMessage, threadTS string) error {
	files := make([]map[string]string, 0, len(msg.Attachments))
	var artifactIDs []string // best-effort cleanup after a successful send
	for i, attachment := range msg.Attachments {
		// Acquire the bytes. Prefer an artifact-server reference (bytes never
		// rode the event bus); fall back to inline base64.
		var (
			data        []byte
			fetchedMime string
			fetchedName string
			err         error
		)
		switch {
		case attachment.ArtifactID != "":
			data, fetchedMime, fetchedName, err = sc.fetchArtifact(ctx, attachment.ArtifactID)
			if err != nil {
				return fmt.Errorf("fetch artifact %q: %w", attachment.ArtifactID, err)
			}
			artifactIDs = append(artifactIDs, attachment.ArtifactID)
		case attachment.ContentBase64 != "":
			data, err = base64.StdEncoding.DecodeString(attachment.ContentBase64)
			if err != nil {
				return fmt.Errorf("decode slack attachment %d: %w", i, err)
			}
		default:
			return fmt.Errorf("slack cannot mix URL and local file attachments in one message")
		}
		if len(data) == 0 {
			return fmt.Errorf("slack attachment %d is empty", i)
		}

		filename := attachment.Filename
		if filename == "" {
			filename = fetchedName
		}
		if filename == "" {
			filename = fmt.Sprintf("attachment-%d", i+1)
		}
		mimeType := attachment.MimeType
		if mimeType == "" {
			mimeType = fetchedMime
		}
		if mimeType == "" {
			mimeType = http.DetectContentType(data)
		}
		isImage := attachment.Type == "image" || strings.HasPrefix(mimeType, "image/")

		var uploadURLResp struct {
			OK        bool   `json:"ok"`
			Error     string `json:"error,omitempty"`
			UploadURL string `json:"upload_url"`
			FileID    string `json:"file_id"`
		}
		// files.getUploadURLExternal only accepts form-encoded arguments; a JSON
		// body is rejected with "invalid_arguments".
		form := url.Values{}
		form.Set("filename", filename)
		form.Set("length", strconv.Itoa(len(data)))
		if isImage {
			form.Set("alt_txt", filename)
		}
		if err := sc.callSlackAPIFormInto(ctx, "https://slack.com/api/files.getUploadURLExternal", form, &uploadURLResp); err != nil {
			return err
		}
		if uploadURLResp.UploadURL == "" || uploadURLResp.FileID == "" {
			return fmt.Errorf("slack files.getUploadURLExternal response missing upload_url or file_id")
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURLResp.UploadURL, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("build slack file upload request: %w", err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := sc.client.Do(req)
		if err != nil {
			return fmt.Errorf("upload slack file %q: %w", filename, err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		uploadResult := strings.TrimSpace(string(respBody))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("slack file upload for %q returned HTTP %d: %s", filename, resp.StatusCode, uploadResult)
		}
		if !strings.HasPrefix(uploadResult, "OK") {
			return fmt.Errorf("slack file upload for %q returned unexpected success body: %q", filename, uploadResult)
		}
		sc.log.Info("uploaded attachment bytes to Slack",
			"filename", filename,
			"bytes", len(data),
			"fileId", uploadURLResp.FileID,
			"response", uploadResult)

		files = append(files, map[string]string{
			"id":    uploadURLResp.FileID,
			"title": filename,
		})
	}

	filesJSON, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("marshal Slack completion files: %w", err)
	}

	completeForm := url.Values{}
	completeForm.Set("files", string(filesJSON))
	completeForm.Set("channel_id", msg.ChatID)
	if msg.Text != "" {
		completeForm.Set("initial_comment", msg.Text)
	}
	if threadTS != "" {
		completeForm.Set("thread_ts", threadTS)
	}

	var completeResp struct {
		OK    bool `json:"ok"`
		Files []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Title string `json:"title"`
		} `json:"files"`
	}
	if err := sc.callSlackAPIFormInto(
		ctx,
		"https://slack.com/api/files.completeUploadExternal",
		completeForm,
		&completeResp,
	); err != nil {
		return err
	}
	if len(completeResp.Files) != len(files) {
		return fmt.Errorf(
			"Slack completed upload without returning all files: requested=%d returned=%d",
			len(files),
			len(completeResp.Files),
		)
	}
	for i := range files {
		if completeResp.Files[i].ID != files[i]["id"] {
			return fmt.Errorf(
				"Slack completed upload with unexpected file id at index %d: requested=%q returned=%q",
				i,
				files[i]["id"],
				completeResp.Files[i].ID,
			)
		}
	}

	sc.log.Info("completed Slack file upload",
		"chatId", msg.ChatID,
		"threadId", threadTS,
		"files", len(completeResp.Files),
		"fileId", completeResp.Files[0].ID)

	// The file is now delivered to Slack; drop the artifact-server copies.
	// Best-effort: a failure here only leaves an artifact to be swept at TTL.
	for _, id := range artifactIDs {
		if err := sc.deleteArtifact(ctx, id); err != nil {
			sc.log.V(1).Info("artifact cleanup failed", "id", id, "err", err.Error())
		}
	}
	return nil
}

// artifactServerBaseURL returns the configured artifact-server base URL with
// any trailing slash trimmed, or "" if unset.
func artifactServerBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("ARTIFACT_SERVER_URL")), "/")
}

// readServiceAccountToken reads the projected pod SA token used to authenticate
// to the artifact-server. SA_TOKEN_PATH overrides the location (tests).
func readServiceAccountToken() (string, error) {
	path := os.Getenv("SA_TOKEN_PATH")
	if path == "" {
		path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// fetchArtifact downloads an artifact's bytes from the artifact-server by id,
// authenticating with the channel pod's ServiceAccount token. It returns the
// bytes plus the server-reported MIME type and filename.
func (sc *SlackChannel) fetchArtifact(ctx context.Context, id string) (data []byte, mimeType, filename string, err error) {
	base := artifactServerBaseURL()
	if base == "" {
		return nil, "", "", fmt.Errorf("ARTIFACT_SERVER_URL is not configured")
	}
	token, err := readServiceAccountToken()
	if err != nil {
		return nil, "", "", fmt.Errorf("read SA token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/artifacts/"+id, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := sc.client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return nil, "", "", fmt.Errorf("artifact-server HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}
	mimeType = resp.Header.Get("Content-Type")
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, perr := mime.ParseMediaType(cd); perr == nil {
			filename = params["filename"]
		}
	}
	return data, mimeType, filename, nil
}

// deleteArtifact removes an artifact from the artifact-server after it has been
// delivered. Best-effort: callers log but do not fail on error.
func (sc *SlackChannel) deleteArtifact(ctx context.Context, id string) error {
	base := artifactServerBaseURL()
	if base == "" {
		return nil
	}
	token, err := readServiceAccountToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/v1/artifacts/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := sc.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("artifact-server delete HTTP %d", resp.StatusCode)
	}
	return nil
}

func slackBlocksForMessage(msg channel.OutboundMessage) ([]map[string]interface{}, error) {
	blocks := make([]map[string]interface{}, 0, len(msg.Attachments)+1)
	if strings.TrimSpace(msg.Text) != "" {
		blocks = append(blocks, map[string]interface{}{
			"type": "section",
			"text": map[string]string{
				"type": "mrkdwn",
				"text": msg.Text,
			},
		})
	}
	for i, attachment := range msg.Attachments {
		attachmentType := attachment.Type
		if attachmentType == "" {
			attachmentType = "image"
		}
		if attachmentType != "image" {
			return nil, fmt.Errorf("slack attachment %d has unsupported type %q", i, attachment.Type)
		}
		if attachment.URL == "" {
			return nil, fmt.Errorf("slack image attachment %d requires url", i)
		}
		if !strings.HasPrefix(attachment.URL, "https://") {
			return nil, fmt.Errorf("slack image attachment %d url must start with https://", i)
		}
		altText := attachment.Filename
		if altText == "" {
			altText = "image"
		}
		blocks = append(blocks, map[string]interface{}{
			"type":      "image",
			"image_url": attachment.URL,
			"alt_text":  altText,
		})
	}
	return blocks, nil
}

// addReaction adds an emoji reaction to a message via the Slack
// reactions.add API. Requires msg.TargetMessageID (Slack ts) and
// msg.Reaction to be set.
// already_reacted is treated as success so redelivered events stay idempotent.
func (sc *SlackChannel) addReaction(ctx context.Context, msg channel.OutboundMessage) error {
	if msg.TargetMessageID == "" || msg.Reaction == "" {
		return nil
	}
	payload := map[string]interface{}{
		"channel":   msg.ChatID,
		"timestamp": msg.TargetMessageID,
		"name":      strings.Trim(msg.Reaction, ":"),
	}
	return sc.callSlackAPI(ctx, "https://slack.com/api/reactions.add", payload, "already_reacted")
}

// setHealthy updates the health status and publishes it to the event bus.
func (sc *SlackChannel) setHealthy(connected bool, message string) {
	sc.mu.Lock()
	sc.healthy = connected
	sc.mu.Unlock()
	_ = sc.PublishHealth(context.Background(), channel.HealthStatus{
		Connected: connected,
		Message:   message,
	})
}
