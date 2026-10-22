// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseActionCommand(t *testing.T) {
	for _, test := range []struct {
		name, line, resumeCommand, command string
		properties                         map[string]string
		data                               string
		ok                                 bool
	}{
		{"plain", "plain output", "", "", nil, "", false},
		{"unknown v2", "::unknown::v", "", "", nil, "", false},
		{"missing v2 terminator", "::set-output name=x", "", "", nil, "", false},
		{"empty command", "::::", "", "", nil, "", false},
		{"text before v2", "debug ::set-output name=x::v", "", "", nil, "", false},
		{"missing legacy terminator", "##[debug", "", "", nil, "", false},
		{"tab is not a name separator", "::set-output\tname=x::v", "", "", nil, "", false},
		{"v2 leading unicode whitespace and line endings", " \t\u2003::set-output name=tab::v\r\n\r\n", "", "set-output", map[string]string{"name": "tab"}, "v", true},
		{"legacy prefix and casing", "prefix ##[SET-OUTPUT NAME=mid;]v\r\n", "", "set-output", map[string]string{"name": "mid"}, "v", true},
		{"only first legacy prefix", "##[unknown]##[set-output name=x]v", "", "", nil, "", false},
		{"v2 delimiter boundaries", "::set-output name=a:b,other=c:d:: a::b ", "", "set-output", map[string]string{"name": "a:b", "other": "c:d"}, " a::b ", true},
		{"legacy first closing bracket", "##[set-output name=x]v]tail", "", "set-output", map[string]string{"name": "x"}, "v]tail", true},
		{"v2 semicolons are literal", "::set-output name=x;y=z::v", "", "set-output", map[string]string{"name": "x;y=z"}, "v", true},
		{"legacy commas are literal", "##[set-output name=x,y=z]v", "", "set-output", map[string]string{"name": "x,y=z"}, "v", true},
		{"values keep their leading equals", "::set-output name==x=y,=key=v,empty=,=x,spaced= =x::v", "", "set-output", map[string]string{"name": "=x=y", "key": "v", "spaced": " =x"}, "v", true},
		{"v2 trims whole list only", "::set-output  \tfile=x, line=2,name = value \t::v", "", "set-output", map[string]string{"file": "x", " line": "2", "name ": " value"}, "v", true},
		{"legacy preserves list whitespace", "##[set-output  name=q ]v", "", "set-output", map[string]string{" name": "q "}, "v", true},
		{"case insensitive duplicate keys", "::SET-OUTPUT NAME=first,Name=last::v", "", "set-output", map[string]string{"name": "last"}, "v", true},
		{"keys stay escaped", "::set-output na%3Ame=x::v", "", "set-output", map[string]string{"na%3ame": "x"}, "v", true},
		{"v2 decode table", "::set-output name=%0D%0A%3A%2C%25%253A%252C%250A%2525%3a%2c%0d%0a%3B%5D::%0D%0A%25%3A%2C%3B%5D%250A", "", "set-output", map[string]string{"name": "\r\n:,%%3A%2C%0A%25%3a%2c%0d%0a%3B%5D"}, "\r\n%%3A%2C%3B%5D%0A", true},
		{"legacy decode table", "##[set-output name=%3B%0D%0A%5D%25%253B%255D%250A%2525%3b%5d%0d%0a%3A%2C]v%5Dw%3Bx", "", "set-output", map[string]string{"name": ";\r\n]%%3B%5D%0A%25%3b%5d%0d%0a%3A%2C"}, "v]w;x", true},
		{"unknown v2 falls back to legacy", "::unknown::prefix ##[set-output name=x]v", "", "set-output", map[string]string{"name": "x"}, "v", true},
		{"unterminated v2 falls back to legacy", "::debug ##[set-output name=x]v", "", "set-output", map[string]string{"name": "x"}, "v", true},
		{"v2 wins over legacy", "::debug::##[set-output name=x]v", "", "debug", map[string]string{}, "##[set-output name=x]v", true},
		{"internal line breaks are not commands", "::debug::hidden\rplain\r\n", "", "", nil, "", false},
		{"v2 resume token", "::ReSuMe-ToKeN::", "RESUME-TOKEN", "resume-token", map[string]string{}, "", true},
		{"legacy resume token", "prefix ##[RESUME-TOKEN]v", "resume-token", "resume-token", map[string]string{}, "v", true},
		{"different resume token", "::other-token::", "resume-token", "", nil, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			command, properties, data, ok := parseActionCommand(test.line, test.resumeCommand)
			assert.Equal(t, test.command, command)
			assert.Equal(t, test.properties, properties)
			assert.Equal(t, test.data, data)
			assert.Equal(t, test.ok, ok)
		})
	}
}

// unsecureRC opts into ::set-env:: and ::add-path::, which are refused without it.
func unsecureRC() *RunContext {
	return &RunContext{Config: &Config{}, Env: map[string]string{allowUnsecureCommandsVar: "true"}}
}

func TestSetEnv(t *testing.T) {
	a := assert.New(t)
	logger, hook := logtest.NewLogger()
	ctx := common.WithLogger(t.Context(), logger)
	rc := unsecureRC()
	handler := rc.commandHandler(ctx)

	handler("::set-env name=x::valz\n")
	a.Equal("valz", rc.Env["x"])
	handler("::SET-ENV name=NODE_OPTIONS::--require command.js\n")
	rc.setEnvFile(ctx, map[string]string{"name": "node_options"}, "--require env.js")
	a.NotContains(rc.Env, "NODE_OPTIONS")
	a.NotContains(rc.Env, "node_options")
	entries := hook.AllRecords()
	require.Len(t, entries, 2)
	a.Equal("##[error]Can't update NODE_OPTIONS environment variable using ::set-env:: command.", entries[0].Message)
	a.Equal("##[error]Can't store NODE_OPTIONS output parameter using '$GITHUB_ENV' command.", entries[1].Message)
}

func TestSetOutput(t *testing.T) {
	rc := &RunContext{Config: &Config{}, CurrentStep: "my-step", StepResults: map[string]*model.StepResult{"my-step": {Outputs: map[string]string{}}}}
	handler := rc.commandHandler(t.Context())

	handler("::set-output NAME=modern%3A::v%0D%0A%25%3B%5D%250A\n")
	handler("##[set-output name=legacy%3B%5D]v%0D%0A%25%3B%5D%250A\n")
	assert.Equal(t, map[string]string{"modern:": "v\r\n%%3B%5D%0A", "legacy;]": "v\r\n%;]%0A"}, rc.StepResults["my-step"].Outputs)
}

func TestAddpath(t *testing.T) {
	for _, prefix := range []string{"::add-path::", "##[add-path]"} {
		t.Run(prefix, func(t *testing.T) {
			rc := unsecureRC()
			handler := rc.commandHandler(t.Context())
			handler(prefix + "/zoo\n")
			handler(prefix + "/boo\n")
			assert.Equal(t, []string{"/boo", "/zoo"}, rc.ExtraPath)
			handler(prefix + "/zoo\n")
			assert.Equal(t, []string{"/zoo", "/boo"}, rc.ExtraPath)
		})
	}
}

func TestSaveState(t *testing.T) {
	rc := &RunContext{
		Config:      &Config{},
		CurrentStep: "step",
		StepResults: map[string]*model.StepResult{},
	}

	ctx := t.Context()

	handler := rc.commandHandler(ctx)
	handler("::save-state name=state-name::state-value\n")

	assert.Equal(t, "state-value", rc.IntraActionState["step"]["state-name"])
}

func TestEscapeCommandData(t *testing.T) {
	a := assert.New(t)

	a.Equal("a%25b%0Dc%0Ad%250A", EscapeCommandData("a%b\rc\nd%0A"))
	a.Equal("a%b\rc\nd%0A", UnescapeCommandData("a%25b%0Dc%0Ad%250A"))
}

func TestUnsecureCommands(t *testing.T) {
	tests := []struct {
		name    string
		jobEnv  map[string]string
		stepEnv map[string]string
		optedIn bool
	}{
		{name: "refused with no opt-in"},
		// GitHub reads the opt-in with bool.TryParse, so "1" is not one.
		{name: "refused for a value bool.TryParse rejects", jobEnv: map[string]string{allowUnsecureCommandsVar: "1"}},
		{name: "opted in through the step environment", stepEnv: map[string]string{allowUnsecureCommandsVar: "true"}, optedIn: true},
		{name: "opted in through the job environment", jobEnv: map[string]string{allowUnsecureCommandsVar: "TRUE"}, optedIn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := assert.New(t)
			rc := &RunContext{Config: &Config{}, Env: tt.jobEnv}
			rc.setCurrentStepEnv(tt.stepEnv)
			handler := rc.commandHandler(t.Context())

			a.False(handler("::set-env name=x::valz\n"))
			a.False(handler("::add-path::/opt/bin\n"))

			if !tt.optedIn {
				a.Empty(rc.Env["x"])
				a.Empty(rc.ExtraPath)
				// The refusal fails the step that produced it, once.
				require.ErrorContains(t, rc.takeUnsecureCommandError(), "set-env")
				a.NoError(rc.takeUnsecureCommandError())
				return
			}
			a.Equal("valz", rc.Env["x"])
			a.Equal([]string{"/opt/bin"}, rc.ExtraPath)
			a.NoError(rc.takeUnsecureCommandError())
		})
	}
}

type maskRecorder struct {
	logging.Handler
	masks []string
}

func (m *maskRecorder) AddMask(mask string) { m.masks = append(m.masks, mask) }

func TestCommandLogWriter(t *testing.T) {
	for _, tt := range []struct {
		name  string
		debug bool
		steps [][]string
		log   []string
		masks []string
	}{
		{
			name: "applied commands are hidden unless echoed",
			steps: [][]string{{
				"::set-env name=x::y", "::add-path::/bin", "::set-output name=a::b", "##[save-state name=a]b", "::add-matcher::m.json", "::remove-matcher owner=m::",
				"::echo::On%0A",
				"::set-env name=x::y", "::add-path::/bin", "::set-output name=a::b", "##[save-state name=a]b", "::add-matcher::m.json", "::remove-matcher owner=m::",
				"::ADD-MASK::my::secret", "::echo::bogus", "::echo::off", "::add-mask ::empty-properties", "::unknown::v", "plain",
			}},
			log: []string{
				"::set-env name=x::y", "::add-path::/bin", "::set-output name=a::b", "##[save-state name=a]b", "::add-matcher::m.json", "::remove-matcher owner=m::",
				"::add-mask::***", "::echo::bogus", "::echo::off", "::unknown::v", "plain",
			},
			masks: []string{"my::secret", "empty-properties"},
		},
		{
			name:  "step debug shows debug lines and echoes commands until echo is off",
			debug: true,
			steps: [][]string{{"::debug::first%0Asecond", "::set-output name=a::b", "::echo::off", "::debug::again", "::set-output name=a::b"}},
			log:   []string{"::debug::first%0Asecond", "::set-output name=a::b", "::echo::off", "::debug::again"},
		},
		{
			name:  "debug lines are hidden without step debug and an invalid echo changes nothing",
			steps: [][]string{{"::echo::bogus", "::set-output name=a::b", "::echo::on", "::debug::d"}},
		},
		{
			name: "annotations fold their location once whether echoed or not",
			steps: [][]string{{
				"::echo::on",
				"::error file=main.go,line=12,col=5,title=vet::undefined: x",
				"::warning File=a%3Ab.go,Line=1,column=2,title=100%252C::50%25%0Anext",
				"::warning file=x,line=1,col=2,column=3::m",
				"::notice line=9,col=4,title=title::m",
				"::error file=x,col=2::m",
				"::error file=x,line=1::m",
				"::notice::std::vector",
				"##[error file=x%3By]m%5D",
			}},
			log: []string{
				"::error::main.go:12:5: vet: undefined: x", "::warning::a:b.go:1:2: 100%252C: 50%25%0Anext", "::warning::x:1:2: m",
				"::notice::title: m", "::error::x: m", "::error::x:1: m", "::notice::std::vector", "::error::x;y: m]",
			},
		},
		{
			name:  "stop-commands keeps lines verbatim until resumed",
			steps: [][]string{{"::stop-commands::resume-token", "::add-mask::kept", "::set-output name=a::b", "::RESUME-TOKEN::", "::add-mask::masked"}},
			log:   []string{"::stop-commands::resume-token", "::add-mask::kept", "::set-output name=a::b", "::RESUME-TOKEN::"},
			masks: []string{"resume-token", "masked"},
		},
		{
			name:  "a short stop token naming a command stays unmasked and resumes in any case",
			steps: [][]string{{"::stop-commands::NOTICE", "::notice::", "::add-mask::masked"}},
			log:   []string{"::stop-commands::NOTICE", "::notice::"},
			masks: []string{"masked"},
		},
		{
			name:  "state does not carry over to the next step",
			steps: [][]string{{"::echo::on", "::stop-commands::token77"}, {"::set-output name=a::b", "::add-mask::masked"}},
			log:   []string{"::stop-commands::token77"},
			masks: []string{"token77", "masked"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logger, hook := logtest.NewLogger()
			recorder := &maskRecorder{}
			ctx := common.WithLoggerHandler(common.WithLogger(t.Context(), logger), recorder)
			rc := &RunContext{
				Config:      &Config{},
				Parent:      &RunContext{Config: &Config{Secrets: map[string]string{"ACTIONS_STEP_DEBUG": strconv.FormatBool(tt.debug)}}},
				Env:         map[string]string{allowUnsecureCommandsVar: "true"},
				CurrentStep: "step",
				StepResults: map[string]*model.StepResult{"step": {Outputs: map[string]string{}}},
			}
			for _, lines := range tt.steps {
				writer := rc.commandLogWriter(ctx)
				for _, line := range lines {
					_, err := io.WriteString(writer, line+"\n")
					require.NoError(t, err)
				}
			}

			var log []string
			for _, record := range hook.AllRecords() {
				log = append(log, strings.TrimSuffix(record.Message, "\n"))
			}
			assert.Equal(t, tt.log, log)
			assert.Equal(t, tt.masks, recorder.masks)
			assert.Equal(t, tt.masks, rc.Masks)
		})
	}
}
