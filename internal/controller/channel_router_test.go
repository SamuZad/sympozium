package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	channel "github.com/sympozium-ai/sympozium/internal/channel"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
)

func TestChannelRouterHandleInboundCopiesModelTuning(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sympoziumv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	maxTokens := int32(12000)
	inst := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "helper", Namespace: "default"},
		Spec: sympoziumv1alpha1.AgentSpec{
			Agents: sympoziumv1alpha1.AgentsSpec{
				Default: sympoziumv1alpha1.AgentConfig{
					Model:       "gpt-5.5",
					Thinking:    "high",
					MaxTokens:   &maxTokens,
					Temperature: "0.2",
				},
			},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst).Build()
	cr := &ChannelRouter{Client: cl, Log: logr.Discard()}

	event, err := eventbus.NewEvent(eventbus.TopicChannelMessageRecv, nil, channel.InboundMessage{
		InstanceName: "helper",
		Channel:      "telegram",
		ChatID:       "chat-1",
		Text:         "hello",
	})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	cr.handleInbound(context.Background(), event)

	var runs sympoziumv1alpha1.AgentRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("List AgentRuns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("AgentRun count = %d, want 1", len(runs.Items))
	}
	model := runs.Items[0].Spec.Model
	if model.Thinking != "high" {
		t.Errorf("Thinking = %q, want high", model.Thinking)
	}
	if model.MaxTokens == nil || *model.MaxTokens != maxTokens {
		t.Errorf("MaxTokens = %v, want %d", model.MaxTokens, maxTokens)
	}
	if model.Temperature != "0.2" {
		t.Errorf("Temperature = %q, want 0.2", model.Temperature)
	}
}

func TestChannelRouterAutoReply(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := sympoziumv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	off := false

	// route triggers a run through the given channel spec, then completes
	// and fails it, returning the run plus the texts posted for each.
	route := func(t *testing.T, ch sympoziumv1alpha1.ChannelSpec) (sympoziumv1alpha1.AgentRun, []string, []string) {
		t.Helper()
		inst := &sympoziumv1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "helper", Namespace: "default"},
			Spec:       sympoziumv1alpha1.AgentSpec{Channels: []sympoziumv1alpha1.ChannelSpec{ch}},
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(inst).Build()
		bus := &recordingEventBus{}
		cr := &ChannelRouter{Client: cl, EventBus: bus, Log: logr.Discard()}
		ctx := context.Background()

		in, _ := eventbus.NewEvent(eventbus.TopicChannelMessageRecv, nil, channel.InboundMessage{
			InstanceName: "helper", Channel: "telegram", ChatID: "chat-1", Text: "hello",
		})
		cr.handleInbound(ctx, in)
		var runs sympoziumv1alpha1.AgentRunList
		if err := cl.List(ctx, &runs); err != nil || len(runs.Items) != 1 {
			t.Fatalf("runs = %d, err = %v", len(runs.Items), err)
		}
		run := runs.Items[0]
		meta := map[string]string{"agentRunID": run.Name, "instanceName": "helper"}

		texts := func() []string {
			var out []string
			for _, p := range bus.published {
				var msg channel.OutboundMessage
				_ = json.Unmarshal(p.Event.Data, &msg)
				out = append(out, msg.Text)
			}
			bus.published = nil
			return out
		}
		done, _ := eventbus.NewEvent(eventbus.TopicAgentRunCompleted, meta, map[string]string{"status": "success", "response": "the answer"})
		cr.handleCompleted(ctx, done)
		completed := texts()
		failed, _ := eventbus.NewEvent(eventbus.TopicAgentRunFailed, meta, map[string]string{"error": "boom"})
		cr.handleFailed(ctx, failed)
		return run, completed, texts()
	}

	t.Run("default relays the response", func(t *testing.T) {
		run, completed, _ := route(t, sympoziumv1alpha1.ChannelSpec{Type: "telegram"})
		if _, ok := run.Annotations[autoReplyAnnotation]; ok {
			t.Errorf("annotation set without autoReply=false: %v", run.Annotations)
		}
		if len(completed) != 1 || completed[0] != "the answer" {
			t.Errorf("completed posts = %q, want the response", completed)
		}
	})

	t.Run("autoReply=false posts only failures", func(t *testing.T) {
		run, completed, failed := route(t, sympoziumv1alpha1.ChannelSpec{Type: "telegram", AutoReply: &off})
		if run.Annotations[autoReplyAnnotation] != "false" {
			t.Errorf("annotation = %q, want false", run.Annotations[autoReplyAnnotation])
		}
		if len(completed) != 0 {
			t.Errorf("response must not be relayed, got %q", completed)
		}
		if len(failed) != 1 || !strings.Contains(failed[0], "boom") {
			t.Errorf("failure notice = %q, want one mentioning the error", failed)
		}
	})

	t.Run("pod is told to reply itself", func(t *testing.T) {
		run, _, _ := route(t, sympoziumv1alpha1.ChannelSpec{Type: "telegram", AutoReply: &off})
		cs, _ := (&AgentRunReconciler{}).buildContainers(&run, false, nil, nil, nil)
		var found bool
		for _, e := range cs[0].Env {
			if e.Name == "CHANNEL_AUTO_REPLY" && e.Value == "false" {
				found = true
			}
		}
		if !found {
			t.Error("agent container should get CHANNEL_AUTO_REPLY=false")
		}
	})
}

func TestCheckChannelAccess(t *testing.T) {
	tests := []struct {
		name        string
		channels    []sympoziumv1alpha1.ChannelSpec
		msg         channel.InboundMessage
		wantAllowed bool
		wantDeny    string
	}{
		{
			name:        "no access control configured",
			channels:    []sympoziumv1alpha1.ChannelSpec{{Type: "telegram"}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: true,
		},
		{
			name:        "channel type not in instance spec",
			channels:    []sympoziumv1alpha1.ChannelSpec{{Type: "slack"}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: true,
		},
		{
			name: "allowed sender in list",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedSenders: []string{"123", "789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: true,
		},
		{
			name: "allowed sender not in list",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedSenders: []string{"789", "012"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
		},
		{
			name: "denied sender in list",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					DeniedSenders: []string{"123"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
		},
		{
			name: "denied sender not in list",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					DeniedSenders: []string{"789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: true,
		},
		{
			name: "sender in both allow and deny lists - deny wins",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedSenders: []string{"123"},
					DeniedSenders:  []string{"123"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
		},
		{
			name: "allowed chat in list",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedChats: []string{"456", "789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: true,
		},
		{
			name: "allowed chat not in list",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedChats: []string{"789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
		},
		{
			name: "allowed chat passes but denied sender blocks",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedChats:  []string{"456"},
					DeniedSenders: []string{"123"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
		},
		{
			name: "deny message returned when set",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedSenders: []string{"789"},
					DenyMessage:    "You are not authorized.",
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
			wantDeny:    "You are not authorized.",
		},
		{
			name: "deny message empty when not set",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedSenders: []string{"789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: false,
			wantDeny:    "",
		},
		{
			name: "discord channel ID routing via AllowedChats - denied",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "discord",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedChats: []string{"1234567890123456789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "discord", SenderID: "user1", ChatID: "9999999999999999999"},
			wantAllowed: false,
		},
		{
			name: "discord channel ID routing via AllowedChats - allowed",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "discord",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedChats: []string{"1234567890123456789"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "discord", SenderID: "user1", ChatID: "1234567890123456789"},
			wantAllowed: true,
		},
		{
			name: "all checks pass",
			channels: []sympoziumv1alpha1.ChannelSpec{{
				Type: "telegram",
				AccessControl: &sympoziumv1alpha1.ChannelAccessControl{
					AllowedSenders: []string{"123"},
					AllowedChats:   []string{"456"},
					DeniedSenders:  []string{"999"},
				},
			}},
			msg:         channel.InboundMessage{Channel: "telegram", SenderID: "123", ChatID: "456"},
			wantAllowed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &sympoziumv1alpha1.Agent{
				Spec: sympoziumv1alpha1.AgentSpec{
					Channels: tt.channels,
				},
			}
			allowed, denyMsg := checkChannelAccess(inst, &tt.msg)
			if allowed != tt.wantAllowed {
				t.Errorf("checkChannelAccess() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
			if denyMsg != tt.wantDeny {
				t.Errorf("checkChannelAccess() denyMsg = %q, want %q", denyMsg, tt.wantDeny)
			}
		})
	}
}
