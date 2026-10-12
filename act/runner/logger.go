// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"

	"golang.org/x/term"
)

const (
	// nocolor = 0
	red     = 31
	green   = 32
	yellow  = 33
	blue    = 34
	magenta = 35
	cyan    = 36
	gray    = 37
)

const (
	// The "stage" of the log entries of a step, and of the job level entries that finalize the job after its steps.
	StagePre      = "Pre"
	StageMain     = "Main"
	StagePost     = "Post"
	StageComplete = "Complete"
	// StepStartedField marks the log entry of a step that is about to run, unlike one that is skipped.
	StepStartedField = "stepStarted"
)

const (
	rawOutputField      = "raw_output"
	scriptLineCyanField = "script_line_cyan"
)

var (
	colors    []int
	nextColor int
	mux       sync.Mutex
)

func init() {
	nextColor = 0
	colors = []int{
		blue, yellow, green, magenta, red, gray, cyan,
	}
}

type masksContextKey string

const masksContextKeyVal = masksContextKey("logrus.FieldLogger")

// Logger returns the appropriate logger for current context
func Masks(ctx context.Context) *[]string {
	val := ctx.Value(masksContextKeyVal)
	if val != nil {
		if masks, ok := val.(*[]string); ok {
			return masks
		}
	}
	return &[]string{}
}

// WithMasks adds a value to the context for the logger
func WithMasks(ctx context.Context, masks *[]string) context.Context {
	return context.WithValue(ctx, masksContextKeyVal, masks)
}

// JobLoggerFactory supplies the handler a job's own log output goes to, in place of stdout.
type JobLoggerFactory interface {
	WithJobLogger() logging.Handler
}

type jobLoggerFactoryContextKey string

var jobLoggerFactoryContextKeyVal = jobLoggerFactoryContextKey("jobloggerkey")

func WithJobLoggerFactory(ctx context.Context, factory JobLoggerFactory) context.Context {
	return context.WithValue(ctx, jobLoggerFactoryContextKeyVal, factory)
}

// WithJobLogger attaches a new logger to context that is aware of steps
func WithJobLogger(ctx context.Context, jobID, jobName string, config *Config, masks *[]string, matrix map[string]any) context.Context {
	ctx = WithMasks(ctx, masks)

	var output logging.Handler
	if jobLoggerFactory, ok := ctx.Value(jobLoggerFactoryContextKeyVal).(JobLoggerFactory); ok && jobLoggerFactory != nil {
		output = jobLoggerFactory.WithJobLogger()
	} else if config.JSONLogger {
		output = newJobLogJSONHandler(os.Stdout)
	} else {
		mux.Lock()
		nextColor++
		color := colors[nextColor%len(colors)]
		mux.Unlock()
		output = newJobLogHandler(os.Stdout, color)
	}

	var handlers []logging.Handler
	if h := common.LoggerHandler(ctx); h != nil { // Adapt to Gitea
		handlers = append(handlers, h)
	}
	handlers = append(handlers, &maskedHandler{
		Handler: output,
		masker:  valueMasker(config.InsecureSecrets, config.maskers()),
	})

	level := logging.LevelTrace
	if config.JobLoggerLevel != nil {
		level = *config.JobLoggerLevel
	}

	rtn := logging.New(logging.NewLevelHandler(logging.NewMultiHandler(handlers...), level)).With(
		"job", jobName,
		"jobID", jobID,
		"dryrun", common.Dryrun(ctx),
		"matrix", matrix,
	).WithContext(ctx)

	return common.WithLogger(ctx, rtn)
}

func WithCompositeLogger(ctx context.Context, masks *[]string) context.Context {
	ctx = WithMasks(ctx, masks)
	return common.WithLogger(ctx, common.Logger(ctx).WithContext(ctx))
}

func WithCompositeStepLogger(ctx context.Context, stepID string) context.Context {
	stepIDs := append(slices.Clone(common.StepIDs(ctx)), stepID)
	ctx = common.WithStepIDs(ctx, stepIDs)

	return common.WithLogger(ctx, common.Logger(ctx).With("stepID", stepIDs).WithContext(ctx))
}

func withStepLogger(ctx context.Context, stepNumber int, stepID, stepName, stageName string) context.Context {
	stepIDs := []string{stepID}
	ctx = common.WithStepIDs(ctx, stepIDs)

	rtn := common.Logger(ctx).With(
		"stepNumber", stepNumber,
		"step", stepName,
		"stepID", stepIDs,
		"stage", stageName,
	)
	return common.WithLogger(ctx, rtn)
}

// entryProcessor rewrites a record before the output handler sees it.
type entryProcessor func(ctx context.Context, r logging.Record) logging.Record

var secretValueEncoders = []func(string) string{
	func(v string) string { return base64.StdEncoding.EncodeToString([]byte(v)) },
	base64ShiftEncoder(1),
	base64ShiftEncoder(2),
	base64InteriorEncoder(0),
	base64InteriorEncoder(1),
	base64InteriorEncoder(2),
	expressionStringEscape,
	jsonStringEscape,
	jsonStringEscapeNoHTML,
	uriDataEscape,
	url.QueryEscape, // the form-encoded twin of uriDataEscape, which spells a space "+"
	url.PathEscape,
	xmlDataEscape,
	trimDoubleQuotes,
}

// base64ShiftEncoder reproduces the 3-byte alignments of `Basic base64("user:token")`, and
// its padded tail only matches a secret that ends the payload.
func base64ShiftEncoder(shift int) func(string) string {
	return func(v string) string {
		value := []byte(v)
		if len(value) > shift {
			value = value[shift:]
		}
		return base64.StdEncoding.EncodeToString(value)
	}
}

const minInteriorBase64Len = 8 // below this a fragment matches unrelated output

// base64InteriorEncoder keeps the aligned middle, so a secret with data after it still matches.
func base64InteriorEncoder(shift int) func(string) string {
	return func(v string) string {
		buf := make([]byte, shift+len(v))
		copy(buf[shift:], v)
		encoded := base64.StdEncoding.EncodeToString(buf)
		if len(encoded) < 8+minInteriorBase64Len {
			return ""
		}
		return encoded[4 : len(encoded)-4]
	}
}

func expressionStringEscape(v string) string {
	return strings.ReplaceAll(v, "'", "''")
}

func uriDataEscape(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

var xmlDataEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlDataEscape(v string) string {
	return xmlDataEscaper.Replace(v)
}

func trimDoubleQuotes(v string) string {
	if len(v) > 8 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		return v[1 : len(v)-1]
	}
	return ""
}

// jsonStringEscape returns v as it appears inside a JSON string, without the quotes,
// which is what `toJSON(secrets)` or any action logging a JSON body produces. Go's encoder
// escapes <, >, & (as act's own toJSON does); the non-HTML variant below covers the runtimes
// that do not. When v has none of those characters both forms are equal and deduplicated.
func jsonStringEscape(v string) string {
	encoded, err := json.Marshal(v, jsontext.EscapeForHTML(true))
	if err != nil {
		return v
	}
	return string(encoded[1 : len(encoded)-1])
}

// jsonStringEscapeNoHTML is jsonStringEscape without HTML escaping, matching the JSON a
// JavaScript (JSON.stringify) or .NET action emits, so a secret containing < > or & is
// masked in that form too.
func jsonStringEscapeNoHTML(v string) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return v
	}
	return string(encoded[1 : len(encoded)-1])
}

// AppendSecretMaskers skips the debug settings, as GitHub does: they arrive as secrets, but
// masking "true" would corrupt unrelated log lines and drop job outputs that say it.
func AppendSecretMaskers(oldnew []string, secrets map[string]string) []string {
	for k, v := range secrets {
		if k != "ACTIONS_STEP_DEBUG" && k != "ACTIONS_RUNNER_DEBUG" {
			oldnew = AppendSecretMasker(oldnew, v)
		}
	}
	return oldnew
}

// AppendSecretMasker registers v and each of its lines, as GitHub does.
func AppendSecretMasker(oldnew []string, v string) []string {
	ret := appendMaskedValue(oldnew, v)
	for l := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == '\r' || r == '\n' }) {
		ret = appendMaskedValue(ret, strings.TrimSpace(l))
	}
	return ret
}

// appendMaskedValue registers one value and every shape it takes on its way into a log.
func appendMaskedValue(ret []string, v string) []string {
	// formatted JSON secrets could otherwise mask {,[,],} everywhere
	if len(strings.TrimSpace(v)) <= 1 || slices.Contains(ret, v) {
		return ret
	}
	ret = append(ret, v, "***")
	// command data reaches the log escaped, so "pass%word" also arrives as "pass%25word"
	if strings.ContainsAny(v, "%\r\n") {
		ret = append(ret, EscapeCommandData(v), "***")
	}
	for _, encode := range secretValueEncoders {
		encoded := encode(v)
		// An encoding that leaves the value unchanged is already masked above.
		if encoded == v || len(encoded) <= 1 || slices.Contains(ret, encoded) {
			continue
		}
		ret = append(ret, encoded, "***")
	}
	return ret
}

// valueMasker applies secrets and ::add-mask:: patterns to every log entry, including
// raw_output (command/stream) lines; there is no bypass by field.
func valueMasker(insecureSecrets bool, oldnew []string) entryProcessor {
	oldnew = slices.Clip(oldnew)
	defReplacer := NewSecretReplacer(oldnew)

	// A ::add-mask:: only ever appends to the job's mask slice, so the replacer built for
	// it stays valid until the slice grows. Cache it, keyed by the slice itself and its
	// length, instead of encoding every secret and mask again for each log line.
	var (
		mu       sync.Mutex
		masksRef *[]string
		pairs    []string
		masked   int
		replacer *strings.Replacer
	)

	return func(ctx context.Context, r logging.Record) logging.Record {
		if insecureSecrets {
			return r
		}

		masks := Masks(ctx)

		if len(*masks) == 0 {
			r.Message = defReplacer.Replace(r.Message)
			return r
		}

		mu.Lock()
		// A composite action logs through the same masker with its own mask slice, so a
		// different slice starts the cache over.
		if masksRef != masks {
			masksRef, pairs, masked, replacer = masks, oldnew, 0, nil
		}
		if replacer == nil || masked != len(*masks) {
			for _, v := range (*masks)[masked:] {
				pairs = AppendSecretMasker(pairs, v)
			}
			masked = len(*masks)
			replacer = NewSecretReplacer(pairs)
		}
		cmasker := replacer
		mu.Unlock()

		r.Message = cmasker.Replace(r.Message)

		return r
	}
}

type maskedHandler struct {
	logging.Handler
	masker entryProcessor
}

func (m *maskedHandler) Handle(ctx context.Context, r logging.Record) error {
	return m.Handler.Handle(ctx, m.masker(ctx, r))
}

func (m *maskedHandler) WithAttrs(attrs []logging.Attr) logging.Handler {
	return &maskedHandler{Handler: m.Handler.WithAttrs(attrs), masker: m.masker}
}

// jobLogHandler writes a job's log to w, one record per line in the shape format gives it.
type jobLogHandler struct {
	w      io.Writer
	mu     *sync.Mutex // shared by every handler derived with WithAttrs, as they share w
	attrs  []logging.Attr
	format func(logging.Record) ([]byte, error)
}

// newJobLogHandler writes readable text.
func newJobLogHandler(w io.Writer, color int) *jobLogHandler {
	return &jobLogHandler{w: w, mu: &sync.Mutex{}, format: textFormatter{w: w, color: color}.format}
}

func (h *jobLogHandler) Enabled(context.Context, logging.Level) bool { return true }

func (h *jobLogHandler) WithAttrs(attrs []logging.Attr) logging.Handler {
	c := *h
	c.attrs = slices.Concat(h.attrs, attrs)
	return &c
}

func (h *jobLogHandler) Handle(_ context.Context, r logging.Record) error {
	r.Attrs = slices.Concat(h.attrs, r.Attrs)
	line, err := h.format(r)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err = h.w.Write(line)
	return err
}

type textFormatter struct {
	w     io.Writer
	color int
}

func (f textFormatter) format(r logging.Record) ([]byte, error) {
	b := &bytes.Buffer{}

	// the web renderer decodes command data, so this local view has to as well
	if _, _, _, ok := tryParseRawActionCommand(r.Message + "\n"); ok {
		r.Message = UnescapeCommandData(r.Message)
	}
	r.Message = strings.TrimSuffix(r.Message, "\n")

	if f.isColored() {
		f.printColored(b, r)
	} else {
		f.print(b, r)
	}

	b.WriteByte('\n')
	return b.Bytes(), nil
}

func attrIsTrue(r logging.Record, key string) bool {
	v, _ := r.Attr(key)
	return v == true
}

func (f textFormatter) printColored(b *bytes.Buffer, r logging.Record) {
	job, _ := r.Attr("job")

	debugFlag := ""
	if r.Level == logging.LevelDebug {
		debugFlag = "[DEBUG] "
	}

	switch {
	case attrIsTrue(r, rawOutputField):
		if attrIsTrue(r, scriptLineCyanField) {
			fmt.Fprintf(b, "\x1b[%dm|\x1b[0m \x1b[36;1m%s\x1b[0m", f.color, r.Message)
		} else {
			fmt.Fprintf(b, "\x1b[%dm|\x1b[0m %s", f.color, r.Message)
		}
	case attrIsTrue(r, "dryrun"):
		fmt.Fprintf(b, "\x1b[1m\x1b[%dm\x1b[7m*DRYRUN*\x1b[0m \x1b[%dm[%s] \x1b[0m%s%s", gray, f.color, job, debugFlag, r.Message)
	default:
		fmt.Fprintf(b, "\x1b[%dm[%s] \x1b[0m%s%s", f.color, job, debugFlag, r.Message)
	}
}

func (f textFormatter) print(b *bytes.Buffer, r logging.Record) {
	job, _ := r.Attr("job")

	debugFlag := ""
	if r.Level == logging.LevelDebug {
		debugFlag = "[DEBUG] "
	}

	switch {
	case attrIsTrue(r, rawOutputField):
		fmt.Fprintf(b, "[%s]   | %s", job, r.Message)
	case attrIsTrue(r, "dryrun"):
		fmt.Fprintf(b, "*DRYRUN* [%s] %s%s", job, debugFlag, r.Message)
	default:
		fmt.Fprintf(b, "[%s] %s%s", job, debugFlag, r.Message)
	}
}

func (f textFormatter) isColored() bool {
	isColored := checkIfTerminal(f.w)

	if force, ok := os.LookupEnv("CLICOLOR_FORCE"); ok && force != "0" {
		isColored = true
	} else if ok && force == "0" {
		isColored = false
	} else if os.Getenv("CLICOLOR") == "0" {
		isColored = false
	}

	return isColored
}

func checkIfTerminal(w io.Writer) bool {
	switch v := w.(type) {
	case *os.File:
		return term.IsTerminal(int(v.Fd()))
	default:
		return false
	}
}

// maskSecrets hides this job's secrets in a value that reaches somewhere the log maskers cannot,
// such as a container name or a job summary. Masks added at runtime count, so a summary written
// after ::add-mask:: is covered too.
func (rc *RunContext) maskSecrets(value string) string {
	oldnew := rc.Config.maskers()
	for _, mask := range rc.Masks {
		oldnew = AppendSecretMasker(oldnew, mask)
	}
	return NewSecretReplacer(oldnew).Replace(value)
}

// maskers is every value this job's configuration says to hide, whatever the sink.
func (c *Config) maskers() []string {
	oldnew := AppendSecretMaskers(nil, c.Secrets)
	for _, mask := range c.ExtraMasks {
		oldnew = AppendSecretMasker(oldnew, mask)
	}
	return oldnew
}

// NewSecretReplacer masks the longest secret first. Replacer matches in argument order, so a
// secret that prefixes another would otherwise mask only that prefix and print the rest.
func NewSecretReplacer(oldnew []string) *strings.Replacer {
	pairs := make([][2]string, 0, len(oldnew)/2)
	for i := 0; i+1 < len(oldnew); i += 2 {
		pairs = append(pairs, [2]string{oldnew[i], oldnew[i+1]})
	}
	slices.SortFunc(pairs, func(a, b [2]string) int { return len(b[0]) - len(a[0]) })

	sorted := make([]string, 0, len(pairs)*2)
	for _, pair := range pairs {
		sorted = append(sorted, pair[0], pair[1])
	}
	return strings.NewReplacer(sorted...)
}
