// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset    = "\x1b[0m"
	ansiRed      = "\x1b[31m"
	ansiYellow   = "\x1b[33m"
	ansiCyan     = "\x1b[36m"
	ansiDimCyan  = "\x1b[2;36m"
	ansiDimWhite = "\x1b[2;37m"
)

// consoleStart is the zero of the elapsed-seconds stamp used when full timestamps are off.
var consoleStart = time.Now()

// console is the state shared by every consoleHandler derived from one another.
type console struct {
	mu           sync.Mutex
	out          io.Writer
	terminal     bool
	opts         TextOptions
	reportCaller bool
	level        slog.LevelVar
}

func newConsole(out io.Writer) *console {
	c := &console{}
	c.setOutput(out)
	return c
}

func (c *console) setOutput(w io.Writer) {
	terminal := checkIfTerminal(w)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.out, c.terminal = w, terminal
}

// consoleHandler is a slog.Handler writing the text format the runner has always used, so that
// moving off logrus changes nothing a user can see: sorted key=value fields on a plain stream,
// and a colored prefix with elapsed or full time on a terminal.
type consoleHandler struct {
	c     *console
	attrs []slog.Attr
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.c.level.Level()
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &consoleHandler{c: h.c, attrs: append(slices.Clone(h.attrs), attrs...)}
}

// WithGroup is a no-op, as only NewSlogHandler wraps a consoleHandler and it never groups.
func (h *consoleHandler) WithGroup(string) slog.Handler { return h }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	c := h.c
	c.mu.Lock()
	opts, reportCaller, terminal := c.opts, c.reportCaller, c.terminal
	c.mu.Unlock()

	data := make(map[string]any, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		data[a.Key] = a.Value.Resolve().Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		data[a.Key] = a.Value.Resolve().Any()
		return true
	})

	var caller *runtime.Frame
	if reportCaller && r.PC != 0 {
		frame, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
		caller = &frame
	}
	prefixFieldClashes(data, caller != nil)

	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b bytes.Buffer
	if terminal && !opts.DisableColors {
		printColored(&b, opts, r, caller, keys, data)
	} else {
		printPlain(&b, opts, r, caller, keys, data)
	}

	line := b.Bytes()
	if opts.Filter != nil {
		line = opts.Filter(line)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.out.Write(line)
	return err
}

// prefixFieldClashes moves fields that share a name with a fixed key out of its way.
func prefixFieldClashes(data map[string]any, reportCaller bool) {
	for _, key := range []string{"time", "level", "msg"} {
		if v, ok := data[key]; ok {
			data["fields."+key] = v
			delete(data, key)
		}
	}
	if reportCaller {
		for _, key := range []string{"func", "file"} {
			if v, ok := data[key]; ok {
				data["fields."+key] = v
			}
		}
	}
}

func printPlain(b *bytes.Buffer, opts TextOptions, r slog.Record, caller *runtime.Frame, keys []string, data map[string]any) {
	if !opts.DisableTimestamp {
		appendKeyValue(b, "time", r.Time.Format(time.RFC3339))
	}
	appendKeyValue(b, "level", plainLevel(Level(r.Level)))
	if r.Message != "" {
		appendKeyValue(b, "msg", r.Message)
	}
	if caller != nil {
		funcVal, fileVal := callerText(opts, caller)
		if funcVal != "" {
			appendKeyValue(b, "func", funcVal)
		}
		if fileVal != "" {
			appendKeyValue(b, "file", fileVal)
		}
	}
	for _, k := range keys {
		appendKeyValue(b, k, data[k])
	}
	b.WriteByte('\n')
}

func callerText(opts TextOptions, caller *runtime.Frame) (funcVal, fileVal string) {
	if opts.CallerPrettyfier != nil {
		return opts.CallerPrettyfier(caller)
	}
	return caller.Function, caller.File + ":" + strconv.Itoa(caller.Line)
}

func printColored(b *bytes.Buffer, opts TextOptions, r slog.Record, caller *runtime.Frame, keys []string, data map[string]any) {
	msg := strings.TrimSuffix(r.Message, "\n")

	var callerStr string
	if caller != nil {
		funcVal, fileVal := callerText(opts, caller)
		if opts.CallerPrettyfier == nil && caller.Function != "" {
			funcVal += "()"
		}
		switch {
		case fileVal == "":
			callerStr = funcVal
		case funcVal == "":
			callerStr = fileVal
		default:
			callerStr = fileVal + " " + funcVal
		}
	}

	level := Level(r.Level)
	prefix := colorize(level, strings.ToUpper(plainLevel(level))[:4])
	switch {
	case opts.DisableTimestamp:
		fmt.Fprintf(b, "%s%s %-44s ", prefix, callerStr, msg)
	case !opts.FullTimestamp:
		fmt.Fprintf(b, "%s[%04d]%s %-44s ", prefix, int(r.Time.Sub(consoleStart)/time.Second), callerStr, msg)
	default:
		fmt.Fprintf(b, "%s[%s]%s %-44s ", prefix, r.Time.Format(time.RFC3339), callerStr, msg)
	}
	for _, k := range keys {
		b.WriteByte(' ')
		b.WriteString(colorize(level, k))
		b.WriteByte('=')
		appendValue(b, data[k])
	}
	b.WriteByte('\n')
}

// plainLevel spells Warn the way logrus did, as "warning".
func plainLevel(l Level) string {
	if s := l.String(); s != "warn" {
		return s
	}
	return "warning"
}

func colorize(l Level, s string) string {
	var color string
	switch {
	case l <= LevelTrace:
		color = ansiDimWhite
	case l <= LevelDebug:
		color = ansiDimCyan
	case l <= LevelInfo:
		color = ansiCyan
	case l <= LevelWarn:
		color = ansiYellow
	default:
		color = ansiRed
	}
	return color + s + ansiReset
}

func appendKeyValue(b *bytes.Buffer, key string, value any) {
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(key)
	b.WriteByte('=')
	appendValue(b, value)
}

func appendValue(b *bytes.Buffer, value any) {
	switch v := value.(type) {
	case string:
		appendString(b, v)
	case []byte:
		appendString(b, string(v))
	case error:
		appendFormatted(b, v, v.Error)
	case fmt.Stringer:
		appendFormatted(b, v, v.String)
	default:
		appendString(b, fmt.Sprint(value))
	}
}

// appendFormatted writes what get returns, and "<nil>" if that panics on a nil receiver.
func appendFormatted(b *bytes.Buffer, v any, get func() string) {
	defer func() {
		if r := recover(); r != nil {
			if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
				appendString(b, "<nil>")
			} else {
				appendString(b, fmt.Sprintf("%%!v(PANIC=%v)", r))
			}
		}
	}()
	appendString(b, get())
}

func appendString(b *bytes.Buffer, s string) {
	if needsQuoting(s) {
		b.WriteString(strconv.Quote(s))
		return
	}
	b.WriteString(s)
}

func needsQuoting(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == '/', c == '@', c == '^', c == '+':
		default:
			return true
		}
	}
	return false
}
