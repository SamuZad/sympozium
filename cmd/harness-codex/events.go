package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/harness"
)

// codexEvent is the subset of `codex exec --json` thread events the shim
// cares about (see codex-rs/exec/src/exec_events.rs). Unknown event and item
// types are ignored.
type codexEvent struct {
	Type  string `json:"type"`
	Usage *struct {
		InputTokens           int64 `json:"input_tokens"`
		CachedInputTokens     int64 `json:"cached_input_tokens"`
		CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
		OutputTokens          int64 `json:"output_tokens"`
		ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
	Item *struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Text     string `json:"text"`
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
		Server   string `json:"server"`
		Tool     string `json:"tool"`
		Message  string `json:"message"`
	} `json:"item"`
	// Error is set on turn.failed; Message on a top-level "error" event.
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// toolKey identifies a tool-invocation series (tool_name, status).
type toolKey struct {
	Name, Status string
}

// codexRun accumulates what the shim learns from a `codex exec --json` run.
type codexRun struct {
	// Usage sums token usage across turns, normalised into disjoint buckets.
	Usage harness.TokenUsage
	// Turns counts turn.completed events.
	Turns int
	// ToolInvocations counts completed tool items by (name, status).
	ToolInvocations map[toolKey]int
	// LastAgentMessage is the most recent agent_message item text — the
	// answer when codex writes nothing to --output-last-message (e.g. the
	// model ended its turn after a tool call without a closing message).
	LastAgentMessage string
	// FailureMessage is the first fatal error reported by the stream.
	FailureMessage string
}

// consume reads JSONL events from r until EOF, echoing every raw line to tee
// (the pod log) and folding recognised events into the run.
func (c *codexRun) consume(r io.Reader, tee io.Writer) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if tee != nil {
				_, _ = tee.Write(line)
			}
			c.consumeLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (c *codexRun) consumeLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev codexEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "turn.completed":
		c.Turns++
		if u := ev.Usage; u != nil {
			// codex passes through the Responses API's usage, where
			// cached_input_tokens and cache_write_input_tokens are both
			// breakdowns OF input_tokens (input_tokens + output_tokens ==
			// total_tokens), and reasoning tokens are a subset of
			// output_tokens. Carve the two cache buckets out of input so the
			// four buckets stay disjoint and sum to the billed total.
			uncached := u.InputTokens - u.CachedInputTokens - u.CacheWriteInputTokens
			if uncached < 0 {
				uncached = 0
			}
			c.Usage.Input += uncached
			c.Usage.CacheRead += u.CachedInputTokens
			c.Usage.CacheWrite += u.CacheWriteInputTokens
			c.Usage.Output += u.OutputTokens
		}
	case "turn.failed":
		if c.FailureMessage == "" && ev.Error != nil {
			c.FailureMessage = ev.Error.Message
		}
	case "error":
		if c.FailureMessage == "" {
			c.FailureMessage = ev.Message
		}
	case "item.completed":
		if ev.Item == nil {
			return
		}
		switch ev.Item.Type {
		case "agent_message":
			if strings.TrimSpace(ev.Item.Text) != "" {
				c.LastAgentMessage = ev.Item.Text
			}
		case "command_execution", "mcp_tool_call", "file_change", "web_search", "collab_tool_call":
			if c.ToolInvocations == nil {
				c.ToolInvocations = map[toolKey]int{}
			}
			c.ToolInvocations[toolKey{Name: codexToolName(ev.Item.Type, ev.Item.Server, ev.Item.Tool), Status: codexToolStatus(ev.Item.Status)}]++
		}
	}
}

// codexToolName maps a codex item to the tool name codex itself uses where
// one exists (shell, apply_patch, web_search) and to "<server>/<tool>" for
// MCP calls, mirroring the agent-runner's tool_name attribute.
func codexToolName(itemType, server, tool string) string {
	switch itemType {
	case "command_execution":
		return "shell"
	case "file_change":
		return "apply_patch"
	case "mcp_tool_call":
		switch {
		case server != "" && tool != "":
			return server + "/" + tool
		case tool != "":
			return tool
		default:
			return "mcp_tool_call"
		}
	default:
		return itemType
	}
}

// codexToolStatus folds codex item statuses onto the agent-runner's
// success/error vocabulary. A declined command is reported as "denied".
func codexToolStatus(status string) string {
	switch strings.ToLower(status) {
	case "failed":
		return "error"
	case "declined":
		return "denied"
	default:
		return "success"
	}
}

// ToolCalls returns the total number of tool invocations seen.
func (c *codexRun) ToolCalls() int {
	n := 0
	for _, v := range c.ToolInvocations {
		n += v
	}
	return n
}

// SortedToolInvocations returns the (name, status, count) triples in a
// stable order for logging and metric emission.
func (c *codexRun) SortedToolInvocations() []struct {
	Key   toolKey
	Count int
} {
	out := make([]struct {
		Key   toolKey
		Count int
	}, 0, len(c.ToolInvocations))
	for k, v := range c.ToolInvocations {
		out = append(out, struct {
			Key   toolKey
			Count int
		}{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Name != out[j].Key.Name {
			return out[i].Key.Name < out[j].Key.Name
		}
		return out[i].Key.Status < out[j].Key.Status
	})
	return out
}
