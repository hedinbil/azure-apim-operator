/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// Helpers for the logs-predicates tests: a log sink that keeps level, error and keys in
// order, a parser for the zap console lines the suite's global logger writes, and a
// settable clock. Everything is prefixed lp so it cannot collide with other test files.

// The exact log messages of an APIM write, spelled out as literals rather than taken
// from the constants in retry.go: the stalled line is matched verbatim by a Datadog log
// monitor, so a change to any of them must fail a test, not slip through with the
// constant.
const (
	lpMsgStarting   = "▶️ APIM write starting"
	lpMsgSucceeded  = "💚 APIM write succeeded"
	lpMsgFailed     = "💔 APIM write failed; backing off"
	lpMsgRejected   = "💔 APIM write rejected; not retrying"
	lpMsgBackingOff = "⏸️ APIM write still backing off; skipping"
	lpMsgHeld       = "⏸️ APIM write stopped; waiting for a spec change or the retry annotation"
	lpMsgStalled    = "🛑 APIM write stalled"
	lpMsgReset      = "🔁 APIM write failures cleared"
)

// lpLine is one captured log call.
type lpLine struct {
	msg string
	// isError is true for logger.Error calls.
	isError bool
	// err is the error passed to logger.Error (nil for Info, or when parsed from text).
	err error
	// errText is the rendered error: err.Error(), or the "error" field of a zap line.
	errText string
	// keys are the log keys in the order they were logged, values included.
	keys []string
	// kv maps each key to its value. A key logged twice keeps the last value.
	kv map[string]any
}

// str renders a value the same way for typed sink values and JSON-decoded zap values:
// int32(5) and float64(5) both become "5".
func (l lpLine) str(key string) string {
	v, ok := l.kv[key]
	if !ok {
		return "<missing>"
	}
	return fmt.Sprint(v)
}

func (l lpLine) has(key string) bool {
	_, ok := l.kv[key]
	return ok
}

// lpCapture records log lines from a logr.Logger.
type lpCapture struct {
	mu    sync.Mutex
	lines []lpLine
}

// logger returns a logger that records into c.
func (c *lpCapture) logger() logr.Logger {
	return logr.New(&lpSink{capture: c})
}

func (c *lpCapture) add(l lpLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, l)
}

// all returns a copy of the lines so far.
func (c *lpCapture) all() []lpLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]lpLine(nil), c.lines...)
}

func (c *lpCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = nil
}

// lpSink is a logr.LogSink that keeps every call, including WithValues keys.
type lpSink struct {
	capture *lpCapture
	values  []any
}

func (s *lpSink) Init(logr.RuntimeInfo)        {}
func (s *lpSink) Enabled(int) bool             { return true }
func (s *lpSink) WithName(string) logr.LogSink { return s }
func (s *lpSink) WithValues(kv ...any) logr.LogSink {
	return &lpSink{capture: s.capture, values: append(append([]any(nil), s.values...), kv...)}
}
func (s *lpSink) Info(_ int, msg string, kv ...any) { s.emit(msg, nil, false, kv) }
func (s *lpSink) Error(err error, msg string, kv ...any) {
	s.emit(msg, err, true, kv)
}

func (s *lpSink) emit(msg string, err error, isError bool, kv []any) {
	all := append(append([]any(nil), s.values...), kv...)
	line := lpLine{msg: msg, isError: isError, err: err, kv: map[string]any{}}
	if err != nil {
		line.errText = err.Error()
	}
	for i := 0; i+1 < len(all); i += 2 {
		key := fmt.Sprint(all[i])
		line.keys = append(line.keys, key)
		line.kv[key] = all[i+1]
	}
	if len(all)%2 == 1 {
		// An odd key list is a logging bug; make it visible to the assertions.
		line.keys = append(line.keys, "<odd>")
		line.kv["<odd>"] = all[len(all)-1]
	}
	s.capture.add(line)
}

// lpWriter is an io.Writer that keeps everything written to it, for teeing the suite's
// zap logger (GinkgoWriter) and for rendering lines through a production-configured zap.
type lpWriter struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (w *lpWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lpWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *lpWriter) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Reset()
}

// lpParseZapConsole parses the lines the suite's development zap logger writes:
// "<time>\t<LEVEL>\t[<logger>\t]<msg>[\t<json fields>]". Stack trace lines that follow
// an error are skipped.
func lpParseZapConsole(text string) []lpLine {
	var out []lpLine
	for _, raw := range strings.Split(text, "\n") {
		fields := strings.Split(raw, "\t")
		if len(fields) < 3 {
			continue
		}
		level := fields[1]
		if level != "INFO" && level != "ERROR" && level != "DEBUG" && level != "WARN" {
			continue
		}
		line := lpLine{isError: level == "ERROR", kv: map[string]any{}}
		last := fields[len(fields)-1]
		if strings.HasPrefix(last, "{") && len(fields) >= 4 {
			line.msg = fields[len(fields)-2]
			dec := json.NewDecoder(strings.NewReader(last))
			// Keep the key order as written: walk the tokens of the top-level object.
			if tok, err := dec.Token(); err == nil && tok == json.Delim('{') {
				for dec.More() {
					keyTok, err := dec.Token()
					if err != nil {
						break
					}
					key, _ := keyTok.(string)
					var value any
					if err := dec.Decode(&value); err != nil {
						break
					}
					line.keys = append(line.keys, key)
					line.kv[key] = value
				}
			}
			if e, ok := line.kv["error"]; ok {
				line.errText = fmt.Sprint(e)
			}
		} else {
			line.msg = last
		}
		out = append(out, line)
	}
	return out
}

// lpRetryLines keeps the lines of the shared APIM write handling for one kind: those
// that carry kind=<kind>. Other lines of the reconcile (🧩, 🔗, 🔐 ...) are dropped.
func lpRetryLines(lines []lpLine, kind string) []lpLine {
	var out []lpLine
	for _, l := range lines {
		if fmt.Sprint(l.kv["kind"]) == kind {
			out = append(out, l)
		}
	}
	return out
}

// lpMsgs lists the messages of lines.
func lpMsgs(lines []lpLine) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.msg)
	}
	return out
}

// lpWithMsg keeps the lines whose message is msg.
func lpWithMsg(lines []lpLine, msg string) []lpLine {
	var out []lpLine
	for _, l := range lines {
		if l.msg == msg {
			out = append(out, l)
		}
	}
	return out
}

// lpClock is a settable, goroutine-safe clock for the retry policy.
type lpClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lpClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lpClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *lpClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
