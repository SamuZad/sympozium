// Package jsonlog provides the shared Sympozium JSONL log contract.
package jsonlog

import (
	"bytes"
	"encoding/json"
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
	var native any
	if json.Unmarshal([]byte(raw), &native) == nil {
		if _, ok := native.(map[string]any); ok {
			data["native_event"] = native
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
	Emit(w.out, w.component, w.harness, level, "process.output", "Native process output", w.stream, data)
}
