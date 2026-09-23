package jsonlog

import (
	"encoding/json"
	"testing"
)

func TestHumanMessageCodexEvents(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"agent message", `{"type":"item.completed","item":{"type":"agent_message","text":"The report is ready."}}`, "The report is ready."},
		{"running command", `{"type":"item.started","item":{"type":"command_execution","command":"git status","status":"in_progress"}}`, "▶ $ git status"},
		{"completed command", `{"type":"item.completed","item":{"type":"command_execution","status":"completed","exit_code":0,"aggregated_output":"clean"}}`, "✓ Command finished (exit 0): clean"},
		{"tool", `{"type":"item.completed","item":{"type":"mcp_tool_call","server":"sympozium_bridge","tool":"slack_conversations_replies","status":"completed"}}`, "Tool sympozium_bridge/slack_conversations_replies (completed)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var event map[string]any
			if err := json.Unmarshal([]byte(tt.raw), &event); err != nil {
				t.Fatal(err)
			}
			if got := humanMessage(event); got != tt.want {
				t.Fatalf("humanMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHumanMessageClaudeText(t *testing.T) {
	event := map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "I found the issue."}}}}
	if got := humanMessage(event); got != "I found the issue." {
		t.Fatalf("humanMessage() = %q", got)
	}
}
