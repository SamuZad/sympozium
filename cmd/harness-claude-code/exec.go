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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// TurnEndGrace is how long `claude` may keep running after the model
	// first ends its turn before the shim terminates it; 0 waits for a
	// natural exit. See runClaude.
	TurnEndGrace time.Duration
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
//
// In print mode Claude Code does not exit when the model ends its turn if
// background work it started is still pending: it waits for that work (up to
// its own time limit, ~10 minutes) and then wakes the model for a follow-up
// turn whose result replaces the real answer. Nobody is waiting for that
// follow-up in a pod, so once the model has ended its turn the shim gives
// `claude` opts.TurnEndGrace to exit on its own and then terminates its
// whole process group, keeping the answer it already has.
func runClaude(ctx context.Context, o *harness.Observability, opts claudeOptions) (*streamResult, string, error) {
	ctx, span := o.StartExecSpan(ctx,
		attribute.String("claude_code.workspace", opts.Workspace),
		attribute.String("claude_code.model", opts.Model),
		attribute.Bool("claude_code.continue", opts.Continue),
	)
	defer span.End()

	execCtx, stop := context.WithCancel(ctx)
	defer stop()
	cmd := exec.CommandContext(execCtx, "claude", claudeArgs(opts)...)
	cmd.Dir = opts.Workspace
	cmd.Stdin = strings.NewReader(opts.Task)
	// On timeout/cancel ask nicely first so Claude Code can flush its
	// transcript and telemetry, then let WaitDelay escalate to SIGKILL.
	// The process group also covers shells and sympozium-tool clients
	// Claude Code left running in the background.
	harness.UseProcessGroup(cmd, 15*time.Second)

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
	var settled atomic.Bool
	var graceTimer *time.Timer
	if opts.TurnEndGrace > 0 {
		result.onTurnEnd = func() {
			if graceTimer != nil {
				return
			}
			graceTimer = time.AfterFunc(opts.TurnEndGrace, func() {
				harness.Logf(harnessName, "claude still running %s after the turn ended (pending background work?); stopping it and keeping the answer", opts.TurnEndGrace)
				settled.Store(true)
				stop()
			})
		}
	}
	result.consume(stdout, os.Stdout)
	if graceTimer != nil {
		graceTimer.Stop()
	}

	runErr := cmd.Wait()
	// Reap anything that outlived claude itself (background shells,
	// sympozium-tool exec clients) so it cannot hold the pod open.
	harness.KillProcessGroup(cmd)
	if settled.Load() {
		// We stopped claude ourselves after it had answered; its exit
		// status (143 / signal) is not a failure of the run.
		result.Settled = true
		runErr = nil
	}
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
	// ParentToolUseID is set on events from inside a subagent (Task tool);
	// nil for the main conversation.
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Usage           *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Message *struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

// topLevel reports whether the event belongs to the main conversation rather
// than a subagent.
func (e *streamEvent) topLevel() bool {
	return e.ParentToolUseID == nil || *e.ParentToolUseID == ""
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use blocks
	ID   string `json:"id"`
	Name string `json:"name"`
	// tool_result blocks
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

// toolKey identifies a tool-invocation series (tool_name, status).
type toolKey struct {
	Name, Status string
}

// streamResult accumulates what the shim learns from a stream-json run.
type streamResult struct {
	// Final is the first "result" event, nil if the process ended without
	// one (crash, timeout, kill). Claude Code can emit more after follow-up
	// turns triggered by finished background tasks; those carry only the
	// follow-up's text, so they never replace the first.
	Final *streamEvent
	// Answers holds the text of every main-conversation turn that ended
	// (stop_reason end_turn), in order. Normally exactly one; more when
	// Claude Code woke the model again before exiting.
	Answers []string
	// Settled is set by runClaude when it stopped claude itself after the
	// model had already ended its turn.
	Settled bool
	// turnText collects main-conversation text since the last user event
	// (tool results), i.e. the text of the assistant message being built.
	turnText     []string
	turnAnswered bool
	// onTurnEnd, when set, is called each time the model ends its turn or a
	// result event arrives.
	onTurnEnd func()
	// ToolCalls counts tool_use blocks across assistant turns.
	ToolCalls int
	// ToolInvocations counts tool calls by (name, status), where status is
	// resolved from the matching tool_result block ("success"/"error").
	ToolInvocations map[toolKey]int
	// pendingTools maps tool_use ids to tool names until their result arrives.
	pendingTools map[string]string
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
		if ev.Message == nil {
			return
		}
		var blocks []contentBlock
		if len(ev.Message.Content) > 0 {
			if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
				return
			}
		}
		defer func() {
			if ev.topLevel() && ev.Message.StopReason == "end_turn" {
				s.endTurn()
			}
		}()
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				s.ToolCalls++
				if s.pendingTools == nil {
					s.pendingTools = map[string]string{}
				}
				if b.ID != "" {
					s.pendingTools[b.ID] = b.Name
				}
				harness.Logf(harnessName, "tool_use %s", b.Name)
			case "text":
				if ev.topLevel() && strings.TrimSpace(b.Text) != "" {
					s.LastAssistantText = b.Text
					s.turnText = append(s.turnText, strings.TrimSpace(b.Text))
				}
			}
		}
	case "user":
		// A main-conversation user event (tool results, or the notice of a
		// finished background task) starts a new assistant message.
		if ev.topLevel() {
			s.turnText = nil
			s.turnAnswered = false
		}
		// Tool results come back as user-role messages; resolve each
		// pending tool_use to a success/error outcome.
		if ev.Message == nil || len(ev.Message.Content) == 0 {
			return
		}
		var blocks []contentBlock
		if err := json.Unmarshal(ev.Message.Content, &blocks); err != nil {
			return
		}
		for _, b := range blocks {
			if b.Type != "tool_result" || b.ToolUseID == "" {
				continue
			}
			name, ok := s.pendingTools[b.ToolUseID]
			if !ok {
				continue
			}
			delete(s.pendingTools, b.ToolUseID)
			status := "success"
			if b.IsError {
				status = "error"
			}
			if s.ToolInvocations == nil {
				s.ToolInvocations = map[toolKey]int{}
			}
			s.ToolInvocations[toolKey{Name: name, Status: status}]++
		}
	case "result":
		if s.Final == nil {
			ev := ev
			s.Final = &ev
		}
		if s.onTurnEnd != nil {
			s.onTurnEnd()
		}
	}
}

// endTurn records the current assistant message as an answer. Claude Code
// may emit one event per content block, each carrying the message's
// stop_reason, so the turn's slot is overwritten rather than appended twice.
func (s *streamResult) endTurn() {
	ans := strings.Join(s.turnText, "\n\n")
	if s.turnAnswered {
		s.Answers[len(s.Answers)-1] = ans
	} else {
		s.Answers = append(s.Answers, ans)
		s.turnAnswered = true
	}
	if s.onTurnEnd != nil {
		s.onTurnEnd()
	}
}

// Usage returns the run's token usage in disjoint buckets, or zero when no
// result event arrived. Claude Code's input_tokens already excludes cache
// traffic, so the buckets map one-to-one.
func (s *streamResult) Usage() harness.TokenUsage {
	if s.Final == nil || s.Final.Usage == nil {
		return harness.TokenUsage{}
	}
	u := s.Final.Usage
	return harness.TokenUsage{
		Input:      int64(u.InputTokens),
		Output:     int64(u.OutputTokens),
		CacheRead:  int64(u.CacheReadInputTokens),
		CacheWrite: int64(u.CacheCreationInputTokens),
	}
}

// SortedToolInvocations returns (name, status, count) triples in a stable
// order. Tool calls whose result never arrived (run killed mid-tool) are
// reported with status "unknown".
func (s *streamResult) SortedToolInvocations() []struct {
	Key   toolKey
	Count int
} {
	merged := map[toolKey]int{}
	for k, v := range s.ToolInvocations {
		merged[k] += v
	}
	for _, name := range s.pendingTools {
		merged[toolKey{Name: name, Status: "unknown"}]++
	}
	out := make([]struct {
		Key   toolKey
		Count int
	}, 0, len(merged))
	for k, v := range merged {
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

// Response returns the final answer: the text of every turn the model ended,
// joined, so a follow-up turn's addendum never hides the answer before it.
// Falls back to the result event's text (if stop_reason was missing from the
// stream), then to the last assistant text seen (partial output).
func (s *streamResult) Response() string {
	var answers []string
	for _, a := range s.Answers {
		if a != "" {
			answers = append(answers, a)
		}
	}
	if len(answers) > 0 {
		return strings.Join(answers, "\n\n")
	}
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
		if s.Settled && s.Response() != "" {
			// The model answered; claude was stopped while waiting on
			// background work, before it printed the result event.
			return nil
		}
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
