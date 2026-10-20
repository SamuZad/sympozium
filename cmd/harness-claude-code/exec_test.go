package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sympozium-ai/sympozium/internal/harness"
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
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"no such file","is_error":true}]},"session_id":"abc"}
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

	// Token buckets map one-to-one onto Claude Code's usage fields.
	if got := s.Usage(); got != (harness.TokenUsage{Input: 100, Output: 45, CacheRead: 300, CacheWrite: 20}) {
		t.Errorf("Usage = %+v", got)
	}
	// Tool outcomes are resolved from the matching tool_result blocks.
	inv := s.SortedToolInvocations()
	if len(inv) != 2 || inv[0].Key != (toolKey{"Bash", "success"}) || inv[1].Key != (toolKey{"Read", "error"}) {
		t.Errorf("tool invocations = %+v", inv)
	}
}

func TestStreamResult_UnresolvedToolIsUnknown(t *testing.T) {
	s := &streamResult{}
	s.consume(strings.NewReader(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t9","name":"Bash","input":{}}]}}`+"\n"), nil)
	inv := s.SortedToolInvocations()
	if len(inv) != 1 || inv[0].Key != (toolKey{"Bash", "unknown"}) || inv[0].Count != 1 {
		t.Fatalf("tool without result should be reported unknown: %+v", inv)
	}
	if s.Usage() != (harness.TokenUsage{}) {
		t.Fatal("no result event → zero usage")
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

// backgroundFollowUpStream is what Claude Code prints when the model answers
// while a backgrounded command is still running: the answer turn ends, then
// minutes later the task finishes, the model is woken for a follow-up turn,
// and only that follow-up's text lands in the result event.
const backgroundFollowUpStream = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"uv run chart.py"}}],"stop_reason":"tool_use"}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"Command running in background"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Here is the full analysis."}],"stop_reason":null}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Charts attached."}],"stop_reason":"end_turn"}}
{"type":"user","message":{"role":"user","content":"<task-notification>chart.py timed out</task-notification>"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"One addendum: the first render timed out."}],"stop_reason":"end_turn"}}
{"type":"result","subtype":"success","is_error":false,"result":"One addendum: the first render timed out.","session_id":"abc"}
`

func TestStreamResult_FollowUpTurnKeepsAnswer(t *testing.T) {
	s := &streamResult{}
	s.consume(strings.NewReader(backgroundFollowUpStream), nil)

	want := "Here is the full analysis.\n\nCharts attached.\n\nOne addendum: the first render timed out."
	if got := s.Response(); got != want {
		t.Errorf("Response = %q, want %q", got, want)
	}
	if len(s.Answers) != 2 {
		t.Errorf("Answers = %q, want 2 turns", s.Answers)
	}
}

func TestStreamResult_KeepsFirstResultEvent(t *testing.T) {
	s := &streamResult{}
	s.consume(strings.NewReader(
		`{"type":"result","subtype":"success","result":"answer","num_turns":4,"session_id":"x"}`+"\n"+
			`{"type":"result","subtype":"success","result":"","num_turns":0,"session_id":"x"}`+"\n"), nil)
	if s.Final == nil || s.Final.NumTurns != 4 || s.Response() != "answer" {
		t.Fatalf("follow-up result must not replace the first: %+v", s.Final)
	}
}

func TestStreamResult_PerBlockEndTurnCountsOnce(t *testing.T) {
	// One assistant message streamed as one event per content block, each
	// carrying the message's stop_reason.
	s := &streamResult{}
	s.consume(strings.NewReader(
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Part one."}],"stop_reason":"end_turn"}}`+"\n"+
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Part two."}],"stop_reason":"end_turn"}}`+"\n"), nil)
	if len(s.Answers) != 1 || s.Response() != "Part one.\n\nPart two." {
		t.Fatalf("Answers = %q", s.Answers)
	}
}

func TestStreamResult_IgnoresSubagentText(t *testing.T) {
	s := &streamResult{}
	s.consume(strings.NewReader(
		`{"type":"assistant","parent_tool_use_id":"task1","message":{"role":"assistant","content":[{"type":"text","text":"subagent report"}],"stop_reason":"end_turn"}}`+"\n"+
			`{"type":"assistant","parent_tool_use_id":null,"message":{"role":"assistant","content":[{"type":"text","text":"Main answer."}],"stop_reason":"end_turn"}}`+"\n"), nil)
	if s.Response() != "Main answer." {
		t.Fatalf("Response = %q, subagent text must not leak into the answer", s.Response())
	}
}

func TestStreamResult_SettledWithoutResultIsSuccess(t *testing.T) {
	s := &streamResult{}
	s.consume(strings.NewReader(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"stop_reason":"end_turn"}}`+"\n"), nil)
	if s.Err() == nil {
		t.Fatal("a run that just stopped without a result event is an error")
	}
	s.Settled = true
	if err := s.Err(); err != nil {
		t.Fatalf("answered then stopped by the shim should succeed, got %v", err)
	}
}

// fakeClaude puts a `claude` shell script on PATH for runClaude to exec.
func fakeClaude(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunClaude_StopsAfterTurnEndWhenBackgroundWorkHangs(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	t.Setenv("BG_PID_FILE", pidFile)
	// Answers, then keeps running on a backgrounded child that also holds
	// stdout open — the shape of Claude Code waiting on a hung task.
	fakeClaude(t, `cat >/dev/null
sleep 300 &
echo $! > "$BG_PID_FILE"
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The real answer."}],"stop_reason":"end_turn"}}'
wait
`)
	o := harness.InitObservability(context.Background(), harnessName)
	start := time.Now()
	res, _, err := runClaude(context.Background(), o, claudeOptions{
		Task: "x", Workspace: t.TempDir(), TurnEndGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("runClaude err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runClaude took %v; the grace cutoff did not fire", elapsed)
	}
	if !res.Settled || res.Err() != nil || res.Response() != "The real answer." {
		t.Fatalf("settled=%v err=%v response=%q", res.Settled, res.Err(), res.Response())
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived the process-group kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunClaude_CleanExitIsNotSettled(t *testing.T) {
	fakeClaude(t, `cat >/dev/null
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}}'
echo '{"type":"result","subtype":"success","result":"ok","session_id":"x"}'
`)
	o := harness.InitObservability(context.Background(), harnessName)
	res, _, err := runClaude(context.Background(), o, claudeOptions{
		Task: "x", Workspace: t.TempDir(), TurnEndGrace: 5 * time.Second,
	})
	if err != nil || res.Settled || res.Err() != nil || res.Response() != "ok" {
		t.Fatalf("err=%v settled=%v resErr=%v response=%q", err, res.Settled, res.Err(), res.Response())
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
