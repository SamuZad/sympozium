// Package jsonlog provides the shared Sympozium JSONL log contract.
package jsonlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Timestamp string         `json:"timestamp"`
	Level     string         `json:"level"`
	Component string         `json:"component"`
	Harness   string         `json:"harness,omitempty"`
	Event     string         `json:"event"`
	Message   string         `json:"message"`
	RunID     string         `json:"run_id,omitempty"`
	Agent     string         `json:"agent,omitempty"`
	Stream    string         `json:"stream,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

var writeMu sync.Mutex

func Emit(out io.Writer, component, harness, level, event, message, stream string, data map[string]any) {
	if out == nil {
		out = os.Stderr
	}
	b, err := json.Marshal(Event{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Level: level, Component: component, Harness: harness, Event: event, Message: message, RunID: os.Getenv("AGENT_RUN_ID"), Agent: os.Getenv("INSTANCE_NAME"), Stream: stream, Data: data})
	if err != nil {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	_, _ = out.Write(append(b, '\n'))
}

type ProcessOutputWriter struct {
	mu                         sync.Mutex
	out                        io.Writer
	component, harness, stream string
	buf                        bytes.Buffer
}

func NewProcessOutputWriter(out io.Writer, component, harness, stream string) *ProcessOutputWriter {
	return &ProcessOutputWriter{out: out, component: component, harness: harness, stream: stream}
}

func (w *ProcessOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			_, _ = w.buf.Write(p)
			break
		}
		_, _ = w.buf.Write(p[:i])
		w.emitLine(w.buf.Bytes())
		w.buf.Reset()
		p = p[i+1:]
	}
	return n, nil
}

func (w *ProcessOutputWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.emitLine(w.buf.Bytes())
		w.buf.Reset()
	}
}

func (w *ProcessOutputWriter) emitLine(line []byte) {
	raw := strings.TrimSpace(string(line))
	if raw == "" {
		return
	}
	data := map[string]any{}
	message := raw
	var native any
	if json.Unmarshal([]byte(raw), &native) == nil {
		if event, ok := native.(map[string]any); ok {
			data["native_event"] = native
			message = humanMessage(event)
		} else {
			data["raw_message"] = raw
		}
	} else {
		data["raw_message"] = raw
	}
	level := "info"
	if w.stream == "stderr" {
		level = "warn"
	}
	Emit(w.out, w.component, w.harness, level, "process.output", message, w.stream, data)
}

// humanMessage turns the common Codex and Claude stream-json shapes into the
// actual text a person expects in a live log view. The complete native event
// remains in data.native_event for machines and detailed inspection.
func humanMessage(event map[string]any) string {
	typeName := stringValue(event["type"])
	if typeName == "error" {
		return firstNonEmpty(stringValue(event["message"]), nestedString(event, "error", "message"), "Native process error")
	}
	if typeName == "turn.failed" {
		return firstNonEmpty(nestedString(event, "error", "message"), stringValue(event["message"]), "Turn failed")
	}
	if typeName == "result" {
		if boolValue(event["is_error"]) || strings.HasPrefix(stringValue(event["subtype"]), "error") {
			return firstNonEmpty(stringValue(event["result"]), eventErrors(event["errors"]), "Run failed: "+stringValue(event["subtype"]), "Run failed")
		}
		return firstNonEmpty(stringValue(event["result"]), "Run completed")
	}
	if typeName == "assistant" || typeName == "user" {
		if msg := streamMessage(event); msg != "" {
			return msg
		}
	}
	if typeName == "system" {
		return claudeSystemMessage(event)
	}
	if item, ok := event["item"].(map[string]any); ok {
		return itemMessage(typeName, item)
	}
	switch typeName {
	case "thread.started":
		if id := stringValue(event["thread_id"]); id != "" {
			return "Thread started: " + id
		}
		return "Thread started"
	case "turn.started":
		return "Turn started"
	case "turn.completed":
		return "Turn completed"
	}
	return firstNonEmpty(stringValue(event["message"]), stringValue(event["text"]), describeUnknownEvent(event), "Native process event")
}

func itemMessage(eventType string, item map[string]any) string {
	kind, status := stringValue(item["type"]), stringValue(item["status"])
	switch kind {
	case "agent_message", "reasoning":
		return firstNonEmpty(stringValue(item["text"]), "Agent message")
	case "command_execution":
		command := stringValue(item["command"])
		if status == "in_progress" {
			return firstNonEmpty("▶ $ "+command, "Command started")
		}
		result := "✓ Command finished"
		if code, ok := item["exit_code"]; ok {
			result += fmt.Sprintf(" (exit %v)", code)
		}
		if output := strings.TrimSpace(stringValue(item["aggregated_output"])); output != "" {
			result += ": " + output
		}
		return result
	case "mcp_tool_call":
		name := strings.Trim(strings.Join([]string{stringValue(item["server"]), stringValue(item["tool"])}, "/"), "/")
		return firstNonEmpty("Tool "+name+statusSuffix(status), "Tool invocation")
	case "file_change":
		return "File changes" + statusSuffix(status)
	}
	return firstNonEmpty(stringValue(item["text"]), kind+statusSuffix(status), eventType)
}

func streamMessage(event map[string]any) string {
	message, _ := event["message"].(map[string]any)
	if message == nil {
		return strings.TrimSpace(stringValue(event["text"]))
	}
	return renderContent(message["content"])
}

func renderContent(content any) string {
	if text := strings.TrimSpace(stringValue(content)); text != "" {
		return text
	}
	if block, ok := content.(map[string]any); ok {
		return renderContent([]any{block})
	}
	contentBlocks, _ := content.([]any)
	var parts []string
	for _, v := range contentBlocks {
		block, _ := v.(map[string]any)
		switch stringValue(block["type"]) {
		case "text":
			if text := strings.TrimSpace(stringValue(block["text"])); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			parts = append(parts, "Tool requested: "+stringValue(block["name"]))
		case "tool_result":
			if text := renderContent(block["content"]); text != "" {
				parts = append(parts, text)
			} else {
				parts = append(parts, "Tool result received")
			}
		case "thinking":
			if text := strings.TrimSpace(firstNonEmpty(stringValue(block["thinking"]), stringValue(block["text"]))); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func claudeSystemMessage(event map[string]any) string {
	subtype := stringValue(event["subtype"])
	if subtype == "init" {
		if model := stringValue(event["model"]); model != "" {
			return "Claude session started: " + model
		}
		return "Claude session started"
	}
	if msg := firstNonEmpty(stringValue(event["message"]), stringValue(event["text"])); msg != "" {
		return msg
	}
	if subtype != "" {
		return "Claude system event: " + subtype
	}
	return "Claude system event"
}

func nestedString(m map[string]any, key, child string) string {
	nested, _ := m[key].(map[string]any)
	return stringValue(nested[child])
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func statusSuffix(status string) string {
	if status == "" {
		return ""
	}
	return " (" + status + ")"
}

func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

func eventErrors(v any) string {
	items, ok := v.([]any)
	if !ok {
		return strings.TrimSpace(stringValue(v))
	}
	var messages []string
	for _, item := range items {
		switch value := item.(type) {
		case string:
			messages = append(messages, value)
		case map[string]any:
			if msg := stringValue(value["message"]); msg != "" {
				messages = append(messages, msg)
			}
		}
	}
	return strings.Join(messages, "; ")
}

func describeUnknownEvent(event map[string]any) string {
	typeName := stringValue(event["type"])
	if subtype := stringValue(event["subtype"]); subtype != "" {
		typeName += "/" + subtype
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return typeName
	}
	return "Native event " + typeName + ": " + string(payload)
}
