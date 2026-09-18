package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sympozium-ai/sympozium/internal/harness"
)

const sampleCodexStream = `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"ls","aggregated_output":"a.txt","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","server":"k8s","tool":"get_pods","status":"failed"}}
{"type":"item.completed","item":{"id":"i3","type":"file_change","changes":[],"status":"completed"}}
{"type":"item.completed","item":{"id":"i4","type":"reasoning","text":"thinking"}}
not json, must be ignored
{"type":"item.completed","item":{"id":"i5","type":"agent_message","text":"All done: see /workspace/report.md"}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"cache_write_input_tokens":50,"output_tokens":120,"reasoning_output_tokens":40}}
`

func TestCodexRun_ParsesStream(t *testing.T) {
	var tee bytes.Buffer
	run := &codexRun{}
	run.consume(strings.NewReader(sampleCodexStream), &tee)

	if tee.String() != sampleCodexStream {
		t.Fatal("every raw line must be teed to the log verbatim")
	}
	if run.Turns != 1 {
		t.Errorf("Turns = %d, want 1", run.Turns)
	}
	// cached and cache-write are both subsets of input; reasoning a subset
	// of output. PromptTotal must equal the API's input_tokens exactly.
	want := harness.TokenUsage{Input: 150, CacheRead: 800, CacheWrite: 50, Output: 120}
	if run.Usage != want {
		t.Errorf("Usage = %+v, want %+v", run.Usage, want)
	}
	if run.Usage.PromptTotal() != 1000 || run.Usage.Total() != 1120 {
		t.Errorf("totals: prompt=%d total=%d, want 1000 / 1120", run.Usage.PromptTotal(), run.Usage.Total())
	}
	if run.LastAgentMessage != "All done: see /workspace/report.md" {
		t.Errorf("LastAgentMessage = %q", run.LastAgentMessage)
	}
	if run.ToolCalls() != 3 {
		t.Errorf("ToolCalls = %d, want 3 (reasoning and agent_message are not tools)", run.ToolCalls())
	}
	got := run.SortedToolInvocations()
	wantKeys := []toolKey{
		{"apply_patch", "success"},
		{"k8s/get_pods", "error"},
		{"shell", "success"},
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("invocations = %+v", got)
	}
	for i, k := range wantKeys {
		if got[i].Key != k || got[i].Count != 1 {
			t.Errorf("invocation[%d] = %+v, want %+v x1", i, got[i], k)
		}
	}
	if run.FailureMessage != "" {
		t.Errorf("unexpected failure: %q", run.FailureMessage)
	}
}

func TestCodexRun_MultipleTurnsAccumulate(t *testing.T) {
	stream := `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}}
{"type":"turn.completed","usage":{"input_tokens":30,"cached_input_tokens":20,"output_tokens":7,"reasoning_output_tokens":2}}
`
	run := &codexRun{}
	run.consume(strings.NewReader(stream), nil)
	if run.Turns != 2 {
		t.Fatalf("Turns = %d", run.Turns)
	}
	if run.Usage != (harness.TokenUsage{Input: 20, CacheRead: 20, Output: 12}) {
		t.Fatalf("Usage = %+v", run.Usage)
	}
}

func TestCodexRun_CapturesFailures(t *testing.T) {
	run := &codexRun{}
	run.consume(strings.NewReader(`{"type":"turn.failed","error":{"message":"rate limited"}}`+"\n"), nil)
	if run.FailureMessage != "rate limited" {
		t.Fatalf("FailureMessage = %q", run.FailureMessage)
	}

	run = &codexRun{}
	run.consume(strings.NewReader(`{"type":"error","message":"stream closed"}`+"\n"+`{"type":"turn.failed","error":{"message":"second"}}`+"\n"), nil)
	if run.FailureMessage != "stream closed" {
		t.Fatalf("first failure should win, got %q", run.FailureMessage)
	}
}

// Real turn.completed event from a production codex run (2026-09-17). The
// prompt total must come out at exactly input_tokens, and the cache hit rate
// (cache_read / prompt) at ~95%.
func TestCodexRun_ProductionSampleBuckets(t *testing.T) {
	line := `{"type":"turn.completed","usage":{"input_tokens":1891853,"cached_input_tokens":1796579,"cache_write_input_tokens":95178,"output_tokens":8728,"reasoning_output_tokens":3276}}` + "\n"
	run := &codexRun{}
	run.consume(strings.NewReader(line), nil)

	want := harness.TokenUsage{Input: 96, CacheRead: 1796579, CacheWrite: 95178, Output: 8728}
	if run.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", run.Usage, want)
	}
	if got := run.Usage.PromptTotal(); got != 1891853 {
		t.Fatalf("PromptTotal = %d, want input_tokens 1891853 (cache buckets must not be double-counted)", got)
	}
	hitRate := float64(run.Usage.CacheRead) / float64(run.Usage.PromptTotal())
	if hitRate < 0.949 || hitRate > 0.950 {
		t.Fatalf("cache hit rate = %.4f, want ~0.9496", hitRate)
	}
}

func TestCodexRun_ClampsNegativeUncachedInput(t *testing.T) {
	// Defensive: a stream where cached > input must not go negative.
	run := &codexRun{}
	run.consume(strings.NewReader(`{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":9,"output_tokens":1}}`+"\n"), nil)
	if run.Usage.Input != 0 || run.Usage.CacheRead != 9 {
		t.Fatalf("Usage = %+v", run.Usage)
	}
}

func TestCodexToolNameAndStatus(t *testing.T) {
	if codexToolName("command_execution", "", "") != "shell" ||
		codexToolName("file_change", "", "") != "apply_patch" ||
		codexToolName("mcp_tool_call", "slack", "post") != "slack/post" ||
		codexToolName("mcp_tool_call", "", "post") != "post" ||
		codexToolName("mcp_tool_call", "", "") != "mcp_tool_call" ||
		codexToolName("web_search", "", "") != "web_search" {
		t.Fatal("unexpected tool name mapping")
	}
	if codexToolStatus("completed") != "success" || codexToolStatus("FAILED") != "error" || codexToolStatus("declined") != "denied" || codexToolStatus("") != "success" {
		t.Fatal("unexpected tool status mapping")
	}
}

func TestCodexRun_HandlesHugeLines(t *testing.T) {
	big := strings.Repeat("x", 3<<20)
	stream := `{"type":"item.completed","item":{"id":"i1","type":"command_execution","aggregated_output":"` + big + `","status":"completed"}}` + "\n" +
		`{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"ok"}}` + "\n"
	run := &codexRun{}
	run.consume(strings.NewReader(stream), nil)
	if run.LastAgentMessage != "ok" || run.ToolCalls() != 1 {
		t.Fatalf("stream mis-parsed after huge line: %+v", run)
	}
}
