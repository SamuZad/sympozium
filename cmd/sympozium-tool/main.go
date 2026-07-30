// Command sympozium-tool is a thin CLI exposing Sympozium-specific
// capabilities as shell commands. It lets harnesses whose only general-
// purpose tool is a shell (e.g. codex) invoke the same capabilities that
// agent-runner exposes natively as function-calling tools.
//
// IPC subcommands (send-message, schedule, exec) speak the fsnotify+JSON
// contract the IPC bridge consumes. Memory subcommands (memory-search,
// memory-store, memory-list) talk HTTP to the central memory-server using
// the pod's projected ServiceAccount token, identical to agent-runner.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sympozium-ai/sympozium/pkg/memoryclient"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "send-message":
		os.Exit(cmdSendMessage(args))
	case "schedule":
		os.Exit(cmdSchedule(args))
	case "exec":
		os.Exit(cmdExec(args))
	case "memory-search":
		os.Exit(cmdMemorySearch(args))
	case "memory-store":
		os.Exit(cmdMemoryStore(args))
	case "memory-list":
		os.Exit(cmdMemoryList(args))
	case "get-attachment":
		os.Exit(cmdGetAttachment(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `sympozium-tool — invoke Sympozium tools from a shell.

Subcommands (IPC):
  send-message   Send a message to a connected channel (Telegram/Slack/Discord/WhatsApp).
  schedule       Create, update, suspend, resume, or delete a recurring schedule.
  exec           Run a command in a SkillPack sidecar (with the sidecar's RBAC).

Subcommands (memory):
  memory-search  Search persistent memory for past findings.
  memory-store   Persist a finding to memory for future runs.
  memory-list    List recent memory entries.

Subcommands (attachments):
  get-attachment Download a channel attachment from the artifact-server by ID.

Run "sympozium-tool <subcommand> --help" for subcommand options.`)
}

// --- send-message -----------------------------------------------------------

func cmdSendMessage(argv []string) int {
	fs := flag.NewFlagSet("send-message", flag.ContinueOnError)
	channel := fs.String("channel", "", "Channel type: telegram | slack | discord | whatsapp")
	text := fs.String("text", "", "Message text (required)")
	chatID := fs.String("chat-id", "", "Target chat/group ID. Empty = owner/self-chat.")
	threadID := fs.String("thread-id", "", "Optional thread id (Slack thread_ts, Discord thread id).")
	format := fs.String("format", "", "Optional format hint: plain | markdown | html")
	var attachmentPaths repeatedStringFlag
	var attachmentURLs repeatedStringFlag
	fs.Var(&attachmentPaths, "attachment-path", "Local image path to attach. May be repeated. Must be under /workspace, /tmp, /skills, or /ipc.")
	fs.Var(&attachmentURLs, "attachment-url", "Public https image URL to attach. May be repeated.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool send-message --channel <name> --text "..." [--chat-id ID] [--thread-id ID] [--format plain|markdown|html] [--attachment-path /tmp/chart.png]

Writes /ipc/messages/send-<ts>.json. The IPC bridge relays it to the channel pod.
Fire-and-forget: exits 0 once the message file is written.`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *channel == "" {
		fmt.Fprintln(os.Stderr, "error: --channel is required")
		return 2
	}
	if *text == "" {
		fmt.Fprintln(os.Stderr, "error: --text is required")
		return 2
	}
	attachments, err := buildSendMessageAttachments(attachmentPaths, attachmentURLs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	msg := struct {
		Channel     string                  `json:"channel"`
		ChatID      string                  `json:"chatId,omitempty"`
		ThreadID    string                  `json:"threadId,omitempty"`
		Text        string                  `json:"text"`
		Format      string                  `json:"format,omitempty"`
		Attachments []sendMessageAttachment `json:"attachments,omitempty"`
	}{
		Channel:     *channel,
		ChatID:      *chatID,
		ThreadID:    *threadID,
		Text:        *text,
		Format:      *format,
		Attachments: attachments,
	}
	return writeIPC("/ipc/messages", "send", msg, fmt.Sprintf("Message queued for %s channel", *channel))
}

type repeatedStringFlag []string

func (f *repeatedStringFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value cannot be empty")
	}
	*f = append(*f, value)
	return nil
}

type sendMessageAttachment struct {
	Type          string `json:"type"`
	URL           string `json:"url,omitempty"`
	ContentBase64 string `json:"contentBase64,omitempty"`
	Filename      string `json:"filename,omitempty"`
	MimeType      string `json:"mimeType,omitempty"`
}

const (
	defaultChannelAttachmentMaxBytes = int64(750 * 1024)
	channelAttachmentMaxBytesEnv     = "CHANNEL_ATTACHMENT_MAX_BYTES"
)

func buildSendMessageAttachments(paths, urls []string) ([]sendMessageAttachment, error) {
	if len(paths)+len(urls) > 10 {
		return nil, fmt.Errorf("attachments supports at most 10 items")
	}
	attachments := make([]sendMessageAttachment, 0, len(paths)+len(urls))
	for _, url := range urls {
		if !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("attachment URL must start with https://")
		}
		attachments = append(attachments, sendMessageAttachment{Type: "image", URL: url})
	}
	for i, path := range paths {
		attachment, err := readSendMessageAttachmentFile(i, path)
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, attachment)
	}
	return attachments, nil
}

func readSendMessageAttachmentFile(index int, path string) (sendMessageAttachment, error) {
	cleanPath := filepath.Clean(path)
	if !isAllowedSendMessageAttachmentPath(cleanPath) {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d path access denied; path must be under /workspace, /skills, /tmp, or /ipc", index)
	}
	maxBytes, err := configuredChannelAttachmentMaxBytes()
	if err != nil {
		return sendMessageAttachment{}, err
	}
	info, err := os.Stat(cleanPath)
	if err != nil {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d path is not readable: %w", index, err)
	}
	if info.IsDir() {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d path is a directory", index)
	}
	if info.Size() == 0 {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d path is empty", index)
	}
	if info.Size() > maxBytes {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d is %d bytes; max is %d bytes", index, info.Size(), maxBytes)
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d path is not readable: %w", index, err)
	}
	if int64(len(data)) > maxBytes {
		return sendMessageAttachment{}, fmt.Errorf("attachment %d is %d bytes; max is %d bytes", index, len(data), maxBytes)
	}
	mimeType := http.DetectContentType(data)
	attachmentType := "file"
	if strings.HasPrefix(mimeType, "image/") {
		attachmentType = "image"
	}

	return sendMessageAttachment{
		Type:          attachmentType,
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		Filename:      filepath.Base(cleanPath),
		MimeType:      mimeType,
	}, nil
}

func configuredChannelAttachmentMaxBytes() (int64, error) {
	raw := strings.TrimSpace(os.Getenv(channelAttachmentMaxBytesEnv))
	if raw == "" {
		return defaultChannelAttachmentMaxBytes, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer byte count", channelAttachmentMaxBytesEnv)
	}
	return value, nil
}

func isAllowedSendMessageAttachmentPath(path string) bool {
	for _, prefix := range []string{"/workspace", "/skills", "/tmp", "/ipc"} {
		if path == prefix || strings.HasPrefix(path, prefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// --- get-attachment ---------------------------------------------------------

func cmdGetAttachment(argv []string) int {
	fs := flag.NewFlagSet("get-attachment", flag.ContinueOnError)
	id := fs.String("id", "", "Artifact ID (required)")
	output := fs.String("output", "", "Output file path. Defaults to the artifact's original filename in the current directory.")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool get-attachment --id ARTIFACT_ID [--output PATH]

Downloads an attachment from the artifact-server using the pod's ServiceAccount
token. Inbound channel attachments arrive as artifact IDs (see the
INBOUND_ATTACHMENTS environment variable or the run's attachment listing).`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *id == "" {
		fmt.Fprintln(os.Stderr, "error: --id is required")
		return 2
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ARTIFACT_SERVER_URL")), "/")
	if base == "" {
		fmt.Fprintln(os.Stderr, "error: ARTIFACT_SERVER_URL is not configured")
		return 1
	}
	tokenPath := os.Getenv("SA_TOKEN_PATH")
	if tokenPath == "" {
		tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read ServiceAccount token: %v\n", err)
		return 1
	}
	token := strings.TrimSpace(string(tokenBytes))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/artifacts/"+url.PathEscape(*id), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		fmt.Fprintf(os.Stderr, "error: artifact-server HTTP %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	path := *output
	if path == "" {
		filename := ""
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
			filename = filepath.Base(strings.TrimSpace(params["filename"]))
		}
		if filename == "" || filename == "." || filename == string(os.PathSeparator) {
			filename = *id
		}
		path = filename
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	n, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", path, err)
		return 1
	}
	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}
	fmt.Printf("saved %s (%d bytes)\n", abs, n)
	return 0
}

// --- schedule ---------------------------------------------------------------

func cmdSchedule(argv []string) int {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	name := fs.String("name", "", "Schedule name (required)")
	action := fs.String("action", "", "create | update | suspend | resume | delete (required)")
	schedule := fs.String("schedule", "", "Cron expression (required for create; optional for update)")
	task := fs.String("task", "", "Task description fired on each run (required for create)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool schedule --name NAME --action <create|update|suspend|resume|delete> [--schedule CRON] [--task "..."]

Writes /ipc/schedules/schedule-<ts>.json. The IPC bridge relays it to the controller,
which creates/updates the corresponding SympoziumSchedule.

Examples:
  sympozium-tool schedule --name daily-report --action create --schedule "0 9 * * 1-5" --task "Summarise yesterday's incidents"
  sympozium-tool schedule --name daily-report --action suspend`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required")
		return 2
	}
	switch *action {
	case "":
		fmt.Fprintln(os.Stderr, "error: --action is required (create|update|suspend|resume|delete)")
		return 2
	case "create":
		if *schedule == "" {
			fmt.Fprintln(os.Stderr, "error: --schedule is required for create")
			return 2
		}
		if *task == "" {
			fmt.Fprintln(os.Stderr, "error: --task is required for create")
			return 2
		}
	case "update":
		if *schedule == "" && *task == "" {
			fmt.Fprintln(os.Stderr, "error: --schedule and/or --task is required for update")
			return 2
		}
	case "suspend", "resume", "delete":
		// name + action only.
	default:
		fmt.Fprintf(os.Stderr, "error: unknown --action %q (use create|update|suspend|resume|delete)\n", *action)
		return 2
	}

	req := struct {
		Name     string `json:"name"`
		Action   string `json:"action"`
		Schedule string `json:"schedule,omitempty"`
		Task     string `json:"task,omitempty"`
	}{Name: *name, Action: *action, Schedule: *schedule, Task: *task}
	return writeIPC("/ipc/schedules", "schedule", req, fmt.Sprintf("Schedule %q %s", *name, *action))
}

// --- exec -------------------------------------------------------------------

func cmdExec(argv []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	target := fs.String("target", "", "SkillPack sidecar to dispatch into (e.g. github-gitops, k8s-ops).")
	workdir := fs.String("workdir", "/workspace", "Working directory inside the sidecar.")
	timeout := fs.Int("timeout", 30, "Timeout in seconds (max 120).")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool exec [--target NAME] [--workdir DIR] [--timeout SECS] -- <command> [args...]

Runs <command> inside the named SkillPack sidecar via /ipc/tools/. The sidecar
executes with its own ServiceAccount RBAC (this is how, e.g., the k8s-ops
sidecar can call kubectl).

Stdout/stderr from the command are printed; this process exits with the
command's exit code.

Examples:
  sympozium-tool exec --target k8s-ops -- kubectl get nodes
  sympozium-tool exec --target github-gitops -- gh pr list --repo me/proj`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	cmdAndArgs := fs.Args()
	if len(cmdAndArgs) == 0 {
		fmt.Fprintln(os.Stderr, "error: command is required after --")
		return 2
	}
	if *timeout <= 0 {
		*timeout = 30
	}
	if *timeout > 120 {
		*timeout = 120
	}
	resolvedTarget, err := resolveExecTarget(*target, os.Getenv("SYMPOZIUM_SKILL_TARGETS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	// Forward piped stdin (e.g. `... -- python - <<'PY' ... PY`) to the
	// sidecar command. The heredoc is consumed by the harness shell and
	// arrives here as this process's stdin; without forwarding it, `python -`
	// (or any stdin-reading command) would run against empty input in the
	// sidecar and silently do nothing.
	stdinData, err := readExecStdin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	req := struct {
		ID      string            `json:"id"`
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Stdin   string            `json:"stdin,omitempty"`
		WorkDir string            `json:"workDir,omitempty"`
		Timeout int               `json:"timeout,omitempty"`
		Target  string            `json:"target,omitempty"`
		Meta    map[string]string `json:"_meta,omitempty"`
	}{
		ID:      id,
		Command: cmdAndArgs[0],
		Args:    cmdAndArgs[1:],
		Stdin:   stdinData,
		WorkDir: *workdir,
		Timeout: *timeout,
		Target:  resolvedTarget,
	}

	toolsDir := "/ipc/tools"
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create %s: %v\n", toolsDir, err)
		return 1
	}
	reqPath := filepath.Join(toolsDir, fmt.Sprintf("exec-request-%s.json", id))
	resPath := filepath.Join(toolsDir, fmt.Sprintf("exec-result-%s.json", id))

	data, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal exec request: %v\n", err)
		return 1
	}
	if err := os.WriteFile(reqPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write exec request: %v\n", err)
		return 1
	}

	deadline := time.Now().Add(time.Duration(*timeout+10) * time.Second)
	for time.Now().Before(deadline) {
		resData, err := os.ReadFile(resPath)
		if err == nil && len(resData) > 0 {
			var result struct {
				ID       string `json:"id"`
				ExitCode int    `json:"exitCode"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
				TimedOut bool   `json:"timedOut,omitempty"`
			}
			if err := json.Unmarshal(resData, &result); err != nil {
				// Partial write — retry once after a short sleep.
				time.Sleep(100 * time.Millisecond)
				resData2, _ := os.ReadFile(resPath)
				if err := json.Unmarshal(resData2, &result); err != nil {
					fmt.Fprintf(os.Stderr, "error: parse exec result: %v\n", err)
					_ = os.Remove(reqPath)
					_ = os.Remove(resPath)
					return 1
				}
			}
			_ = os.Remove(reqPath)
			_ = os.Remove(resPath)
			if result.Stdout != "" {
				fmt.Print(result.Stdout)
				if !strings.HasSuffix(result.Stdout, "\n") {
					fmt.Println()
				}
			}
			if result.Stderr != "" {
				fmt.Fprint(os.Stderr, result.Stderr)
				if !strings.HasSuffix(result.Stderr, "\n") {
					fmt.Fprintln(os.Stderr)
				}
			}
			if result.TimedOut {
				fmt.Fprintln(os.Stderr, "(command timed out)")
			}
			return result.ExitCode
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "error: timed out waiting for sidecar response — is the named SkillPack sidecar attached?")
	_ = os.Remove(reqPath)
	return 124
}

// execStdinMaxBytes bounds forwarded stdin so a single exec request stays a
// reasonable size on the /ipc/tools tmpfs and in the JSON payload.
const execStdinMaxBytes = 1 << 20 // 1 MiB

// readExecStdin returns piped stdin for an exec invocation. It returns "" when
// stdin is a character device (an interactive terminal, or /dev/null as the
// codex harness supplies when no heredoc is present) and errors if the input
// exceeds execStdinMaxBytes. A heredoc/pipe/file is a non-char-device, so it is
// read and forwarded verbatim.
func readExecStdin() (string, error) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", nil // no usable stdin; treat as empty
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return "", nil // terminal or /dev/null — nothing piped
	}
	buf, err := io.ReadAll(io.LimitReader(os.Stdin, execStdinMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(buf) > execStdinMaxBytes {
		return "", fmt.Errorf("stdin exceeds %d bytes", execStdinMaxBytes)
	}
	return string(buf), nil
}

func resolveExecTarget(input, rawInventory string) (string, error) {
	in := normalizeExecTarget(input)
	if in == "" {
		return "", nil
	}
	inventory := parseExecTargets(rawInventory)
	if len(inventory) == 0 {
		return in, nil
	}
	for _, target := range inventory {
		if target == in {
			return target, nil
		}
	}
	var matches []string
	for _, target := range inventory {
		if strings.HasSuffix(target, "-"+in) {
			matches = append(matches, target)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous target %q matches multiple sidecars: %s; use the full SkillPack name", input, strings.Join(matches, ", "))
	}
	return "", fmt.Errorf("unknown target %q. Available targets: %s", input, strings.Join(inventory, ", "))
}

func parseExecTargets(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	targets := make([]string, 0, len(parts))
	for _, part := range parts {
		if target := normalizeExecTarget(part); target != "" {
			targets = append(targets, target)
		}
	}
	return targets
}

func normalizeExecTarget(target string) string {
	return strings.ToLower(strings.TrimSpace(target))
}

// --- shared -----------------------------------------------------------------

func writeIPC(dir, prefix string, payload any, okMsg string) int {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create %s: %v\n", dir, err)
		return 1
	}
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal request: %v\n", err)
		return 1
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", prefix, id))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", path, err)
		return 1
	}
	fmt.Println(okMsg)
	return 0
}

// --- memory -----------------------------------------------------------------

// newMemoryClient builds a memoryclient pointing at MEMORY_SERVER_URL and
// authenticated with the pod's projected ServiceAccount token. Same defaults
// agent-runner uses, so memory entries created from the codex shell land in
// the same scope and are visible to all harnesses in the ensemble.
func newMemoryClient() (*memoryclient.Client, error) {
	base := strings.TrimRight(os.Getenv("MEMORY_SERVER_URL"), "/")
	if base == "" {
		return nil, fmt.Errorf("MEMORY_SERVER_URL is not set; memory is disabled for this run")
	}
	return memoryclient.New(base), nil
}

func validateScope(s string) (string, error) {
	if s == "" {
		return "agent", nil
	}
	if s != "agent" && s != "ensemble" {
		return "", fmt.Errorf("--scope must be 'agent' or 'ensemble', got %q", s)
	}
	return s, nil
}

func cmdMemorySearch(argv []string) int {
	fs := flag.NewFlagSet("memory-search", flag.ContinueOnError)
	query := fs.String("query", "", "Natural language search query (required)")
	topK := fs.Int("top-k", 5, "Maximum number of results")
	scope := fs.String("scope", "agent", "agent | ensemble")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool memory-search --query "..." [--top-k 5] [--scope agent|ensemble]

Hybrid (vector + full-text) search over persistent memory. Returns the top hits
the caller is authorised to see in the chosen scope. Use 'agent' for your own
private notes, 'ensemble' for memory shared with other personas in the same
ensemble (governed by the ensemble's membrane visibility rules).`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *query == "" {
		fmt.Fprintln(os.Stderr, "error: --query is required")
		return 2
	}
	sc, err := validateScope(*scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	c, err := newMemoryClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	req := memoryclient.SearchRequest{Scope: sc, Query: *query, TopK: *topK}
	if sc == "ensemble" {
		req.EnsembleName = os.Getenv("ENSEMBLE_NAME")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hits, err := c.Search(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory search error:", err)
		return 1
	}
	printHits(hits, sc, "search")
	return 0
}

func cmdMemoryStore(argv []string) int {
	fs := flag.NewFlagSet("memory-store", flag.ContinueOnError)
	content := fs.String("content", "", "Content to store (required)")
	tagsRaw := fs.String("tags", "", "Comma-separated tags (e.g. 'kafka,consumer-lag')")
	scope := fs.String("scope", "agent", "agent | ensemble")
	visibility := fs.String("visibility", "", "public | trusted (ensemble scope only)")
	parentID := fs.String("parent-id", "", "Optional parent memory id (for threaded notes)")
	ttlDays := fs.Int("ttl-days", 0, "Optional expiry in days (0 = never)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool memory-store --content "..." [--tags a,b] [--scope agent|ensemble] [--visibility public|trusted] [--parent-id ID] [--ttl-days N]

Persist a finding for future runs. Be specific: include root cause, resolution
steps, service names, and namespaces. 'agent' scope (default) is private to
this agent; 'ensemble' scope shares with other personas per the ensemble's
membrane visibility rules.`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *content == "" {
		fmt.Fprintln(os.Stderr, "error: --content is required")
		return 2
	}
	sc, err := validateScope(*scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	var tags []string
	if *tagsRaw != "" {
		for _, t := range strings.Split(*tagsRaw, ",") {
			if v := strings.TrimSpace(t); v != "" {
				tags = append(tags, v)
			}
		}
	}
	c, err := newMemoryClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	req := memoryclient.StoreRequest{
		Scope:    sc,
		Content:  *content,
		Tags:     tags,
		ParentID: *parentID,
		TTLDays:  *ttlDays,
	}
	if sc == "ensemble" {
		req.EnsembleName = os.Getenv("ENSEMBLE_NAME")
		req.Visibility = *visibility
		if req.Visibility == "" {
			req.Visibility = os.Getenv("WORKFLOW_MEMBRANE_VISIBILITY")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entry, err := c.Store(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory store error:", err)
		return 1
	}
	if entry != nil && entry.ID != "" {
		fmt.Printf("Stored memory %s (scope=%s)\n", entry.ID, sc)
	} else {
		fmt.Printf("Stored memory (scope=%s)\n", sc)
	}
	return 0
}

func cmdMemoryList(argv []string) int {
	fs := flag.NewFlagSet("memory-list", flag.ContinueOnError)
	scope := fs.String("scope", "agent", "agent | ensemble")
	limit := fs.Int("limit", 20, "Maximum entries to return")
	offset := fs.Int("offset", 0, "Skip this many entries (pagination)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool memory-list [--scope agent|ensemble] [--limit 20] [--offset 0]

Return the most recent memory entries the caller is authorised to see in the
chosen scope, ordered newest first. Use memory-search instead if you need
tag-narrowed or query-driven reads.`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	sc, err := validateScope(*scope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	c, err := newMemoryClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	req := memoryclient.ListRequest{Scope: sc, Limit: *limit, Offset: *offset}
	if sc == "ensemble" {
		req.EnsembleName = os.Getenv("ENSEMBLE_NAME")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hits, err := c.List(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory list error:", err)
		return 1
	}
	printHits(hits, sc, "list")
	return 0
}

// printHits renders search/list results as markdown — codex's training data
// is heavy on markdown, so this is the most legible shape for the model.
func printHits(hits []memoryclient.SearchHit, scope, op string) {
	if len(hits) == 0 {
		fmt.Printf("(no results from memory-%s in scope=%s)\n", op, scope)
		return
	}
	fmt.Printf("Found %d result(s) in scope=%s:\n\n", len(hits), scope)
	for i, h := range hits {
		fmt.Printf("## [%d] %s\n", i+1, h.ID)
		if !h.CreatedAt.IsZero() {
			fmt.Printf("- created: %s\n", h.CreatedAt.UTC().Format(time.RFC3339))
		}
		if len(h.Tags) > 0 {
			fmt.Printf("- tags: %s\n", strings.Join(h.Tags, ", "))
		}
		if h.Visibility != "" {
			fmt.Printf("- visibility: %s\n", h.Visibility)
		}
		if h.Score > 0 {
			fmt.Printf("- score: %.3f\n", h.Score)
		}
		fmt.Println()
		fmt.Println(h.Content)
		fmt.Println()
	}
}
