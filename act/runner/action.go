// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"crypto/sha256"
	"embed"
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
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/container"

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
var trampoline embed.FS

var (
	ContainerImageExistsLocally     = container.ImageExistsLocally
	ContainerNewDockerBuildExecutor = container.NewDockerBuildExecutor
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
			_, closer, err := readFile("Dockerfile")
			addError("Dockerfile", err)
			if err == nil {
				closer.Close()
				action := &model.Action{
					Name: "(Synthetic)",
					Runs: model.ActionRuns{
						Using: "docker",
						Image: "Dockerfile",
					},
				}
				logger.Debugf("Using synthetic action %v for Dockerfile", action)
				return action, nil
			}
			if step.With != nil {
				if val, ok := step.With["args"]; ok {
					var b []byte
					if b, err = trampoline.ReadFile("res/trampoline.js"); err != nil {
						return nil, err
					}
					err2 := writeFile(filepath.Join(actionDir, actionPath, "trampoline.js"), b, 0o400)
					if err2 != nil {
						return nil, err2
					}
					action := &model.Action{
						Name: "(Synthetic)",
						Inputs: map[string]model.Input{
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
	}
	if allErrors != nil {
		return nil, errors.Join(allErrors...)
	}
	defer closer.Close()

	action, err := model.ReadAction(reader)
	return action, err
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

	defer git.AcquireCloneLock(actionDir)()

	if !rc.Config.NoActionPatch {
		// A concurrent job's prepare resets this directory, so patch under the copy's lock.
		defer patchActions(ctx, actionScriptPaths(filepath.Join(actionDir, actionPath), step.getActionModel()))()
	}

	if err := removeGitIgnore(ctx, actionDir); err != nil {
		return err
	}

	return rc.JobContainer.CopyDir(containerActionDirCopy, actionDir+"/", rc.Config.UseGitIgnore, true)(ctx)
}

func runActionImpl(step actionStep, actionDir string, remoteAction *remoteAction) common.Executor {
	rc := step.getRunContext()
	stepModel := step.getStepModel()

	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		actionPath := ""
		if remoteAction != nil && remoteAction.Path != "" {
			actionPath = remoteAction.Path
		}

		action := step.getActionModel()

		rc.withGithubEnv(ctx, step.getGithubContext(ctx), *step.getEnv())
		populateEnvsFromSavedState(step.getEnv(), step, rc)
		if err := populateEnvsFromInput(ctx, step.getEnv(), action, rc); err != nil {
			return err
		}

		actionLocation := path.Join(actionDir, actionPath)
		actionName, containerActionDir := getContainerActionPaths(stepModel, actionLocation, rc)

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
			location := actionLocation
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
			buildArgs := []string{"go", "build", "-o", execFileName, action.Runs.Main}
			execArgs := []string{filepath.Join(containerActionDir, execFileName)}

			return common.NewPipelineExecutor(
				rc.JobContainer.Exec(buildArgs, *step.getEnv(), "", containerActionDir),
				rc.JobContainer.Exec(execArgs, *step.getEnv(), "", ""),
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

// https://github.com/nektos/act/issues/228#issuecomment-629709055
// files in .gitignore are not copied in a Docker container
// this causes issues with actions that ignore other important resources
// such as `node_modules` for example
func removeGitIgnore(ctx context.Context, directory string) error {
	gitIgnorePath := path.Join(directory, ".gitignore")
	if _, err := os.Stat(gitIgnorePath); err == nil {
		// .gitignore exists
		common.Logger(ctx).Debugf("Removing %s before docker cp", gitIgnorePath)
		err := os.Remove(gitIgnorePath)
		if err != nil {
			return err
		}
	}
	return nil
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
	sanitized := regexp.MustCompile("[^a-zA-Z0-9]").ReplaceAllString(name, "-")
	if localAction {
		// For local actions a short hash of the raw repository and action path is appended so the tag stays unique per repository.
		sum := sha256.Sum256([]byte(repository + "\x00" + actionName))
		sanitized += "-" + hex.EncodeToString(sum[:])[:12]
	}
	// "-dockeraction" ensures that "./", "./test " won't get converted to "act-:latest", "act-test-:latest" which are invalid docker image names
	image := fmt.Sprintf("%s-dockeraction:%s", sanitized, "latest")
	image = "act-" + strings.TrimLeft(image, "-")
	return strings.ToLower(image)
}

// TODO: break out parts of function to reduce complexicity
func execAsDocker(ctx context.Context, step actionStep, actionName, actionDir, basedir string, localAction bool, stage stepStage) error {
	logger := common.Logger(ctx)
	rc := step.getRunContext()
	action := step.getActionModel()

	var prepImage common.Executor
	var image string
	forcePull := false
	if after, ok := strings.CutPrefix(action.Runs.Image, "docker://"); ok {
		image = after
		// Apply forcePull only for prebuild docker images
		forcePull = rc.Config.ForcePull
	} else {
		image = dockerActionImageTag(step.getGithubContext(ctx).Repository, actionName, localAction)
		contextDir, fileName := filepath.Split(filepath.Join(basedir, action.Runs.Image))

		anyArchExists, err := ContainerImageExistsLocally(ctx, image, "any")
		if err != nil {
			return err
		}

		correctArchExists, err := ContainerImageExistsLocally(ctx, image, rc.Config.ContainerArchitecture)
		if err != nil {
			return err
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
					defer git.AcquireCloneLock(actionDir)()
					return inner(ctx)
				}
			}
		} else {
			logger.Debugf("image '%s' for architecture '%s' already exists", image, rc.Config.ContainerArchitecture)
		}
	}
	cmd, err := shellquote.Split(step.getStepModel().With["args"])
	if err != nil {
		return err
	}
	ee, err := evalDockerEnv(ctx, step, action)
	if err != nil {
		return err
	}
	if action.Runs.Args != nil {
		// a fresh slice, the manifest is evaluated again for every stage
		cmd = make([]string, len(action.Runs.Args))
		for i, v := range action.Runs.Args {
			if cmd[i], err = ee.Interpolate(ctx, v); err != nil {
				return fmt.Errorf("unable to interpolate runs.args: %w", err)
			}
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
			if fields := strings.Fields(step.getStepModel().With["entrypoint"]); len(fields) > 0 {
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
	eval := rc.NewExpressionEvaluator(ctx)
	// Set Defaults
	for k, input := range action.Inputs {
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
	return ee, nil
}

func newStepContainer(ctx context.Context, step step, image string, cmd, entrypoint []string, runnerOptions string) container.Container {
	rc := step.getRunContext()
	logWriter := rc.commandLogWriter(ctx)
	envList := make([]string, 0)
	for k, v := range *step.getEnv() {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	envList = append(envList, rc.runnerEnv(ctx)...)

	binds, mounts := rc.GetBindsAndMounts()
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
	state, ok := rc.IntraActionState[step.getStepModel().ID]
	if ok {
		for name, value := range state {
			envName := "STATE_" + name
			(*env)[envName] = value
		}
	}
}

func populateEnvsFromInput(ctx context.Context, env *map[string]string, action *model.Action, rc *RunContext) error {
	eval := rc.NewExpressionEvaluator(ctx)
	for inputID, input := range action.Inputs {
		envKey := regexp.MustCompile("[^A-Z0-9-]").ReplaceAllString(strings.ToUpper(inputID), "_")
		envKey = "INPUT_" + envKey
		if _, ok := (*env)[envKey]; !ok {
			var err error
			if (*env)[envKey], err = eval.Interpolate(ctx, input.Default); err != nil {
				return fmt.Errorf("unable to interpolate the default of input %s: %w", inputID, err)
			}
		}
	}
	return nil
}

func getContainerActionPaths(step *model.Step, actionDir string, rc *RunContext) (string, string) {
	actionName := ""
	containerActionDir := "."
	if step.Type() != model.StepTypeUsesActionRemote {
		actionName = getOsSafeRelativePath(actionDir, rc.Config.Workdir)
		containerActionDir = rc.JobContainer.ToContainerPath(rc.Config.Workdir) + "/" + actionName
		actionName = "./" + actionName
	} else if step.Type() == model.StepTypeUsesActionRemote {
		actionName = getOsSafeRelativePath(actionDir, rc.ActionCacheDir())
		containerActionDir = rc.JobContainer.GetActPath() + "/actions/" + actionName
	}

	if actionName == "" {
		actionName = filepath.Base(actionDir)
		if runtime.GOOS == "windows" {
			actionName = strings.ReplaceAll(actionName, "\\", "/")
		}
	}
	return actionName, containerActionDir
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
		actionDir = sar.actionDir()
		actionPath = sar.remoteAction.Path
	} else {
		actionDir = filepath.Join(rc.Config.Workdir, stepModel.Uses)
	}

	actionName, containerActionDir = getContainerActionPaths(stepModel, path.Join(actionDir, actionPath), rc)
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
			if err := populateEnvsFromInput(ctx, step.getEnv(), action, rc); err != nil {
				return err
			}
		}
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
			buildArgs := []string{"go", "build", "-o", execFileName, action.Runs.Pre}
			execArgs := []string{filepath.Join(containerActionDir, execFileName)}

			return common.NewPipelineExecutor(
				rc.JobContainer.Exec(buildArgs, *step.getEnv(), "", containerActionDir),
				rc.JobContainer.Exec(execArgs, *step.getEnv(), "", ""),
			)(ctx)
		default:
			return nil
		}
	}
}

func shouldRunPostStep(step actionStep) common.Conditional {
	return func(ctx context.Context) bool {
		log := common.Logger(ctx)
		stepResults := step.getRunContext().getStepsContext()
		stepResult := stepResults[step.getStepModel().ID]

		if stepResult == nil {
			log.WithField("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s'; step was not executed", step.getStepModel())
			return false
		}

		if stepResult.Conclusion == model.StepStatusSkipped {
			log.WithField("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s'; main step was skipped", step.getStepModel())
			return false
		}

		if step.getActionModel() == nil {
			log.WithField("stepResult", model.StepStatusSkipped).Debugf("skipping post step for '%s': no action model available", step.getStepModel())
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
			buildArgs := []string{"go", "build", "-o", execFileName, action.Runs.Post}
			execArgs := []string{filepath.Join(containerActionDir, execFileName)}

			return common.NewPipelineExecutor(
				rc.JobContainer.Exec(buildArgs, *step.getEnv(), "", containerActionDir),
				rc.JobContainer.Exec(execArgs, *step.getEnv(), "", ""),
			)(ctx)

		default:
			return nil
		}
	}
}
