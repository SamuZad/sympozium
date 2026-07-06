package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/sympozium-ai/sympozium/internal/channel"
)

// roundTripFunc lets a test substitute the http.Client transport.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func newTestSlackChannel(rt roundTripFunc) *SlackChannel {
	return &SlackChannel{
		BotToken: "xoxb-test",
		log:      logr.Discard(),
		client:   &http.Client{Transport: rt},
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestAddReaction_PostsExpectedPayload(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte
	sc := newTestSlackChannel(func(req *http.Request) (*http.Response, error) {
		captured = req
		buf, _ := io.ReadAll(req.Body)
		capturedBody = buf
		return jsonResponse(`{"ok":true}`), nil
	})

	err := sc.addReaction(context.Background(), channel.OutboundMessage{
		Channel:         "slack",
		ChatID:          "C123",
		Reaction:        ":robot_face:",
		TargetMessageID: "1700000000.000100",
	})
	if err != nil {
		t.Fatalf("addReaction: %v", err)
	}
	if captured.URL.String() != "https://slack.com/api/reactions.add" {
		t.Errorf("URL = %s", captured.URL)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer xoxb-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["channel"] != "C123" {
		t.Errorf("channel = %v", payload["channel"])
	}
	if payload["timestamp"] != "1700000000.000100" {
		t.Errorf("timestamp = %v", payload["timestamp"])
	}
	if payload["name"] != "robot_face" { // colons stripped
		t.Errorf("name = %v (want colons stripped)", payload["name"])
	}
}

func TestAddReaction_NoOpWhenIncomplete(t *testing.T) {
	called := false
	sc := newTestSlackChannel(func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(`{"ok":true}`), nil
	})

	cases := []channel.OutboundMessage{
		{Channel: "slack", ChatID: "C", Reaction: "eyes"},       // missing ts
		{Channel: "slack", ChatID: "C", TargetMessageID: "1.0"}, // missing reaction
		{Channel: "slack", ChatID: "C"},                         // both missing
	}
	for i, msg := range cases {
		if err := sc.addReaction(context.Background(), msg); err != nil {
			t.Errorf("case %d: unexpected error: %v", i, err)
		}
	}
	if called {
		t.Error("HTTP transport should not be invoked for incomplete messages")
	}
}

func TestAddReaction_AlreadyReactedIsSuccess(t *testing.T) {
	sc := newTestSlackChannel(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"ok":false,"error":"already_reacted"}`), nil
	})
	err := sc.addReaction(context.Background(), channel.OutboundMessage{
		Channel: "slack", ChatID: "C", Reaction: "eyes", TargetMessageID: "1.0",
	})
	if err != nil {
		t.Errorf("already_reacted should be treated as success, got %v", err)
	}
}

func TestAddReaction_SlackErrorBubblesUp(t *testing.T) {
	sc := newTestSlackChannel(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"ok":false,"error":"invalid_auth"}`), nil
	})
	err := sc.addReaction(context.Background(), channel.OutboundMessage{
		Channel: "slack", ChatID: "C", Reaction: "eyes", TargetMessageID: "1.0",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("want error containing invalid_auth, got %v", err)
	}
}

func TestAddReaction_HTTPErrorBubblesUp(t *testing.T) {
	sc := newTestSlackChannel(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})
	err := sc.addReaction(context.Background(), channel.OutboundMessage{
		Channel: "slack", ChatID: "C", Reaction: "eyes", TargetMessageID: "1.0",
	})
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Errorf("want network error, got %v", err)
	}
}

func TestAddReaction_NonJSONResponseFails(t *testing.T) {
	sc := newTestSlackChannel(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte("<html>oops</html>"))),
		}, nil
	})
	err := sc.addReaction(context.Background(), channel.OutboundMessage{
		Channel: "slack", ChatID: "C", Reaction: "eyes", TargetMessageID: "1.0",
	})
	if err == nil {
		t.Error("expected error for non-JSON response")
	}
}

func TestAddReaction_Non2xxFails(t *testing.T) {
	sc := newTestSlackChannel(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("upstream down")),
		}, nil
	})
	err := sc.addReaction(context.Background(), channel.OutboundMessage{
		Channel: "slack", ChatID: "C", Reaction: "eyes", TargetMessageID: "1.0",
	})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("want 503 error, got %v", err)
	}
}

func TestSendMessage_PostsExpectedPayload(t *testing.T) {
	var capturedBody []byte
	var capturedURL string
	sc := newTestSlackChannel(func(req *http.Request) (*http.Response, error) {
		capturedURL = req.URL.String()
		capturedBody, _ = io.ReadAll(req.Body)
		return jsonResponse(`{"ok":true}`), nil
	})
	err := sc.sendMessage(context.Background(), channel.OutboundMessage{
		Channel: "slack", ChatID: "C123", Text: "hello", ThreadID: "1700.0001",
	})
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if capturedURL != "https://slack.com/api/chat.postMessage" {
		t.Errorf("URL = %s", capturedURL)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["channel"] != "C123" || payload["text"] != "hello" || payload["thread_ts"] != "1700.0001" {
		t.Errorf("payload = %+v", payload)
	}
	if _, ok := payload["blocks"]; ok {
		t.Errorf("plain text message should not include blocks: %+v", payload)
	}
}

func TestSendMessage_WithImageAttachmentsPostsBlocks(t *testing.T) {
	var capturedBody []byte
	sc := newTestSlackChannel(func(req *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(req.Body)
		return jsonResponse(`{"ok":true}`), nil
	})
	err := sc.sendMessage(context.Background(), channel.OutboundMessage{
		Channel:  "slack",
		ChatID:   "C123",
		Text:     "chart attached",
		ThreadID: "1700.0001",
		Attachments: []channel.Attachment{
			{Type: "image", URL: "https://example.com/chart.png", Filename: "chart.png"},
			{URL: "https://example.com/detail.jpg"},
		},
	})
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	blocks, ok := payload["blocks"].([]interface{})
	if !ok || len(blocks) != 3 {
		t.Fatalf("blocks = %#v, want 3 blocks", payload["blocks"])
	}
	section := blocks[0].(map[string]interface{})
	if section["type"] != "section" {
		t.Fatalf("first block = %#v, want section", section)
	}
	firstImage := blocks[1].(map[string]interface{})
	if firstImage["type"] != "image" || firstImage["image_url"] != "https://example.com/chart.png" || firstImage["alt_text"] != "chart.png" {
		t.Fatalf("first image block = %#v", firstImage)
	}
	secondImage := blocks[2].(map[string]interface{})
	if secondImage["type"] != "image" || secondImage["image_url"] != "https://example.com/detail.jpg" || secondImage["alt_text"] != "image" {
		t.Fatalf("second image block = %#v", secondImage)
	}
	if payload["thread_ts"] != "1700.0001" {
		t.Fatalf("thread_ts = %v", payload["thread_ts"])
	}
}

func TestSendMessage_RejectsInsecureImageAttachmentURL(t *testing.T) {
	called := false
	sc := newTestSlackChannel(func(req *http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(`{"ok":true}`), nil
	})
	err := sc.sendMessage(context.Background(), channel.OutboundMessage{
		Channel: "slack",
		ChatID:  "C123",
		Text:    "chart attached",
		Attachments: []channel.Attachment{
			{Type: "image", URL: "http://example.com/chart.png"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("expected https validation error, got %v", err)
	}
	if called {
		t.Fatal("Slack API should not be called for invalid attachment URL")
	}
}

func TestSendMessage_WithEmbeddedImageUploadsSlackFile(t *testing.T) {
	imageBytes := []byte("fake png bytes")
	var uploadURLPayload map[string]interface{}
	var uploadedBody []byte
	var uploadedContentType string
	var completePayload map[string]interface{}

	sc := newTestSlackChannel(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://slack.com/api/files.getUploadURLExternal":
			body, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(body, &uploadURLPayload); err != nil {
				t.Fatalf("decode getUploadURLExternal payload: %v", err)
			}
			return jsonResponse(`{"ok":true,"upload_url":"https://upload.slack.test/file","file_id":"F123"}`), nil
		case "https://upload.slack.test/file":
			uploadedContentType = req.Header.Get("Content-Type")
			uploadedBody, _ = io.ReadAll(req.Body)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("OK"))}, nil
		case "https://slack.com/api/files.completeUploadExternal":
			body, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(body, &completePayload); err != nil {
				t.Fatalf("decode completeUploadExternal payload: %v", err)
			}
			return jsonResponse(`{"ok":true}`), nil
		default:
			t.Fatalf("unexpected Slack request URL: %s", req.URL.String())
			return nil, nil
		}
	})

	err := sc.sendMessage(context.Background(), channel.OutboundMessage{
		Channel:  "slack",
		ChatID:   "C123",
		Text:     "chart attached",
		ThreadID: "1700.0001",
		Attachments: []channel.Attachment{
			{
				Type:          "image",
				Filename:      "chart.png",
				MimeType:      "image/png",
				ContentBase64: base64.StdEncoding.EncodeToString(imageBytes),
			},
		},
	})
	if err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if uploadURLPayload["filename"] != "chart.png" || uploadURLPayload["length"] != float64(len(imageBytes)) || uploadURLPayload["alt_txt"] != "chart.png" {
		t.Fatalf("getUploadURLExternal payload = %+v", uploadURLPayload)
	}
	if string(uploadedBody) != string(imageBytes) || uploadedContentType != "image/png" {
		t.Fatalf("uploaded body/content-type = %q/%q", string(uploadedBody), uploadedContentType)
	}
	if completePayload["channel_id"] != "C123" || completePayload["initial_comment"] != "chart attached" || completePayload["thread_ts"] != "1700.0001" {
		t.Fatalf("completeUploadExternal payload = %+v", completePayload)
	}
	files, ok := completePayload["files"].([]interface{})
	if !ok || len(files) != 1 {
		t.Fatalf("files = %#v, want one file", completePayload["files"])
	}
	file := files[0].(map[string]interface{})
	if file["id"] != "F123" || file["title"] != "chart.png" {
		t.Fatalf("completed file = %#v", file)
	}
}
