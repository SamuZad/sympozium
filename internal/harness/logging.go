package harness

import (
	"fmt"
	"io"
	"os"

	"github.com/sympozium-ai/sympozium/internal/jsonlog"
)

// LogEvent is the stable JSONL schema emitted by Sympozium harnesses.
type LogEvent = jsonlog.Event

// ProcessOutputWriter converts arbitrary child stdout/stderr into Sympozium
// JSONL while preserving native JSON under data.native_event.
type ProcessOutputWriter = jsonlog.ProcessOutputWriter

func NewProcessOutputWriter(out io.Writer, harness, stream string) *ProcessOutputWriter {
	return jsonlog.NewProcessOutputWriter(out, "harness", harness, stream)
}

// EmitLog writes one JSON object followed by exactly one newline. Message is
// always Sympozium-owned, human-readable plain text; variable data is in Data.
func EmitLog(out io.Writer, harness, level, event, message, stream string, data map[string]any) {
	jsonlog.Emit(out, "harness", harness, level, event, message, stream, data)
}

// Logf emits a harness diagnostic in the same JSONL schema as child output.
func Logf(name, format string, args ...any) {
	EmitLog(os.Stderr, name, "info", "harness.log", fmt.Sprintf(format, args...), "stderr", nil)
}
