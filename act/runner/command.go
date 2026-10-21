// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"unicode"

	"gitea.com/gitea/runner/act/common"
)

// parseActionCommand parses like GitHub's runner, but rejects a bare \r GitHub would split at and keeps a value's leading '=' so masks match.
func parseActionCommand(line, resumeCommand string) (command string, properties map[string]string, data string, legacy, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if strings.ContainsAny(line, "\r\n") {
		return "", nil, "", false, false
	}
	trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
	for _, legacy := range []bool{false, true} {
		header, found := strings.CutPrefix(trimmed, "::")
		separator, propertySeparator, unescapeProperty, unescapeData := "::", ",", commandPropertyUnescaper.Replace, UnescapeCommandData
		if legacy {
			_, header, found = strings.Cut(line, "##[")
			separator, propertySeparator, unescapeProperty, unescapeData = "]", ";", UnescapeLegacyCommand, UnescapeLegacyCommand
		}
		if !found {
			continue
		}
		header, data, found := strings.Cut(header, separator)
		if !found {
			continue
		}
		command, propertyList, _ := strings.Cut(header, " ")
		command = strings.ToLower(command)
		switch command {
		case "stop-commands", "set-env", "set-output", "save-state", "add-path", "add-mask", "add-matcher",
			"remove-matcher", "warning", "error", "notice", "debug", "group", "endgroup", "echo":
		default:
			if resumeCommand == "" || !strings.EqualFold(command, resumeCommand) {
				continue
			}
		}
		if !legacy {
			propertyList = strings.TrimSpace(propertyList)
		}
		properties := map[string]string{}
		for property := range strings.SplitSeq(propertyList, propertySeparator) {
			if key, value, _ := strings.Cut(strings.TrimLeft(property, "="), "="); value != "" {
				properties[strings.ToLower(key)] = unescapeProperty(value)
			}
		}
		return command, properties, unescapeData(data), legacy, true
	}
	return "", nil, "", false, false
}

// commandHandler reports whether a line is logged, keeping per-handler command state as GitHub does.
func (rc *RunContext) commandHandler(ctx context.Context) common.LineHandler {
	rawLogger := common.Logger(ctx).With(rawOutputField, true)
	debug := rc.topLevelRunContext().Config.RunnerDebug()
	echo := debug
	resumeCommand := ""
	return func(line string) bool {
		command, kvPairs, arg, _, ok := parseActionCommand(line, resumeCommand)
		if !ok {
			return true
		}
		if resumeCommand != "" {
			// Resumed here rather than from the switch, because the end token is arbitrary
			// and a token naming a real command would otherwise never resume.
			if strings.EqualFold(command, resumeCommand) {
				resumeCommand = ""
			}
			return true
		}
		echoed := echo
		if (command == "set-env" || command == "add-path") && rc.refuseUnsecureCommand(ctx, command) {
			return echoed
		}
		switch command {
		case "set-env":
			rc.setEnv(ctx, kvPairs, arg, true)
		case "set-output":
			rc.setOutput(ctx, kvPairs, arg)
		case "add-path":
			rc.addPath(arg)
		case "save-state":
			rc.saveState(ctx, kvPairs, arg)
		case "echo":
			switch strings.ToLower(strings.TrimSpace(arg)) {
			case "on":
				echo = true
			case "off":
				echo = false
			}
		case "add-mask":
			rc.AddMask(ctx, arg)
			if echoed {
				rawLogger.Infof("::add-mask::***")
			}
			return false
		case "stop-commands":
			resumeCommand = arg
			if len(arg) > 6 { // as on GitHub, so the log does not give away the resume token
				rc.AddMask(ctx, arg)
			}
			return true
		case "debug":
			return debug
		case "notice", "warning", "error":
			rawLogger.Infof("%s", formatAnnotation(command, kvPairs, arg))
			return false
		case "group", "endgroup":
			return true
		}
		return echoed
	}
}

// formatAnnotation folds the location and title into the message, as Gitea's web UI drops command properties.
func formatAnnotation(level string, properties map[string]string, message string) string {
	prefix := properties["file"]
	if prefix != "" {
		if line := properties["line"]; line != "" {
			prefix += ":" + line
			if col := cmp.Or(properties["col"], properties["column"]); col != "" {
				prefix += ":" + col
			}
		}
		prefix += ": "
	}
	if title := properties["title"]; title != "" {
		prefix += title + ": "
	}
	return "::" + level + "::" + EscapeCommandData(prefix+message)
}

// serviceCommandHandler applies only ::add-mask::, so a service cannot set the job's env, outputs or state.
func (rc *RunContext) serviceCommandHandler(ctx context.Context) common.LineHandler {
	return func(line string) bool {
		command, _, arg, _, ok := parseActionCommand(line, "")
		if !ok || command != "add-mask" {
			return true
		}
		rc.AddMask(ctx, arg)
		return false
	}
}

const allowUnsecureCommandsVar = "ACTIONS_ALLOW_UNSECURE_COMMANDS"

// refuseUnsecureCommand reports whether a deprecated ::set-env:: or ::add-path:: command must
// not run, recording the error that fails the step. GitHub disabled both because a step that
// echoes untrusted content can use them to set NODE_OPTIONS or PATH for every later step.
func (rc *RunContext) refuseUnsecureCommand(ctx context.Context, command string) bool {
	if rc.allowUnsecureCommandsOptIn() {
		return false
	}

	// The step executor logs the failure itself, so keep this line's wording distinct.
	common.Logger(ctx).With(rawOutputField, true).Errorf("##[error]%s", EscapeCommandData(fmt.Sprintf(
		"The `%s` command is disabled: it can set the environment of every later step from untrusted output. "+
			"Write to $GITHUB_ENV or $GITHUB_PATH instead, or set ACTIONS_ALLOW_UNSECURE_COMMANDS to allow it",
		command)))

	rc.unsecureCommandMu.Lock()
	defer rc.unsecureCommandMu.Unlock()
	if rc.unsecureCommandErr == nil {
		rc.unsecureCommandErr = fmt.Errorf("the `%s` workflow command is disabled", command)
	}
	return true
}

// allowUnsecureCommandsOptIn reports whether the workflow itself asked for the deprecated
// commands, from any env scope, as it can on GitHub.
func (rc *RunContext) allowUnsecureCommandsOptIn() bool {
	return isTruthyEnv(rc.currentStepEnv()[allowUnsecureCommandsVar]) ||
		isTruthyEnv(rc.Env[allowUnsecureCommandsVar]) ||
		isTruthyEnv(rc.GlobalEnv[allowUnsecureCommandsVar])
}

// isTruthyEnv mirrors GitHub's bool.TryParse: only "true", in any casing.
func isTruthyEnv(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "true")
}

// takeUnsecureCommandError returns and clears the error left by a refused command.
func (rc *RunContext) takeUnsecureCommandError() error {
	rc.unsecureCommandMu.Lock()
	defer rc.unsecureCommandMu.Unlock()
	err := rc.unsecureCommandErr
	rc.unsecureCommandErr = nil
	return err
}

func (rc *RunContext) setEnv(ctx context.Context, kvPairs map[string]string, arg string, fromCommand bool) {
	name := kvPairs["name"]
	if strings.EqualFold(name, "NODE_OPTIONS") {
		message := "Can't store NODE_OPTIONS output parameter using '$GITHUB_ENV' command."
		if fromCommand {
			message = "Can't update NODE_OPTIONS environment variable using ::set-env:: command."
		}
		common.Logger(ctx).With(rawOutputField, true).Errorf("##[error]%s", EscapeCommandData(message))
		return
	}
	if rc.Env == nil {
		rc.Env = make(map[string]string)
	}
	if rc.GlobalEnv == nil {
		rc.GlobalEnv = map[string]string{}
	}
	newenv := map[string]string{
		name: arg,
	}
	mergeIntoMap := mergeIntoMapCaseSensitive
	if rc.JobContainer != nil && rc.JobContainer.IsEnvironmentCaseInsensitive() {
		mergeIntoMap = mergeIntoMapCaseInsensitive
	}
	mergeIntoMap(rc.Env, newenv)
	mergeIntoMap(rc.GlobalEnv, newenv)
}

func (rc *RunContext) setEnvFile(ctx context.Context, kvPairs map[string]string, arg string) {
	rc.setEnv(ctx, kvPairs, arg, false)
}

func (rc *RunContext) setOutput(ctx context.Context, kvPairs map[string]string, arg string) {
	logger := common.Logger(ctx)
	stepID := rc.CurrentStep
	outputName := kvPairs["name"]

	result, ok := rc.StepResults[stepID]
	if !ok {
		logger.Infof("No outputs registered for step '%s'", stepID)
		return
	}

	result.Outputs[outputName] = arg
}

func (rc *RunContext) addPath(arg string) {
	extraPath := []string{arg}
	for _, v := range rc.ExtraPath {
		if v != arg {
			extraPath = append(extraPath, v)
		}
	}
	rc.ExtraPath = extraPath
}

// A Replacer never rescans what it wrote, so "%250A" stays a literal "%0A".
var (
	commandDataEscaper       = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	commandDataUnescaper     = strings.NewReplacer("%25", "%", "%0D", "\r", "%0A", "\n")
	commandPropertyUnescaper = strings.NewReplacer("%25", "%", "%0D", "\r", "%0A", "\n", "%3A", ":", "%2C", ",")
	legacyCommandUnescaper   = strings.NewReplacer("%3B", ";", "%0D", "\r", "%0A", "\n", "%5D", "]", "%25", "%")
)

// EscapeCommandData encodes the data part of a "::cmd::" or "##[cmd]" line the runner writes itself,
// so the log renderer decodes it back. Lines forwarded from step output are already escaped.
func EscapeCommandData(arg string) string {
	return commandDataEscaper.Replace(arg)
}

func UnescapeCommandData(arg string) string {
	return commandDataUnescaper.Replace(arg)
}

// UnescapeLegacyCommand decodes a "##[cmd]" line, which also spells ";" and "]" escaped.
func UnescapeLegacyCommand(arg string) string {
	return legacyCommandUnescaper.Replace(arg)
}

func (rc *RunContext) saveState(_ context.Context, kvPairs map[string]string, arg string) {
	stepID := rc.CurrentStep
	if stepID != "" {
		if rc.IntraActionState == nil {
			rc.IntraActionState = map[string]map[string]string{}
		}
		state, ok := rc.IntraActionState[stepID]
		if !ok {
			state = map[string]string{}
			rc.IntraActionState[stepID] = state
		}
		state[kvPairs["name"]] = arg
	}
}
