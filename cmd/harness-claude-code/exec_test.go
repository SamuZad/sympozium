package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestClaudeArgs(t *testing.T) {
	args := claudeArgs(claudeOptions{
		Task:          "do the thing",
		Workspace:     "/workspace",
		Model:         "claude-fable-5-1",
		SystemPrompt:  "persona",
		MCPConfigPath: "/workspace/.claude/sympozium-mcp.json",
		Continue:      true,
		MaxBudgetUSD:  "2.50",
		ExtraArgs:     []string{"--fallback-model", "claude-sonnet-5"},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-p",
		"--output-format stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--model claude-fable-5-1",
		"--append-system-prompt persona",
		"--mcp-config /workspace/.claude/sympozium-mcp.json --strict-mcp-config",
		"--continue",
		"--max-budget-usd 2.50",
		"--fallback-model claude-sonnet-5",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	// The task travels on stdin, never argv.
	if strings.Contains(joined, "do the thing") {
		t.Errorf("task must not appear in argv: %v", args)
	}

	minimal := strings.Join(claudeArgs(claudeOptions{Task: "x"}), " ")
	for _, absent := range []string{"--model", "--append-system-prompt", "--mcp-config", "--continue", "--max-budget-usd"} {
		if strings.Contains(minimal, absent) {
			t.Errorf("minimal args should not contain %q: %s", absent, minimal)
		}
	}
}

const sampleStream = `{"type":"system","subtype":"init","session_id":"abc","model":"claude-fable-5-1","tools":["Bash","Read"]}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]},"session_id":"abc"}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a.txt"}]},"session_id":"abc"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"a.txt"}}]},"session_id":"abc"}
this line is not json and must be ignored
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done: see [report](/workspace/report.md)."}]},"session_id":"abc"}
{"type":"result","subtype":"success","is_error":false,"duration_ms":4200,"num_turns":3,"result":"Done: see [report](/workspace/report.md).","session_id":"abc","total_cost_usd":0.0123,"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":300,"output_tokens":45}}
`

func TestStreamResult_ParsesSuccess(t *testing.T) {
	var tee bytes.Buffer
	s := &streamResult{}
	s.consume(strings.NewReader(sampleStream), &tee)

	if tee.String() != sampleStream {
		t.Fatal("every raw line must be teed to the log verbatim")
	}
	if s.Final == nil {
		t.Fatal("result event not captured")
	}
	if s.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", s.ToolCalls)
	}
	if got := s.Response(); got != "Done: see [report](/workspace/report.md)." {
		t.Errorf("Response = %q", got)
	}
	if s.InputTokens() != 420 || s.OutputTokens() != 45 {
		t.Errorf("tokens = in %d / out %d, want 420 / 45", s.InputTokens(), s.OutputTokens())
	}
	if s.Final.SessionID != "abc" || s.Final.NumTurns != 3 {
		t.Errorf("unexpected final event: %+v", s.Final)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

func TestStreamResult_PartialOutputWithoutResult(t *testing.T) {
	lines := strings.Split(sampleStream, "\n")
	// Drop the result line: simulates a kill/timeout mid-run.
	truncated := strings.Join(lines[:len(lines)-2], "\n") + "\n"
	s := &streamResult{}
	s.consume(strings.NewReader(truncated), nil)

	if s.Final != nil {
		t.Fatal("expected no result event")
	}
	if got := s.Response(); got != "Done: see [report](/workspace/report.md)." {
		t.Errorf("partial Response = %q, want last assistant text", got)
	}
	if s.InputTokens() != 0 || s.OutputTokens() != 0 {
		t.Error("no usage should be reported without a result event")
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "no result event") {
		t.Errorf("Err = %v, want no-result error", err)
	}
}

func TestStreamResult_ErrorSubtypes(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			"max turns",
			`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":10,"session_id":"x"}`,
			"error_max_turns",
		},
		{
			"string errors",
			`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom","bang"],"session_id":"x"}`,
			"error_during_execution: boom; bang",
		},
		{
			"object errors",
			`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":[{"message":"rate limited"}],"session_id":"x"}`,
			"rate limited",
		},
		{
			"is_error with success subtype",
			`{"type":"result","subtype":"success","is_error":true,"result":"Authentication failed","session_id":"x"}`,
			"success",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &streamResult{}
			s.consume(strings.NewReader(tc.line+"\n"), nil)
			err := s.Err()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Err = %v, want to contain %q", err, tc.want)
			}
		})
	}
}

func TestStreamResult_HandlesHugeLines(t *testing.T) {
	// Tool results can be megabytes; the reader must not truncate or stall.
	big := strings.Repeat("x", 3<<20)
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"` + big + `"}]}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"x"}` + "\n"
	s := &streamResult{}
	s.consume(strings.NewReader(line), nil)
	if s.Final == nil || s.Response() != "ok" {
		t.Fatalf("result lost after huge line: %+v", s.Final)
	}
}

func TestTailBuffer(t *testing.T) {
	tb := newTailBuffer(8)
	_, _ = tb.Write([]byte("0123456789"))
	if tb.String() != "23456789" {
		t.Fatalf("tail = %q", tb.String())
	}
	_, _ = tb.Write([]byte("ab"))
	if tb.String() != "456789ab" {
		t.Fatalf("tail = %q", tb.String())
	}
}
