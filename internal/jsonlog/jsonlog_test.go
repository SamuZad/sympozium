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

func TestHumanMessageClaudeSystemAndNestedToolResult(t *testing.T) {
	system := map[string]any{"type": "system", "subtype": "init", "model": "claude-opus"}
	if got := humanMessage(system); got != "Claude session started: claude-opus" {
		t.Fatalf("system message = %q", got)
	}
	toolResult := map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "text", "text": "42 rows returned"}}}}}}
	if got := humanMessage(toolResult); got != "42 rows returned" {
		t.Fatalf("tool result message = %q", got)
	}
}

func TestHumanMessageClaudeFailureAndUnknownEvent(t *testing.T) {
	failure := map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{map[string]any{"message": "rate limited"}}}
	if got := humanMessage(failure); got != "rate limited" {
		t.Fatalf("failure message = %q", got)
	}
	unknown := map[string]any{"type": "future_event", "subtype": "progress", "detail": "new CLI output"}
	if got := humanMessage(unknown); got != `Native event future_event/progress: {"detail":"new CLI output","subtype":"progress","type":"future_event"}` {
		t.Fatalf("unknown message = %q", got)
	}
}
