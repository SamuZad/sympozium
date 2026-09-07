package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sympozium-ai/sympozium/internal/harness"
)

// claudeOptions is everything the shim decides about one `claude` invocation.
type claudeOptions struct {
	Task          string
	Workspace     string
	Model         string
	SystemPrompt  string
	MCPConfigPath string
	Continue      bool
	// MaxBudgetUSD caps API spend for the run (--max-budget-usd); empty
	// means no cap.
	MaxBudgetUSD string
	ExtraArgs    []string
}

// claudeArgs renders the argv for `claude`. The task itself is fed on stdin
// (see runClaude) so it is never subject to argv length limits or misread as
// a flag.
func claudeArgs(o claudeOptions) []string {
	args := []string{
		"-p",
		// stream-json gives us every event (tool calls, token usage, the
		// final result) as JSONL on stdout; --verbose is mandatory with it
		// in print mode.
		"--output-format", "stream-json",
		"--verbose",
		// The agent container is already locked down at the K8s pod level
		// (readOnlyRootFilesystem, all caps dropped, non-root UID,
		// NetworkPolicy) — that's the real security boundary, and there is
		// nobody to answer a permission prompt in a headless pod.
		"--dangerously-skip-permissions",
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", o.SystemPrompt)
	}
	if o.MCPConfigPath != "" {
		// --strict-mcp-config: use only our bridge, never MCP servers a
		// checked-out repo might declare in its own .mcp.json.
		args = append(args, "--mcp-config", o.MCPConfigPath, "--strict-mcp-config")
	}
	if o.Continue {
		args = append(args, "--continue")
	}
	if o.MaxBudgetUSD != "" {
		args = append(args, "--max-budget-usd", o.MaxBudgetUSD)
	}
	args = append(args, o.ExtraArgs...)
	return args
}

// runClaude executes `claude` in the workspace, tees its stream-json output to
// the pod log while parsing it, and returns the accumulated result, the tail
// of stderr (for error classification), and the process error if any.
func runClaude(ctx context.Context, o *harness.Observability, opts claudeOptions) (*streamResult, string, error) {
	ctx, span := o.StartExecSpan(ctx,
		attribute.String("claude_code.workspace", opts.Workspace),
		attribute.String("claude_code.model", opts.Model),
		attribute.Bool("claude_code.continue", opts.Continue),
	)
	defer span.End()

	cmd := exec.CommandContext(ctx, "claude", claudeArgs(opts)...)
	cmd.Dir = opts.Workspace
	cmd.Stdin = strings.NewReader(opts.Task)
	// On timeout/cancel ask nicely first so Claude Code can flush its
	// transcript and telemetry, then let WaitDelay escalate to SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second

	stderrTail := newTailBuffer(64 * 1024)
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrTail)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &streamResult{}, "", fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		harness.MarkSpanError(span, err)
		return &streamResult{}, "", fmt.Errorf("start claude: %w", err)
	}

	result := &streamResult{}
	result.consume(stdout, os.Stdout)

	runErr := cmd.Wait()
	if runErr != nil {
		harness.MarkSpanError(span, runErr)
		runErr = fmt.Errorf("claude exited: %w", runErr)
	}
	return result, stderrTail.String(), runErr
}

// streamEvent is the subset of Claude Code's stream-json events the shim
// cares about. Unknown fields and event types are ignored.
type streamEvent struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	IsError      bool            `json:"is_error"`
	Result       string          `json:"result"`
	SessionID    string          `json:"session_id"`
	NumTurns     int             `json:"num_turns"`
	DurationMs   int64           `json:"duration_ms"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Errors       json.RawMessage `json:"errors"`
	Usage        *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// streamResult accumulates what the shim learns from a stream-json run.
type streamResult struct {
	// Final is the terminal "result" event, nil if the process ended
	// without one (crash, timeout, kill).
	Final *streamEvent
	// ToolCalls counts tool_use blocks across assistant turns.
	ToolCalls int
	// LastAssistantText is the most recent assistant text block, used as a
	// partial answer when no result event arrived.
	LastAssistantText string
}

// consume reads JSONL events from r until EOF, echoing every raw line to tee
// (the pod log) and folding recognised events into the result.
func (s *streamResult) consume(r io.Reader, tee io.Writer) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if tee != nil {
				_, _ = tee.Write(line)
			}
			s.consumeLine(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *streamResult) consumeLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	switch ev.Type {
	case "assistant":
		if ev.Message == nil || len(ev.Message.Content) == 0 {
			return
		}
		var blocks []contentBlock
		if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				s.ToolCalls++
				harness.Logf(harnessName, "tool_use %s", b.Name)
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					s.LastAssistantText = b.Text
				}
			}
		}
	case "result":
		ev := ev
		s.Final = &ev
	}
}

// Response returns the final answer: the result event's text when present,
// otherwise the last assistant text seen (partial output).
func (s *streamResult) Response() string {
	if s.Final != nil && strings.TrimSpace(s.Final.Result) != "" {
		return strings.TrimSpace(s.Final.Result)
	}
	return strings.TrimSpace(s.LastAssistantText)
}

// InputTokens totals prompt-side tokens including cache writes and reads —
// they are all input the model processed, which is what the agent-runner's
// inputTokens metric represents.
func (s *streamResult) InputTokens() int {
	if s.Final == nil || s.Final.Usage == nil {
		return 0
	}
	u := s.Final.Usage
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

func (s *streamResult) OutputTokens() int {
	if s.Final == nil || s.Final.Usage == nil {
		return 0
	}
	return s.Final.Usage.OutputTokens
}

// Err classifies the terminal event: nil for a successful result, an error
// when Claude Code reported failure (is_error, or an error_* subtype such as
// error_max_turns) or when no result event arrived at all.
func (s *streamResult) Err() error {
	if s.Final == nil {
		return errors.New("claude produced no result event")
	}
	if !s.Final.IsError && !strings.HasPrefix(s.Final.Subtype, "error") {
		return nil
	}
	msg := s.Final.Subtype
	if msg == "" {
		msg = "error"
	}
	if details := decodeErrors(s.Final.Errors); details != "" {
		msg += ": " + details
	}
	return fmt.Errorf("claude reported %s", msg)
}

// decodeErrors flattens the result event's `errors` field, which Claude Code
// emits as a list of strings (older builds) or objects.
func decodeErrors(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, "; ")
	}
	var objs []map[string]any
	if err := json.Unmarshal(raw, &objs); err == nil {
		parts := make([]string, 0, len(objs))
		for _, o := range objs {
			if m, ok := o["message"].(string); ok && m != "" {
				parts = append(parts, m)
				continue
			}
			b, _ := json.Marshal(o)
			parts = append(parts, string(b))
		}
		return strings.Join(parts, "; ")
	}
	return string(raw)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
