// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/container"

	_ "embed"

	"gitea.dev/actionslib/pkg/model"
	"github.com/kballard/go-shellquote"
)

type actionStep interface {
	step

	getActionModel() *model.Action
	getCompositeRunContext(context.Context) (*RunContext, error)
	getCompositeSteps() *compositeSteps
}

type readAction func(ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error)

type actionYamlReader func(filename string) (io.Reader, io.Closer, error)

type fileWriter func(filename string, data []byte, perm fs.FileMode) error

type runAction func(step actionStep, actionDir string, remoteAction *remoteAction) common.Executor

//go:embed res/trampoline.js
var trampoline string

var (
	ContainerImageExistsLocally     = container.ImageExistsLocally
	ContainerNewDockerBuildExecutor = container.NewDockerBuildExecutor
	dockerActionNamePattern         = regexp.MustCompile("[^a-zA-Z0-9]")
	inputEnvPattern                 = regexp.MustCompile("[^A-Z0-9-]")
)

func readActionImpl(ctx context.Context, step *model.Step, actionDir, actionPath string, readFile actionYamlReader, writeFile fileWriter) (*model.Action, error) {
	logger := common.Logger(ctx)
	allErrors := []error{}
	addError := func(fileName string, err error) {
		if err != nil {
			allErrors = append(allErrors, fmt.Errorf("failed to read '%s' from action '%s' with path '%s' of step %w", fileName, step.String(), actionPath, err))
		} else {
			// One successful read, clear error state
			allErrors = nil
		}
	}
	reader, closer, err := readFile("action.yml")
	addError("action.yml", err)
	if os.IsNotExist(err) {
		reader, closer, err = readFile("action.yaml")
		addError("action.yaml", err)
		if os.IsNotExist(err) {
			dockerfile := "Dockerfile"
			_, closer, err := readFile(dockerfile)
			addError(dockerfile, err)
			if os.IsNotExist(err) {
				dockerfile = "dockerfile"
				_, closer, err = readFile(dockerfile)
				addError(dockerfile, err)
			}
			if err == nil {
				closer.Close()
				action := &model.Action{
					Name: "(Synthetic)",
					Runs: model.ActionRuns{
						Using: "docker",
						Image: dockerfile,
					},
				}
				logger.Debugf("Using synthetic action %v for %s", action, dockerfile)
				return action, nil
			}
			if val, ok := step.With["args"]; ok {
				if err := writeFile(filepath.Join(actionDir, actionPath, "trampoline.js"), []byte(trampoline), 0o644); err != nil {
					return nil, err
				}
				action := &model.Action{
					Name: "(Synthetic)",
					Inputs: map[string]model.Input{
						"args": {},
						"cwd": {
							Description: "(Actual working directory)",
							Required:    false,
							Default:     filepath.Join(actionDir, actionPath),
						},
						"command": {
							Description: "(Actual program)",
							Required:    false,
							Default:     val,
						},
					},
					Runs: model.ActionRuns{
						Using: "node12",
						Main:  "trampoline.js",
					},
				}
				logger.Debugf("Using synthetic action %v", action)
				return action, nil
			}
		}
	}
	if allErrors != nil {
		return nil, errors.Join(allErrors...)
	}
	defer closer.Close()

	return model.ReadAction(reader)
}

func maybeCopyToActionDir(ctx context.Context, step actionStep, actionDir, actionPath, containerActionDir string) error {
	logger := common.Logger(ctx)
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	if stepModel.Type() != model.StepTypeUsesActionRemote {
		return nil
	}

	containerActionDirCopy := strings.TrimSuffix(containerActionDir, actionPath)
	logger.Debug(containerActionDirCopy)

	if !strings.HasSuffix(containerActionDirCopy, `/`) {
		containerActionDirCopy += `/`
	}

	unlock, err := git.AcquireCloneLock(ctx, actionDir)
	if err != nil {
		return err
	}
	defer unlock()

	if err := rc.JobContainer.CopyDir(containerActionDirCopy, actionDir+"/", false, true)(ctx); err != nil { // an exported commit has no .git to apply gitignore with
		return err
	}
	if rc.Config.NoActionPatch {
		return nil
	}
	patched := patchedScripts(ctx, actionDir, actionScriptPaths(actionDir, filepath.Join(actionDir, actionPath), step.getActionModel()))
	if len(patched) == 0 {
		return nil
	}
	return rc.JobContainer.Copy(containerActionDirCopy, patched...)(ctx)
}

func runActionImpl(step actionStep, actionDir string, remoteAction *remoteAction) common.Executor {
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		actionPath := ""
		if remoteAction != nil {
			actionPath = remoteAction.Path
		}

		action := step.getActionModel()

		rc.withGithubEnv(ctx, getStepGithubContext(ctx, step), *step.getEnv())
		populateEnvsFromSavedState(step.getEnv(), step, rc)
		if err := populateEnvsFromInput(ctx, step, action); err != nil {
			return err
		}
		warnUnexpectedInputs(ctx, action, stepModel.With)
		printRunActionHeader(ctx, stepModel, action, *step.getEnv(), rc)

		actionName, containerActionDir := getContainerActionPaths(step)

		logger.Debugf("type=%v actionDir=%s actionPath=%s workdir=%s actionCacheDir=%s actionName=%s containerActionDir=%s", stepModel.Type(), actionDir, actionPath, rc.Config.Workdir, rc.ActionCacheDir(), actionName, containerActionDir)

		x := action.Runs.Using
		switch {
		case x.IsNode():
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}
			containerArgs := nodeActionCommand(path.Join(containerActionDir, action.Runs.Main))
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.JobContainer.Exec(containerArgs, *step.getEnv(), "", "")(ctx)
		case x.IsDocker():
			location := path.Join(actionDir, actionPath)
			if remoteAction == nil {
				location = containerActionDir
			}
			return execAsDocker(ctx, step, actionName, actionDir, location, remoteAction == nil, stepStageMain)
		case x.IsComposite():
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			return execAsComposite(step)(ctx)
		case x == model.ActionRunsUsingGo:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			rc.ApplyExtraPath(ctx, step.getEnv())

			execFileName := action.Runs.Main + ".out"
			return common.NewPipelineExecutor(
				rc.JobContainer.Exec([]string{"go", "build", "-o", execFileName, action.Runs.Main}, *step.getEnv(), "", containerActionDir),
				rc.JobContainer.Exec([]string{filepath.Join(containerActionDir, execFileName)}, *step.getEnv(), "", ""),
			)(ctx)
		default:
			return fmt.Errorf("the runs.using key must be one of: %v, got %s", []string{
				model.ActionRunsUsingDocker,
				model.ActionRunsUsingNode12,
				model.ActionRunsUsingNode16,
				model.ActionRunsUsingNode20,
				model.ActionRunsUsingNode24,
				model.ActionRunsUsingComposite,
				model.ActionRunsUsingGo,
			}, action.Runs.Using)
		}
	}
}

// /var/run is a symlink, so without the flag node's import.meta.url differs from argv[1], which ESM actions compare.
func nodeActionCommand(script string) []string {
	return []string{"node", "--preserve-symlinks-main", script}
}

// dockerActionImageTag derives the local docker image tag used when an action
// is built from a Dockerfile.
//
// For Gitea: a local action (`uses: ./` or `uses: ./path`) has an actionName
// that is the workspace-relative path of the action. That path is identical
// across repositories (e.g. "./" for a self-referencing action), so without
// namespacing, every repository's local docker action would build and reuse the
// same `act-dockeraction:latest` image on a shared docker daemon. A subsequent
// repository would then silently run the image built for an earlier one.
// Including the repository keeps the tag stable for caching within a repository
// while preventing cross-repository collisions.
// See https://gitea.com/gitea/runner/issues/1039.
func dockerActionImageTag(repository, actionName string, localAction bool) string {
	name := actionName
	if localAction {
		name = path.Join(repository, actionName)
	}
	// The human-readable name is sanitized by collapsing every non-alphanumeric character to "-".
	sanitized := dockerActionNamePattern.ReplaceAllString(name, "-")
	if localAction {
		// For local actions a short hash of the raw repository and action path is appended so the tag stays unique per repository.
		sum := sha256.Sum256([]byte(repository + "\x00" + actionName))
		sanitized += "-" + hex.EncodeToString(sum[:])[:12]
	}
	// "-dockeraction" ensures that "./", "./test " won't get converted to "act-:latest", "act-test-:latest" which are invalid docker image names
	return strings.ToLower("act-" + strings.TrimLeft(sanitized+"-dockeraction:latest", "-"))
}

// One image per step and job: its stages share a build, and concurrent jobs never run each other's.
func (rc *RunContext) dockerActionImage(ctx context.Context, step actionStep, actionName string, localAction bool) string {
	job := rc.topLevelRunContext()
	if job.dockerActionImages == nil {
		job.dockerActionImages = map[*model.Step]string{}
	}
	image, ok := job.dockerActionImages[step.getStepModel()]
	if !ok {
		image = dockerActionImageTag(getStepGithubContext(ctx, step).Repository, actionName, localAction) + "-" + strings.ToLower(rand.Text())
		if !common.Dryrun(ctx) { // a dry run builds nothing to remove
			job.dockerActionImages[step.getStepModel()] = image
		}
	}
	return image
}

// TODO: break out parts of function to reduce complexicity
func execAsDocker(ctx context.Context, step actionStep, actionName, actionDir, basedir string, localAction bool, stage stepStage) error {
	logger := common.Logger(ctx)
	rc := step.getRunContext()
	action := step.getActionModel()
	if rc.kubernetes {
		return errKubernetesDocker
	}

	var prepImage common.Executor
	var image string
	forcePull := false
	if after, ok := strings.CutPrefix(action.Runs.Image, "docker://"); ok {
		image = after
		// Apply forcePull only for prebuild docker images
		forcePull = rc.Config.ForcePull
	} else {
		image = rc.dockerActionImage(ctx, step, actionName, localAction)
		contextDir, fileName := filepath.Split(filepath.Join(basedir, action.Runs.Image))

		anyArchExists, err := ContainerImageExistsLocally(ctx, image, "any")
		if err != nil {
			return err
		}

		correctArchExists := anyArchExists
		if anyArchExists && rc.Config.ContainerArchitecture != "" && rc.Config.ContainerArchitecture != "any" {
			correctArchExists, err = ContainerImageExistsLocally(ctx, image, rc.Config.ContainerArchitecture)
			if err != nil {
				return err
			}
		}

		if anyArchExists && !correctArchExists {
			wasRemoved, err := container.RemoveImage(ctx, image, true, true)
			if err != nil {
				return err
			}
			if !wasRemoved {
				return fmt.Errorf("failed to remove image '%s'", image)
			}
		}

		if !correctArchExists || rc.Config.ForceRebuild {
			logger.Debugf("image '%s' for architecture '%s' will be built from context '%s", image, rc.Config.ContainerArchitecture, contextDir)
			var buildContext io.ReadCloser
			if localAction {
				buildContext, err = rc.JobContainer.GetContainerArchive(ctx, contextDir+"/.")
				if err != nil {
					return err
				}
				defer buildContext.Close()
			}
			prepImage = ContainerNewDockerBuildExecutor(container.NewDockerBuildExecutorInput{
				ContextDir:   contextDir,
				Dockerfile:   fileName,
				ImageTag:     image,
				BuildContext: buildContext,
				Platform:     rc.Config.ContainerArchitecture,
				BuildArgs:    rc.proxyBuildArgs(),
			})
			if buildContext == nil {
				// Held across the whole build: the daemon drains contextDir lazily.
				inner := prepImage
				prepImage = func(ctx context.Context) error {
					unlock, err := git.AcquireCloneLock(ctx, actionDir)
					if err != nil {
						return err
					}
					defer unlock()
					return inner(ctx)
				}
			}
		} else {
			logger.Debugf("image '%s' for architecture '%s' already exists", image, rc.Config.ContainerArchitecture)
		}
	}
	ee, err := evalDockerEnv(ctx, step, action)
	if err != nil {
		return err
	}
	var cmd []string
	if action.Runs.Args != nil {
		// a fresh slice, the manifest is evaluated again for every stage
		cmd = make([]string, len(action.Runs.Args))
		for i, v := range action.Runs.Args {
			if cmd[i], err = ee.Interpolate(ctx, v); err != nil {
				return fmt.Errorf("unable to interpolate runs.args: %w", err)
			}
		}
	} else {
		cmd, err = shellquote.Split(stepInput(step.getStepModel(), "args"))
		if err != nil {
			return err
		}
	}
	entrypoint, err := dockerEntrypoint(step, stage)
	if err != nil {
		return err
	}
	stepContainer := newStepContainer(ctx, step, image, cmd, entrypoint, rc.Config.ContainerOptions)
	return common.NewPipelineExecutor(
		prepImage,
		stepContainer.Pull(forcePull),
		stepContainer.Remove(),
		stepContainer.Create(rc.Config.ContainerCapAdd, rc.Config.ContainerCapDrop),
		stepContainer.Start(true),
	).Finally(stepContainer.Close())(ctx)
}

// dockerEntrypoint returns the entrypoint the action's image runs with for the given
// stage. Only the main stage honours the `entrypoint` input.
func dockerEntrypoint(step actionStep, stage stepStage) ([]string, error) {
	runs := step.getActionModel().Runs

	var entrypoint string
	switch stage {
	case stepStagePre:
		entrypoint = runs.PreEntrypoint
	case stepStagePost:
		entrypoint = runs.PostEntrypoint
	default:
		entrypoint = runs.Entrypoint
		if entrypoint == "" {
			if fields := strings.Fields(stepInput(step.getStepModel(), "entrypoint")); len(fields) > 0 {
				return fields, nil
			}
		}
	}

	if entrypoint == "" {
		return nil, nil
	}
	return shellquote.Split(entrypoint)
}

// evalDockerEnv returns an evaluator bound to the environment it installed.
func evalDockerEnv(ctx context.Context, step step, action *model.Action) (*expressionEvaluator, error) {
	rc := step.getRunContext()

	var err error
	inputs := make(map[string]string)
	providedInputs := make(map[string]bool, len(step.getStepModel().With))
	for key := range step.getStepModel().With {
		providedInputs[strings.ToLower(key)] = true
	}
	eval := rc.NewStepExpressionEvaluator(ctx, step)
	for k, input := range action.Inputs {
		if providedInputs[strings.ToLower(k)] {
			continue
		}
		if inputs[k], err = eval.Interpolate(ctx, input.Default); err != nil {
			return nil, fmt.Errorf("unable to interpolate the default of input %s: %w", k, err)
		}
	}
	mergeIntoMap(step, step.getEnv(), inputs, step.getStepModel().With)

	ee := rc.NewActionInputsExpressionEvaluator(ctx, step)
	runsEnv := make(map[string]string, len(action.Runs.Env))
	for k, v := range action.Runs.Env {
		if runsEnv[k], err = ee.Interpolate(ctx, v); err != nil {
			return nil, fmt.Errorf("unable to interpolate env %s: %w", k, err)
		}
	}
	mergeIntoMap(step, step.getEnv(), runsEnv, maps.Clone(*step.getEnv()))
	aliasGiteaEnv(*step.getEnv())
	return ee, nil
}

func newStepContainer(ctx context.Context, step step, image string, cmd, entrypoint []string, runnerOptions string) container.Container {
	rc := step.getRunContext()
	logWriter := rc.commandLogWriter(ctx)
	envList := make([]string, 0)
	for k, v := range *step.getEnv() {
		if rc.IsHostEnv() && strings.EqualFold(k, rc.JobContainer.GetPathVariableName()) && v == rc.JobContainer.DefaultPathVariable() { // the runner's own PATH stays out, as on GitHub
			continue
		}
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	envList = append(envList, rc.runnerEnv(ctx)...)

	binds, mounts := rc.GetBindsAndMounts(ctx)
	networkMode := "container:" + rc.jobContainerName()
	if rc.IsHostEnv() {
		networkMode = "default"
	}
	return ContainerNewContainer(&container.NewContainerInput{
		Cmd:           cmd,
		Entrypoint:    entrypoint,
		WorkingDir:    rc.JobContainer.ToContainerPath(rc.Config.Workdir),
		Image:         image,
		Name:          createContainerName(rc.jobContainerName(), "STEP-"+step.getStepModel().ID),
		Env:           envList,
		Mounts:        mounts,
		NetworkMode:   networkMode,
		Binds:         binds,
		Stdout:        logWriter,
		Stderr:        logWriter,
		Privileged:    rc.Config.Privileged,
		UsernsMode:    rc.Config.UsernsMode,
		Platform:      rc.Config.ContainerArchitecture,
		RunnerOptions: runnerOptions,
		AutoRemove:    true,
		ValidVolumes:  rc.validVolumes(),
		AllocatePTY:   rc.Config.AllocatePTY,
	})
}

func populateEnvsFromSavedState(env *map[string]string, step actionStep, rc *RunContext) {
	for name, value := range rc.IntraActionState[step.getStepModel().ID] {
		(*env)["STATE_"+name] = value
	}
}

func inputEnvKey(inputID string) string {
	return "INPUT_" + inputEnvPattern.ReplaceAllString(strings.ToUpper(inputID), "_")
}

func populateEnvsFromInput(ctx context.Context, step step, action *model.Action) error {
	env := step.getEnv()
	eval := step.getRunContext().NewStepExpressionEvaluator(ctx, step)
	for inputID, input := range action.Inputs {
		envKey := inputEnvKey(inputID)
		if _, ok := (*env)[envKey]; !ok {
			var err error
			if (*env)[envKey], err = eval.Interpolate(ctx, input.Default); err != nil {
				return fmt.Errorf("unable to interpolate the default of input %s: %w", inputID, err)
			}
		}
	}
	return nil
}

// lookupFold finds name in values, ignoring case where the environment does, as GitHub's inputs always do.
func lookupFold[V any](values map[string]V, name string, caseInsensitive bool) (V, bool) {
	if value, ok := values[name]; ok || !caseInsensitive {
		return value, ok
	}
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	var zero V
	return zero, false
}

func stepInput(step *model.Step, name string) string {
	value, _ := lookupFold(step.With, name, true)
	return value
}

func warnUnexpectedInputs(ctx context.Context, action *model.Action, inputs map[string]string) {
	docker := action.Runs.Using.IsDocker()
	var unexpectedInputs []string
	for input := range inputs {
		_, declared := lookupFold(action.Inputs, input, true)
		containerInput := docker && (strings.EqualFold(input, "entryPoint") || strings.EqualFold(input, "args"))
		if !declared && !containerInput {
			unexpectedInputs = append(unexpectedInputs, input)
		}
	}
	if len(unexpectedInputs) == 0 {
		return
	}
	validInputs := slices.Sorted(maps.Keys(action.Inputs))
	if docker {
		validInputs = append([]string{"entryPoint", "args"}, validInputs...)
	}
	slices.Sort(unexpectedInputs)
	common.Logger(ctx).With(rawOutputField, true).Warnf("##[warning]%s", EscapeCommandData(fmt.Sprintf(
		"Unexpected input(s) '%s', valid inputs are ['%s']", strings.Join(unexpectedInputs, "', '"), strings.Join(validInputs, "', '"))))
}

func getContainerActionPaths(step actionStep) (string, string) {
	rc := step.getRunContext()
	if sar, ok := step.(*stepActionRemote); ok { // a copy per action, as its repository's directory is shared
		actionName := path.Join(sar.containerKey(), sar.remoteAction.Path)
		return actionName, rc.JobContainer.GetActPath() + "/actions/" + actionName
	}
	actionName := getOsSafeRelativePath(filepath.Join(rc.Config.Workdir, step.getStepModel().Uses), rc.Config.Workdir)
	return "./" + actionName, rc.JobContainer.ToContainerPath(rc.Config.Workdir) + "/" + actionName
}

func getOsSafeRelativePath(s, prefix string) string {
	actionName := strings.TrimPrefix(s, prefix)
	if runtime.GOOS == "windows" {
		actionName = strings.ReplaceAll(actionName, "\\", "/")
	}
	actionName = strings.TrimPrefix(actionName, "/")

	return actionName
}

func shouldRunPreStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		log := common.Logger(ctx)

		if step.getActionModel() == nil {
			log.Debugf("skip pre step for '%s': no action model available", step.getStepModel())
			return false
		}

		return true
	}
}

func hasPreStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		action := step.getActionModel()
		return action.Runs.Using.IsComposite() ||
			(action.Runs.Using.IsNode() &&
				action.Runs.Pre != "") ||
			(action.Runs.Using.IsDocker() &&
				action.Runs.PreEntrypoint != "") ||
			(action.Runs.Using == model.ActionRunsUsingGo &&
				action.Runs.Pre != "")
	}
}

// actionStagePaths resolves where a step's action lives and where the job container sees
// it, for the pre and post stage.
func actionStagePaths(step actionStep) (actionDir, actionPath, actionName, containerActionDir string) {
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	if sar, ok := step.(*stepActionRemote); ok {
		actionDir = sar.actionDir
		actionPath = sar.remoteAction.Path
	} else {
		actionDir = filepath.Join(rc.Config.Workdir, stepModel.Uses)
	}

	actionName, containerActionDir = getContainerActionPaths(step)
	return actionDir, actionPath, actionName, containerActionDir
}

// execDockerActionStage runs a docker action's image for its pre or post stage.
func execDockerActionStage(ctx context.Context, step actionStep, stage stepStage) error {
	actionDir, actionPath, actionName, containerActionDir := actionStagePaths(step)

	_, remote := step.(*stepActionRemote)
	location := containerActionDir
	if remote {
		location = path.Join(actionDir, actionPath)
	}
	return execAsDocker(ctx, step, actionName, actionDir, location, !remote, stage)
}

func runPreStep(step actionStep) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("run pre step for '%s'", step.getStepModel())

		rc := step.getRunContext()
		action := step.getActionModel()

		actionDir, actionPath, _, containerActionDir := actionStagePaths(step)

		x := action.Runs.Using
		if !x.IsComposite() {
			// defaults in pre steps were missing, however provided inputs are available
			if err := populateEnvsFromInput(ctx, step, action); err != nil {
				return err
			}
		}
		printRunActionHeader(ctx, step.getStepModel(), action, *step.getEnv(), rc)
		switch {
		case x.IsNode():
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			containerArgs := nodeActionCommand(path.Join(containerActionDir, action.Runs.Pre))
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.JobContainer.Exec(containerArgs, *step.getEnv(), "", "")(ctx)

		case x.IsDocker():
			return execDockerActionStage(ctx, step, stepStagePre)

		case x.IsComposite():
			if step.getCompositeSteps() == nil {
				if _, err := step.getCompositeRunContext(ctx); err != nil {
					return err
				}
			}

			if steps := step.getCompositeSteps(); steps != nil && steps.pre != nil {
				return steps.pre(ctx)
			}
			return errors.New("missing steps in composite action")

		case x == model.ActionRunsUsingGo:
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			rc.ApplyExtraPath(ctx, step.getEnv())

			execFileName := action.Runs.Pre + ".out"
			return common.NewPipelineExecutor(
				rc.JobContainer.Exec([]string{"go", "build", "-o", execFileName, action.Runs.Pre}, *step.getEnv(), "", containerActionDir),
				rc.JobContainer.Exec([]string{filepath.Join(containerActionDir, execFileName)}, *step.getEnv(), "", ""),
			)(ctx)
		default:
			return nil
		}
	}
}

func shouldRunPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		if remote, ok := step.(*stepActionRemote); ok && remote.preStepStarted && !remote.action.Runs.Using.IsComposite() {
			return true
		}
		log := common.Logger(ctx)
		stepResults := step.getRunContext().StepResults
		stepResult := stepResults[step.getStepModel().ID]

		if stepResult == nil {
			log.With("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s'; step was not executed", step.getStepModel())
			return false
		}

		if stepResult.Conclusion == model.StepStatusSkipped {
			log.With("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s'; main step was skipped", step.getStepModel())
			return false
		}

		if step.getActionModel() == nil {
			log.With("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s': no action model available", step.getStepModel())
			return false
		}

		return true
	}
}

func hasPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		action := step.getActionModel()
		return action.Runs.Using.IsComposite() ||
			(action.Runs.Using.IsNode() &&
				action.Runs.Post != "") ||
			(action.Runs.Using.IsDocker() &&
				action.Runs.PostEntrypoint != "") ||
			(action.Runs.Using == model.ActionRunsUsingGo &&
				action.Runs.Post != "")
	}
}

func runPostStep(step actionStep) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		logger.Debugf("run post step for '%s'", step.getStepModel())

		rc := step.getRunContext()
		action := step.getActionModel()

		actionDir, actionPath, _, containerActionDir := actionStagePaths(step)

		x := action.Runs.Using
		if !x.IsComposite() { // a composite's children print their own
			logger.With(rawOutputField, true).Info("Post job cleanup.")
		}
		switch {
		case x.IsNode():

			populateEnvsFromSavedState(step.getEnv(), step, rc)

			containerArgs := nodeActionCommand(path.Join(containerActionDir, action.Runs.Post))
			logger.Debugf("executing remote job container: %s", containerArgs)

			rc.ApplyExtraPath(ctx, step.getEnv())

			return rc.JobContainer.Exec(containerArgs, *step.getEnv(), "", "")(ctx)

		case x.IsDocker():
			populateEnvsFromSavedState(step.getEnv(), step, rc)

			return execDockerActionStage(ctx, step, stepStagePost)

		case x.IsComposite():
			if err := maybeCopyToActionDir(ctx, step, actionDir, actionPath, containerActionDir); err != nil {
				return err
			}

			if steps := step.getCompositeSteps(); steps != nil && steps.post != nil {
				return steps.post(ctx)
			}
			return errors.New("missing steps in composite action")

		case x == model.ActionRunsUsingGo:
			populateEnvsFromSavedState(step.getEnv(), step, rc)
			rc.ApplyExtraPath(ctx, step.getEnv())

			execFileName := action.Runs.Post + ".out"
			return common.NewPipelineExecutor(
				rc.JobContainer.Exec([]string{"go", "build", "-o", execFileName, action.Runs.Post}, *step.getEnv(), "", containerActionDir),
				rc.JobContainer.Exec([]string{filepath.Join(containerActionDir, execFileName)}, *step.getEnv(), "", ""),
			)(ctx)

		default:
			return nil
		}
	}
}
