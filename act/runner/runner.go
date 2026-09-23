// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	docker_container "github.com/moby/moby/api/types/container"
	log "github.com/sirupsen/logrus"
	"go.yaml.in/yaml/v4"
)

// Config contains the config for a new runner
type Config struct {
	Actor                         string                                        // the user that triggered the event
	Workdir                       string                                        // path to working directory
	ActionCacheDir                string                                        // path used for caching action contents
	ActionOfflineMode             bool                                          // when offline, use cached action contents
	ActionCloneDepth              int                                           // limit history when cloning an action repo, 0 clones every branch in full
	BindWorkdir                   bool                                          // bind the workdir to the job container
	EventName                     string                                        // name of event to run
	EventPath                     string                                        // path to JSON file to use for event.json in containers
	ForcePull                     bool                                          // force pulling of the image, even if already present
	ForceRebuild                  bool                                          // force rebuilding local docker image action
	JSONLogger                    bool                                          // use json or text logger
	Env                           map[string]string                             // env for containers
	Secrets                       map[string]string                             // list of secrets
	ExtraMasks                    []string                                      // values to hide that are not in Secrets, such as the proxy password
	Vars                          map[string]string                             // list of vars
	Token                         string                                        // GitHub token
	InsecureSecrets               bool                                          // switch hiding output when printing to terminal
	Privileged                    bool                                          // use privileged mode
	UsernsMode                    string                                        // user namespace to use
	ContainerArchitecture         string                                        // Desired OS/architecture platform for running containers
	ContainerDaemonSocket         string                                        // Path to Docker daemon socket
	ContainerOptions              string                                        // Options for the job container
	UseGitIgnore                  bool                                          // controls if paths in .gitignore should not be copied into container, default true
	GitHubInstance                string                                        // GitHub instance to use, default "github.com"
	ContainerCapAdd               []string                                      // list of kernel capabilities to add to the containers
	ContainerCapDrop              []string                                      // list of kernel capabilities to remove from the containers
	ArtifactServerPath            string                                        // the path where the artifact server stores uploads
	ArtifactServerAddr            string                                        // the address the artifact server binds to
	ArtifactServerPort            string                                        // the port the artifact server binds to
	NoSkipCheckout                bool                                          // do not skip actions/checkout
	DisableActEnv                 bool                                          // do not inject the ACT=true environment variable into jobs
	ContainerNetworkMode          docker_container.NetworkMode                  // the network mode of job containers (the value of --network)
	ContainerNetworkCreateOptions container.NewDockerNetworkCreateExecutorInput // the default network create options
	ProxyEnv                      map[string]string                             // the proxy variables the job runs with, also given to service containers and image builds
	NoActionPatch                 bool                                          // run actions exactly as published, applying no compatibility patches, see patch_actions.go

	PresetGitHubContext   *model.GithubContext // overrides actor, ref, repository, token and related context fields
	EventJSON             string               // the content of JSON file to use for event.json in containers, overrides EventPath
	ContainerNamePrefix   string               // the prefix of container name
	ContainerMaxLifetime  time.Duration        // the max lifetime of job containers
	CleanWorkdir          bool                 // remove host executor workdir on teardown
	DefaultActionInstance string               // the default actions web site
	// DefaultActionInstanceIsSelfHosted reports whether DefaultActionInstance is this
	// self-hosted Gitea (DEFAULT_ACTIONS_URL=self). It gates token trust: only then may the
	// task token be attached to action clone URLs on DefaultActionInstance's host, which can
	// differ from GitHubInstance when the runner registered with a different hostname than
	// AppURL. It is never set for github.com or a GithubMirror, so the token stays on-instance.
	DefaultActionInstanceIsSelfHosted bool
	PlatformPicker                    func(labels []string) string
	JobLoggerLevel                    *log.Level    // the level of job logger
	ValidVolumes                      []string      // only volumes (and bind mounts) in this slice can be mounted on the job container or service containers
	SharedToolCache                   bool          // one tool cache for all jobs instead of one per job
	InsecureSkipTLS                   bool          // whether to skip verifying TLS certificate of the Gitea instance
	MaxParallel                       int           // max parallel jobs to run across all workflows (0 = no limit, uses CPU count)
	AllocatePTY                       bool          // allocate a pseudo-TTY for each step's process
	ServiceReadyTimeout               time.Duration // how long a job waits for its service containers to report healthy (0 uses the default)
	RunnerName                        string        // name this runner registered with, reported as `runner.name`, defaults to the hostname
	JobStartedHook                    string        // script run inside the job environment before the job's first step; ACTIONS_RUNNER_HOOK_JOB_STARTED is read from Env when empty
	JobCompletedHook                  string        // script run inside the job environment after the job's last step; ACTIONS_RUNNER_HOOK_JOB_COMPLETED is read from Env when empty
}

// RunnerDebug reports whether debug logging is on, exposed as `runner.debug` and
// RUNNER_DEBUG. Only the secret also makes the reporter keep ::debug:: output, the env
// is accepted for `exec` and for runners configured with it.
func (c Config) RunnerDebug() bool {
	return c.Secrets["ACTIONS_STEP_DEBUG"] == "true" || c.Env["ACTIONS_STEP_DEBUG"] == "true"
}

// GetToken: Adapt to Gitea
func (c Config) GetToken() string {
	token := c.Secrets["GITHUB_TOKEN"]
	if c.Secrets["GITEA_TOKEN"] != "" {
		token = c.Secrets["GITEA_TOKEN"]
	}
	return token
}

// DefaultActionURL returns the host used for implicit remote actions.
func (c Config) DefaultActionURL() string {
	if c.DefaultActionInstance != "" {
		return c.DefaultActionInstance
	}
	if c.GitHubInstance != "" {
		return c.GitHubInstance
	}
	return "github.com"
}

type caller struct {
	runContext *RunContext

	updateResultLock         sync.Mutex        // For Gitea
	reusedWorkflowJobResults map[string]string // For Gitea
}

type runnerImpl struct {
	config    *Config
	eventJSON string
	caller    *caller // the job calling this runner (caller of a reusable workflow)
}

type Runner = runnerImpl

// New Creates a new Runner
func New(runnerConfig *Config) (*Runner, error) {
	runner := &runnerImpl{
		config: runnerConfig,
	}

	return runner.configure()
}

func (runner *runnerImpl) configure() (*runnerImpl, error) {
	if runner.config.RunnerName == "" {
		// Callers that do not register, such as `exec`, still get a `runner.name`.
		runner.config.RunnerName, _ = os.Hostname()
	}

	runner.eventJSON = "{}"
	if runner.config.EventJSON != "" {
		runner.eventJSON = runner.config.EventJSON
	} else if runner.config.EventPath != "" {
		log.Debugf("Reading event.json from %s", runner.config.EventPath)
		eventJSONBytes, err := os.ReadFile(runner.config.EventPath)
		if err != nil {
			return nil, err
		}
		runner.eventJSON = string(eventJSONBytes)
	}
	return runner, nil
}

func maxParallelFor(strategy *model.Strategy, combinations int) int {
	maxParallel := 4 // actionslib has no default for an undeclared max-parallel
	if strategy != nil {
		if limit, err := strategy.MaxParallel(); err != nil {
			log.Errorf("Ignoring invalid max-parallel: %v", err)
		} else if limit > 0 {
			maxParallel = limit
		}
	}
	return min(maxParallel, combinations)
}

// NewPlanExecutor ...
func (runner *runnerImpl) NewPlanExecutor(plan *model.Plan) common.Executor {
	maxJobNameLen := 0

	stagePipeline := make([]common.Executor, 0)
	log.Debugf("Plan Stages: %v", plan.Stages)

	for i := range plan.Stages {
		stage := plan.Stages[i]
		stagePipeline = append(stagePipeline, func(ctx context.Context) error {
			pipeline := make([]common.Executor, 0)
			for _, run := range stage.Runs {
				log.Debugf("Stages Runs: %v", stage.Runs)
				stageExecutor := make([]common.Executor, 0)
				job := run.Job()
				log.Debugf("Job.Name: %v", job.Name)
				log.Debugf("Job.RawNeeds: %v", job.RawNeeds)
				log.Debugf("Job.RawRunsOn: %v", job.RawRunsOn)
				log.Debugf("Job.Env: %v", job.Env)
				log.Debugf("Job.If: %v", job.If)
				for step := range job.Steps {
					if nil != job.Steps[step] {
						log.Debugf("Job.Steps: %v", job.Steps[step].String())
					}
				}
				log.Debugf("Job.TimeoutMinutes: %v", job.TimeoutMinutes)
				log.Debugf("Job.Services: %v", job.Services)
				log.Debugf("Job.Strategy: %v", job.Strategy)
				log.Debugf("Job.RawContainer: %v", job.RawContainer)
				log.Debugf("Job.Defaults.Run.Shell: %v", job.Defaults.Run.Shell)
				log.Debugf("Job.Defaults.Run.WorkingDirectory: %v", job.Defaults.Run.WorkingDirectory)
				log.Debugf("Job.Outputs: %v", job.Outputs)
				log.Debugf("Job.Uses: %v", job.Uses)
				log.Debugf("Job.With: %v", job.With)
				log.Debugf("Job.Result: %v", job.Result)

				if job.Strategy != nil || job.RawStrategy.Kind == yaml.ScalarNode {
					strategyRc, err := runner.newRunContext(ctx, run, nil)
					if err != nil {
						return err
					}
					if job.RawStrategy.Kind == yaml.ScalarNode {
						if err := model.DecodeEvaluated("job strategy", job.RawStrategy, strategyRc.ExprEval.shared(ctx).EvaluateYamlNode, &job.Strategy); err != nil {
							return err
						}
					} else {
						log.Debugf("Job.Strategy.FailFast: %v", job.Strategy.GetFailFast())
						log.Debugf("Job.Strategy.FailFastString: %v", job.Strategy.FailFastString)
						log.Debugf("Job.Strategy.MaxParallelString: %v", job.Strategy.MaxParallelString)
						log.Debugf("Job.Strategy.RawMatrix: %v", job.Strategy.RawMatrix)
						// An unevaluated expression is left in place, which GetMatrixes below rejects.
						if err := strategyRc.NewExpressionEvaluator(ctx).EvaluateYamlNode(ctx, &job.Strategy.RawMatrix); err != nil {
							log.Errorf("Error while evaluating matrix: %v", err)
						}
						for _, value := range []*string{&job.Strategy.FailFastString, &job.Strategy.MaxParallelString} {
							if evaluated, err := strategyRc.ExprEval.Interpolate(ctx, *value); err == nil {
								*value = evaluated
							}
						}
					}
				}

				matrixes, err := job.GetMatrixes()
				if err != nil {
					return fmt.Errorf("could not get job matrix: %w", err)
				}
				log.Debugf("Job Matrices: %v", matrixes)

				maxParallel := maxParallelFor(job.Strategy, len(matrixes))

				log.Infof("Running job with maxParallel=%d for %d matrix combinations", maxParallel, len(matrixes))

				for i, matrix := range matrixes {
					index, total := i, len(matrixes)
					if runner.caller == nil && runner.config.PresetGitHubContext != nil && len(matrix) > 0 {
						var expanded struct {
							Index int `yaml:"job-index"`
							Total int `yaml:"job-total"`
						}
						_ = job.RawStrategy.Decode(&expanded) // Gitea expanded the combination, a version writing neither leaves them unknown
						index, total = expanded.Index, expanded.Total
					}
					rc, err := runner.newCombinationRunContext(ctx, run, matrix, index, total)
					if err != nil {
						return err
					}
					rc.JobName = rc.Name
					if len(matrixes) > 1 {
						rc.Name = fmt.Sprintf("%s-%d", rc.Name, i+1)
					}
					if len(rc.String()) > maxJobNameLen {
						maxJobNameLen = len(rc.String())
					}
					if rc.caller != nil { // For Gitea
						rc.caller.setReusedWorkflowJobResult(rc.Run.JobID, "pending")
					}
					stageExecutor = append(stageExecutor, func(ctx context.Context) error {
						jobName := fmt.Sprintf("%-*s", maxJobNameLen, rc.String())
						executor, err := rc.Executor()
						if err != nil {
							return err
						}

						jobCtx := common.WithJobErrorContainer(WithJobLogger(ctx, rc.Run.JobID, jobName, rc.Config, &rc.Masks, matrix))
						jobCtx, cancelTimeout := applyJobTimeout(jobCtx, rc, job)
						defer cancelTimeout()
						return executor(jobCtx)
					})
				}
				// Run all matrix combinations of this job, then drop its aggregation mutex: the
				// combos are the only users of it, so once they finish the jobMutexes entry can be
				// released, keeping the map from growing unbounded over a long-lived runner.
				stageParallel := common.NewParallelExecutor(maxParallel, stageExecutor...)
				pipeline = append(pipeline, func(ctx context.Context) error {
					defer jobMutexes.Delete(job)
					return stageParallel(ctx)
				})
			}

			// For pipeline execution:
			// - If only 1 element: run it directly (no need for additional parallelization)
			// - If multiple elements: run them in parallel up to maxParallel or ncpu
			if len(pipeline) == 0 {
				return nil
			}

			if len(pipeline) == 1 {
				// Single run/job: execute directly without additional parallelization wrapper
				// This ensures max-parallel is the only limiting factor
				log.Debugf("Single pipeline element, executing directly")
				return pipeline[0](ctx)
			}

			// Multiple runs/jobs: execute in parallel up to maxParallel (if set) or ncpu
			parallelism := runtime.NumCPU()

			// If MaxParallel is set in config, use it
			if runner.config.MaxParallel > 0 {
				parallelism = runner.config.MaxParallel
				log.Debugf("Using configured max-parallel: %d", parallelism)
			} else {
				log.Debugf("Using CPU count for parallelism: %d", parallelism)
			}

			// Don't exceed the number of pipeline elements
			if parallelism > len(pipeline) {
				parallelism = len(pipeline)
			}

			log.Infof("Executing %d pipeline elements with parallelism %d", len(pipeline), parallelism)
			return common.NewParallelExecutor(parallelism, pipeline...)(ctx)
		})
	}

	return common.NewPipelineExecutor(stagePipeline...).Then(handleFailure(plan))
}

func handleFailure(plan *model.Plan) common.Executor {
	return func(ctx context.Context) error {
		for _, stage := range plan.Stages {
			for _, run := range stage.Runs {
				if run.Job().Result == "failure" && !run.Job().ContinueOnError {
					return fmt.Errorf("job '%s' failed", run.String())
				}
			}
		}
		return nil
	}
}

func (runner *runnerImpl) newRunContext(ctx context.Context, run *model.Run, matrix map[string]any) (*RunContext, error) {
	return runner.newCombinationRunContext(ctx, run, matrix, 0, 1)
}

func (runner *runnerImpl) newCombinationRunContext(ctx context.Context, run *model.Run, matrix map[string]any, index, total int) (*RunContext, error) {
	rc := &RunContext{
		Config:      runner.config,
		Run:         run,
		EventJSON:   runner.eventJSON,
		StepResults: make(map[string]*model.StepResult),
		Matrix:      matrix,
		caller:      runner.caller,
		jobIndex:    index,
		jobTotal:    total,
	}
	if err := rc.resolveWorkflowCall(ctx); err != nil {
		return nil, err
	}
	rc.ExprEval = rc.NewExpressionEvaluator(ctx)
	rc.Name = rc.maskSecrets(rc.ExprEval.InterpolateName(ctx, run.String()))
	if job := run.Job(); job != nil && job.RawOutputs.Kind != 0 {
		job.Outputs = nil // a job no combination ran for reads "" as on GitHub, a Gitea needs placeholder has no RawOutputs and keeps its outputs
	}

	return rc, nil
}

// For Gitea
// Keyed by job id, not name: only the values are read, and masking can collapse two names into one.
func (c *caller) setReusedWorkflowJobResult(jobID, result string) {
	c.updateResultLock.Lock()
	defer c.updateResultLock.Unlock()
	c.reusedWorkflowJobResults[jobID] = result
}
