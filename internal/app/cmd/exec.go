// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2019 nektos
// SPDX-License-Identifier: MIT

package cmd

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/artifactcache"
	"gitea.com/gitea/runner/act/artifacts"
	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/runner"
	"gitea.com/gitea/runner/internal/app/run"
	"gitea.com/gitea/runner/internal/pkg/config"

	"gitea.dev/actionslib/pkg/model"
	"github.com/joho/godotenv"
	"github.com/moby/moby/api/types/container"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type executeArgs struct {
	runList               bool
	job                   string
	event                 string
	eventpath             string
	workdir               string
	workflowsPath         string
	noWorkflowRecurse     bool
	autodetectEvent       bool
	forcePull             bool
	forceRebuild          bool
	jsonLogger            bool
	inputs                []string
	inputfile             string
	envs                  []string
	envfile               string
	secrets               []string
	vars                  []string
	defaultActionsURL     string
	insecureSecrets       bool
	privileged            bool
	usernsMode            string
	containerArchitecture string
	containerDaemonSocket string
	useGitIgnore          bool
	containerCapAdd       []string
	containerCapDrop      []string
	containerOptions      string
	artifactServerPath    string
	artifactServerAddr    string
	artifactServerPort    string
	noSkipCheckout        bool
	debug                 bool
	dryrun                bool
	image                 string
	cacheHandler          *artifactcache.Handler
	network               string
	githubInstance        string
	toolCacheMode         string
}

// sharedToolCache reports whether mode mounts one tool cache for every job.
func sharedToolCache(mode string) (bool, error) {
	if !slices.Contains(config.ToolCacheModes, mode) {
		return false, fmt.Errorf("invalid --tool-cache-mode %q: must be one of %q", mode, config.ToolCacheModes)
	}
	return mode == config.ToolCacheModeShared, nil
}

// WorkflowsPath returns path to workflow file(s)
func (i *executeArgs) WorkflowsPath() string {
	return i.resolve(i.workflowsPath)
}

// Envfile returns path to .env
func (i *executeArgs) Envfile() string {
	return i.resolve(i.envfile)
}

// Inputfile returns path to .env-format inputfile
func (i *executeArgs) Inputfile() string {
	return i.resolve(i.inputfile)
}

func (i *executeArgs) LoadSecrets() map[string]string {
	s := make(map[string]string)
	for _, secretPair := range i.secrets {
		secretPairParts := strings.SplitN(secretPair, "=", 2)
		secretPairParts[0] = strings.ToUpper(secretPairParts[0])
		if strings.EqualFold(s[secretPairParts[0]], secretPairParts[0]) {
			log.Errorf("Secret %s is already defined (secrets are case insensitive)", secretPairParts[0])
		}
		if len(secretPairParts) == 2 {
			s[secretPairParts[0]] = secretPairParts[1]
		} else if env, ok := os.LookupEnv(secretPairParts[0]); ok && env != "" {
			s[secretPairParts[0]] = env
		} else {
			fmt.Printf("Provide value for '%s': ", secretPairParts[0])
			val, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				log.Errorf("failed to read input: %v", err)
				os.Exit(1)
			}
			s[secretPairParts[0]] = string(val)
		}
	}
	return s
}

func readEnvs(path string, envs map[string]string) bool {
	if _, err := os.Stat(path); err == nil {
		env, err := godotenv.Read(path)
		if err != nil {
			log.Fatalf("Error loading from %s: %v", path, err)
		}
		maps.Copy(envs, env)
		return true
	}
	return false
}

func (i *executeArgs) LoadVars() map[string]string {
	return parseKVAndFile(i.vars, "")
}

func (i *executeArgs) LoadInputs() map[string]string {
	return parseKVAndFile(i.inputs, i.Inputfile())
}

// eventJSON assembles the payload the run is triggered with, the `--input` values overriding
// the inputs the `--eventpath` file carries.
func (i *executeArgs) eventJSON() (string, error) {
	payload := []byte("{}")
	if path := i.resolve(i.eventpath); path != "" {
		var err error
		if payload, err = os.ReadFile(path); err != nil {
			return "", fmt.Errorf("failed to read %s: %w", path, err)
		}
	}

	cliInputs := i.LoadInputs()
	if len(cliInputs) == 0 {
		return string(payload), nil
	}

	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return "", fmt.Errorf("failed to parse event payload: %w", err)
	}
	if event == nil { // a JSON `null` payload unmarshals into a nil map
		event = make(map[string]any)
	}
	inputs, ok := event["inputs"].(map[string]any)
	if !ok {
		inputs = make(map[string]any)
	}
	for name, value := range cliInputs {
		inputs[name] = value
	}
	event["inputs"] = inputs

	merged, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("failed to marshal event payload: %w", err)
	}
	return string(merged), nil
}

func (i *executeArgs) LoadEnvs() map[string]string {
	envs := parseKVAndFile(i.envs, i.Envfile())

	envs["ACTIONS_CACHE_URL"] = i.cacheHandler.ExternalURL() + "/"
	// The same server answers cache v2, which docker buildx reads from the results origin alone.
	if envs["ACTIONS_RESULTS_URL"] == "" {
		envs["ACTIONS_RESULTS_URL"] = cmp.Or(os.Getenv("ACTIONS_RESULTS_URL"), i.cacheHandler.ExternalURL())
	}
	envs[runner.CacheServiceV2Env] = "true"

	return envs
}

func parseKVAndFile(rawKVs []string, filePath string) map[string]string {
	result := make(map[string]string)
	_ = readEnvs(filePath, result)

	for _, raw := range rawKVs {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		} else {
			result[parts[0]] = ""
		}
	}
	return result
}

// Workdir returns path to workdir
func (i *executeArgs) Workdir() string {
	return i.resolve(".")
}

func (i *executeArgs) resolve(path string) string {
	basedir, err := filepath.Abs(i.workdir)
	if err != nil {
		log.Fatal(err)
	}
	if path == "" {
		return path
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(basedir, path)
	}
	return path
}

func printList(plan *model.Plan) {
	type lineInfoDef struct {
		jobID   string
		jobName string
		stage   string
		wfName  string
		wfFile  string
		events  string
	}
	lineInfos := []lineInfoDef{}

	header := lineInfoDef{
		jobID:   "Job ID",
		jobName: "Job name",
		stage:   "Stage",
		wfName:  "Workflow name",
		wfFile:  "Workflow file",
		events:  "Events",
	}

	jobs := map[string]bool{}
	duplicateJobIDs := false

	jobIDMaxWidth := len(header.jobID)
	jobNameMaxWidth := len(header.jobName)
	stageMaxWidth := len(header.stage)
	wfNameMaxWidth := len(header.wfName)
	wfFileMaxWidth := len(header.wfFile)
	eventsMaxWidth := len(header.events)

	for i, stage := range plan.Stages {
		for _, r := range stage.Runs {
			jobID := r.JobID
			line := lineInfoDef{
				jobID:   jobID,
				jobName: r.String(),
				stage:   strconv.Itoa(i),
				wfName:  r.Workflow.Name,
				wfFile:  r.Workflow.File,
				events:  strings.Join(r.Workflow.On(), `,`),
			}
			if _, ok := jobs[jobID]; ok {
				duplicateJobIDs = true
			} else {
				jobs[jobID] = true
			}
			lineInfos = append(lineInfos, line)
			if jobIDMaxWidth < len(line.jobID) {
				jobIDMaxWidth = len(line.jobID)
			}
			if jobNameMaxWidth < len(line.jobName) {
				jobNameMaxWidth = len(line.jobName)
			}
			if stageMaxWidth < len(line.stage) {
				stageMaxWidth = len(line.stage)
			}
			if wfNameMaxWidth < len(line.wfName) {
				wfNameMaxWidth = len(line.wfName)
			}
			if wfFileMaxWidth < len(line.wfFile) {
				wfFileMaxWidth = len(line.wfFile)
			}
			if eventsMaxWidth < len(line.events) {
				eventsMaxWidth = len(line.events)
			}
		}
	}

	jobIDMaxWidth += 2
	jobNameMaxWidth += 2
	stageMaxWidth += 2
	wfNameMaxWidth += 2
	wfFileMaxWidth += 2

	fmt.Printf("%*s%*s%*s%*s%*s%*s\n",
		-stageMaxWidth, header.stage,
		-jobIDMaxWidth, header.jobID,
		-jobNameMaxWidth, header.jobName,
		-wfNameMaxWidth, header.wfName,
		-wfFileMaxWidth, header.wfFile,
		-eventsMaxWidth, header.events,
	)
	for _, line := range lineInfos {
		fmt.Printf("%*s%*s%*s%*s%*s%*s\n",
			-stageMaxWidth, line.stage,
			-jobIDMaxWidth, line.jobID,
			-jobNameMaxWidth, line.jobName,
			-wfNameMaxWidth, line.wfName,
			-wfFileMaxWidth, line.wfFile,
			-eventsMaxWidth, line.events,
		)
	}
	if duplicateJobIDs {
		fmt.Print("\nDetected multiple jobs with the same job name, use `-W` to specify the path to the specific workflow.\n")
	}
}

func runExecList(planner model.WorkflowPlanner, execArgs *executeArgs) error {
	// plan with filtered jobs - to be used for filtering only
	var filterPlan *model.Plan

	// Determine the event name to be filtered
	var filterEventName string

	if len(execArgs.event) > 0 {
		log.Infof("Using chosed event for filtering: %s", execArgs.event)
		filterEventName = execArgs.event
	} else if execArgs.autodetectEvent {
		// collect all events from loaded workflows
		events := planner.GetEvents()

		// set default event type to first event from many available
		// this way user dont have to specify the event.
		log.Infof("Using first detected workflow event for filtering: %s", events[0])

		filterEventName = events[0]
	}

	var err error
	switch {
	case execArgs.job != "":
		log.Infof("Preparing plan with a job: %s", execArgs.job)
		filterPlan, err = planner.PlanJob(execArgs.job)
		if err != nil {
			return err
		}
	case filterEventName != "":
		log.Infof("Preparing plan for a event: %s", filterEventName)
		filterPlan, err = planner.PlanEvent(filterEventName)
		if err != nil {
			return err
		}
	default:
		log.Infof("Preparing plan with all jobs")
		filterPlan, err = planner.PlanAll()
		if err != nil {
			return err
		}
	}

	printList(filterPlan)

	return nil
}

func (i *executeArgs) runnerConfig(eventName string, env, proxyEnv map[string]string, maxLifetime time.Duration, sharedToolCache bool) (*runner.Config, error) {
	eventJSON, err := i.eventJSON()
	if err != nil {
		return nil, err
	}

	return &runner.Config{
		Workdir:               i.Workdir(),
		BindWorkdir:           false,
		ForcePull:             i.forcePull,
		ForceRebuild:          i.forceRebuild,
		JSONLogger:            i.jsonLogger,
		Env:                   env,
		ProxyEnv:              proxyEnv,
		Vars:                  i.LoadVars(),
		Secrets:               i.LoadSecrets(),
		InsecureSecrets:       i.insecureSecrets,
		Privileged:            i.privileged,
		UsernsMode:            i.usernsMode,
		ContainerArchitecture: i.containerArchitecture,
		ContainerDaemonSocket: i.containerDaemonSocket,
		UseGitIgnore:          i.useGitIgnore,
		GitHubInstance:        i.githubInstance,
		ContainerCapAdd:       i.containerCapAdd,
		ContainerCapDrop:      i.containerCapDrop,
		ContainerOptions:      i.containerOptions,
		ArtifactServerPath:    i.artifactServerPath,
		ArtifactServerPort:    i.artifactServerPort,
		ArtifactServerAddr:    i.artifactServerAddr,
		NoSkipCheckout:        i.noSkipCheckout,
		EventName:             eventName,
		EventJSON:             eventJSON,
		// PresetGitHubContext:   preset,
		ContainerNamePrefix:               "GITEA-ACTIONS-TASK-" + eventName,
		ContainerMaxLifetime:              maxLifetime,
		ContainerNetworkMode:              container.NetworkMode(i.network),
		DefaultActionInstance:             i.defaultActionsURL,
		DefaultActionInstanceIsSelfHosted: i.defaultActionsURL != "" && i.defaultActionsURL != "https://github.com",
		PlatformPicker: func(_ []string) string {
			return i.image
		},
		ValidVolumes:    []string{"**"}, // All volumes are allowed for `exec` command
		SharedToolCache: sharedToolCache,
	}, nil
}

func runExec(ctx context.Context, execArgs *executeArgs) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		planner, err := model.NewWorkflowPlanner(execArgs.WorkflowsPath(), execArgs.noWorkflowRecurse)
		if err != nil {
			return err
		}

		if execArgs.runList {
			return runExecList(planner, execArgs)
		}

		// plan with triggered jobs
		var plan *model.Plan

		// Determine the event name to be triggered
		var eventName string

		// collect all events from loaded workflows
		events := planner.GetEvents()

		switch {
		case len(execArgs.event) > 0:
			log.Infof("Using chosed event for filtering: %s", execArgs.event)
			eventName = execArgs.event
		case len(events) == 1 && len(events[0]) > 0:
			log.Infof("Using the only detected workflow event: %s", events[0])
			eventName = events[0]
		case execArgs.autodetectEvent && len(events) > 0 && len(events[0]) > 0:
			// set default event type to first event from many available
			// this way user dont have to specify the event.
			log.Infof("Using first detected workflow event: %s", events[0])
			eventName = events[0]
		default:
			log.Infof("Using default workflow event: push")
			eventName = "push"
		}

		// build the plan for this run
		if execArgs.job != "" {
			log.Infof("Planning job: %s", execArgs.job)
			plan, err = planner.PlanJob(execArgs.job)
			if err != nil {
				return err
			}
		} else {
			log.Infof("Planning jobs for event: %s", eventName)
			plan, err = planner.PlanEvent(eventName)
			if err != nil {
				return err
			}
		}

		maxLifetime := 3 * time.Hour
		if deadline, ok := ctx.Deadline(); ok {
			maxLifetime = time.Until(deadline)
		}

		// init a cache server
		handler, err := artifactcache.StartHandler(artifactcache.Options{
			Policy: run.CachePolicy(&config.Config{Cache: config.DefaultCache()}),
			Logger: log.StandardLogger().WithField("module", "cache_request"),
		})
		if err != nil {
			return err
		}
		log.Infof("cache handler listens on: %v", handler.ExternalURL())
		execArgs.cacheHandler = handler

		if len(execArgs.artifactServerAddr) == 0 {
			ip := common.GetOutboundIP()
			if ip == nil {
				return errors.New("unable to determine outbound IP address")
			}
			execArgs.artifactServerAddr = ip.String()
		}

		if len(execArgs.artifactServerPath) == 0 {
			tempDir, err := os.MkdirTemp("", "gitea-act-")
			if err != nil {
				fmt.Println(err)
			}
			defer os.RemoveAll(tempDir)

			execArgs.artifactServerPath = tempDir
		}

		// Register ACTIONS_RUNTIME_TOKEN against local cache server
		env := execArgs.LoadEnvs()
		const actionsRuntimeTokenEnvName = "ACTIONS_RUNTIME_TOKEN"
		actionsRuntimeToken := env[actionsRuntimeTokenEnvName]
		if actionsRuntimeToken == "" {
			actionsRuntimeToken = os.Getenv(actionsRuntimeTokenEnvName)
		}
		if actionsRuntimeToken == "" {
			tmpBranch := make([]byte, 12)
			if _, err := rand.Read(tmpBranch); err != nil {
				actionsRuntimeToken = "token"
			} else {
				actionsRuntimeToken = hex.EncodeToString(tmpBranch)
			}
			env[actionsRuntimeTokenEnvName] = actionsRuntimeToken
			os.Setenv(actionsRuntimeTokenEnvName, actionsRuntimeToken)
		}
		handler.RegisterJob(actionsRuntimeToken, artifactcache.JobCredential{Repo: "__local/__exec"})

		// no service aliases: exec builds one config for the whole plan
		run.BypassProxyForDockerHost(os.Getenv("DOCKER_HOST"))
		proxyEnv := run.JobProxyEnv(env, env["ACTIONS_CACHE_URL"], nil)
		maps.Copy(env, proxyEnv)

		shared, err := sharedToolCache(execArgs.toolCacheMode)
		if err != nil {
			return err
		}

		// run the plan
		config, err := execArgs.runnerConfig(eventName, env, proxyEnv, maxLifetime, shared)
		if err != nil {
			return err
		}

		config.Env["ACT_EXEC"] = "true"
		config.Secrets[actionsRuntimeTokenEnvName] = actionsRuntimeToken

		if t := config.Secrets["GITEA_TOKEN"]; t != "" {
			config.Token = t
		} else if t := config.Secrets["GITHUB_TOKEN"]; t != "" {
			config.Token = t
		}

		if !execArgs.debug {
			logLevel := log.InfoLevel
			config.JobLoggerLevel = &logLevel
		}

		r, err := runner.New(config)
		if err != nil {
			return err
		}

		artifactCancel := artifacts.Serve(ctx, execArgs.artifactServerPath, execArgs.artifactServerAddr, execArgs.artifactServerPort)
		log.Debugf("artifacts server started at %s:%s", execArgs.artifactServerPath, execArgs.artifactServerPort)

		ctx = common.WithDryrun(ctx, execArgs.dryrun)
		executor := r.NewPlanExecutor(plan).Finally(func(ctx context.Context) error {
			artifactCancel()
			return nil
		})

		return executor(ctx)
	}
}

func loadExecCmd(ctx context.Context) *cobra.Command {
	execArg := executeArgs{}

	execCmd := &cobra.Command{
		Use:   "exec",
		Short: "Run workflow locally.",
		Args:  cobra.MaximumNArgs(20),
		RunE:  runExec(ctx, &execArg),
	}

	execCmd.Flags().BoolVarP(&execArg.runList, "list", "l", false, "list workflows")
	execCmd.Flags().StringVarP(&execArg.job, "job", "j", "", "run a specific job ID; when several workflow files define that job, also pass --workflows/-W to select the file")
	execCmd.Flags().StringVarP(&execArg.event, "event", "E", "", "run a event name")
	execCmd.Flags().StringVarP(&execArg.eventpath, "eventpath", "e", "", "path to a JSON event payload file exposed as the event that triggered the workflow")
	execCmd.PersistentFlags().StringVarP(&execArg.workflowsPath, "workflows", "W", "./.gitea/workflows/", "path to workflow file(s)")
	execCmd.PersistentFlags().StringVarP(&execArg.workdir, "directory", "C", ".", "working directory")
	execCmd.PersistentFlags().BoolVarP(&execArg.noWorkflowRecurse, "no-recurse", "", false, "Flag to disable running workflows from subdirectories of specified path in '--workflows'/'-W' flag")
	execCmd.Flags().BoolVarP(&execArg.autodetectEvent, "detect-event", "", false, "Use first event type from workflow as event that triggered the workflow")
	execCmd.Flags().BoolVarP(&execArg.forcePull, "pull", "p", false, "pull docker image(s) even if already present")
	execCmd.Flags().BoolVarP(&execArg.forceRebuild, "rebuild", "", false, "rebuild local action docker image(s) even if already present")
	execCmd.PersistentFlags().BoolVar(&execArg.jsonLogger, "json", false, "Output logs in json format")
	execCmd.Flags().StringArrayVarP(&execArg.inputs, "input", "", []string{}, "set an input the workflow declares under its workflow_dispatch or workflow_call trigger, others stay invisible to the inputs context (e.g. --input name=bar; can be specified multiple times with highest precedence)")
	execCmd.Flags().StringVarP(&execArg.inputfile, "input-file", "", "", "path to an .env-format file containing key=value pairs as baseline workflow inputs (override event inputs)")
	execCmd.Flags().StringArrayVarP(&execArg.envs, "env", "", []string{}, "env to make available to actions with optional value (e.g. --env myenv=foo or --env myenv; override env-file)")
	execCmd.PersistentFlags().StringVarP(&execArg.envfile, "env-file", "", ".env", "environment file to read and use as env in the containers")
	execCmd.Flags().StringArrayVarP(&execArg.secrets, "secret", "s", []string{}, "secret to make available to actions with optional value (e.g. -s mysecret=foo or -s mysecret)")
	execCmd.Flags().StringArrayVarP(&execArg.vars, "var", "", []string{}, "variable to make available to actions with optional value (e.g. --var myvar=foo or --var myvar)")
	execCmd.PersistentFlags().BoolVarP(&execArg.insecureSecrets, "insecure-secrets", "", false, "NOT RECOMMENDED! Doesn't hide secrets while printing logs.")
	execCmd.Flags().BoolVar(&execArg.privileged, "privileged", false, "use privileged mode")
	execCmd.Flags().StringVar(&execArg.usernsMode, "userns", "", "user namespace to use")
	execCmd.PersistentFlags().StringVarP(&execArg.containerArchitecture, "container-architecture", "", "", "Architecture which should be used to run containers, e.g.: linux/amd64. If not specified, will use host default architecture. Requires Docker server API Version 1.41+. Ignored on earlier Docker server platforms.")
	execCmd.PersistentFlags().StringVarP(&execArg.containerDaemonSocket, "container-daemon-socket", "", "/var/run/docker.sock", "Path to Docker daemon socket which will be mounted to containers")
	execCmd.Flags().BoolVar(&execArg.useGitIgnore, "use-gitignore", true, "Controls whether paths specified in .gitignore should be copied into container")
	execCmd.Flags().StringArrayVarP(&execArg.containerCapAdd, "container-cap-add", "", []string{}, "kernel capabilities to add to the workflow containers (e.g. --container-cap-add SYS_PTRACE)")
	execCmd.Flags().StringArrayVarP(&execArg.containerCapDrop, "container-cap-drop", "", []string{}, "kernel capabilities to remove from the workflow containers (e.g. --container-cap-drop SYS_PTRACE)")
	execCmd.Flags().StringVarP(&execArg.containerOptions, "container-opts", "", "", "container options")
	execCmd.PersistentFlags().StringVarP(&execArg.artifactServerPath, "artifact-server-path", "", ".", "Defines the path where the artifact server stores uploads and retrieves downloads from. If not specified the artifact server will not start.")
	execCmd.PersistentFlags().StringVarP(&execArg.artifactServerAddr, "artifact-server-addr", "", "", "Defines the address where the artifact server listens")
	execCmd.PersistentFlags().StringVarP(&execArg.artifactServerPort, "artifact-server-port", "", "34567", "Defines the port where the artifact server listens (will only bind to localhost).")
	execCmd.PersistentFlags().StringVarP(&execArg.defaultActionsURL, "default-actions-url", "", "https://github.com", "Defines the default url of action instance.")
	execCmd.PersistentFlags().BoolVarP(&execArg.noSkipCheckout, "no-skip-checkout", "", false, "Do not skip actions/checkout")
	execCmd.PersistentFlags().BoolVarP(&execArg.debug, "debug", "d", false, "enable debug log")
	execCmd.PersistentFlags().BoolVarP(&execArg.dryrun, "dryrun", "n", false, "dryrun mode")
	execCmd.PersistentFlags().StringVarP(&execArg.image, "image", "i", config.DefaultImage, "Docker image to use. Use \"-self-hosted\" to run directly on the host.")
	execCmd.PersistentFlags().StringVarP(&execArg.toolCacheMode, "tool-cache-mode", "", config.ToolCacheModeNone, "What to mount at RUNNER_TOOL_CACHE: none, or shared to reuse one tool cache across runs")
	execCmd.PersistentFlags().StringVarP(&execArg.network, "network", "", "", "Specify the network to which the container will connect")
	execCmd.PersistentFlags().StringVarP(&execArg.githubInstance, "gitea-instance", "", "", "Gitea instance to use.")

	return execCmd
}
