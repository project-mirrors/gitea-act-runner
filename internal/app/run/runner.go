// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.com/gitea/runner/act/artifactcache"
	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/act/runner"
	"gitea.com/gitea/runner/internal/pkg/client"
	"gitea.com/gitea/runner/internal/pkg/config"
	"gitea.com/gitea/runner/internal/pkg/disk"
	"gitea.com/gitea/runner/internal/pkg/envcheck"
	"gitea.com/gitea/runner/internal/pkg/labels"
	"gitea.com/gitea/runner/internal/pkg/metrics"
	"gitea.com/gitea/runner/internal/pkg/report"
	"gitea.com/gitea/runner/internal/pkg/telemetry"
	"gitea.com/gitea/runner/internal/pkg/ver"

	"connectrpc.com/connect"
	"gitea.dev/actionslib/pkg/model"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	docker_container "github.com/moby/moby/api/types/container"
	log "github.com/sirupsen/logrus"
	"go.yaml.in/yaml/v4"
)

// CapabilityCancelling tells the server this runner understands the
// transitional cancelling state and will run post-step cleanup before
// finalizing a task as RESULT_CANCELLED.
const CapabilityCancelling = "cancelling"

// RunnerCapabilities are the capability flags this runner advertises to the
// server during registration and declaration. The server uses them to enable
// transitional features that require runner-side support.
func RunnerCapabilities() []string {
	return []string{CapabilityCancelling}
}

// Runner runs the pipeline.
type Runner struct {
	name string
	uuid string

	cfg *config.Config

	client       client.Client
	labels       labels.Labels
	envs         map[string]string
	cacheHandler *artifactcache.Handler
	capabilities string

	isolatedCacheNetwork func() string

	runningTasks            sync.Map
	runningCount            atomic.Int64
	lastIdleCleanupUnixNano atomic.Int64
	now                     func() time.Time
	healthCheckLast         time.Time
	healthCheckReady        bool
	healthCheckReason       string
	healthStatusSet         bool
	healthStatusReady       bool
	healthStatusReason      string
	runHealthCheck          func(context.Context, string, time.Duration, map[string]string) error
}

func NewRunner(cfg *config.Config, reg *config.Registration, cli client.Client) *Runner {
	ls := labels.Labels{}
	for _, v := range reg.Labels {
		if l, err := labels.Parse(v); err == nil {
			ls = append(ls, l)
		}
	}
	envs := make(map[string]string, len(cfg.Runner.Envs))
	maps.Copy(envs, cfg.Runner.Envs)
	var cacheHandler *artifactcache.Handler
	if cacheEnabled(cfg) {
		if cfg.Cache.ExternalServer != "" {
			warnIgnoredCachePolicy(cfg)
			// The v1 client appends its path to this without a separator, so the slash is required.
			envs["ACTIONS_CACHE_URL"] = strings.TrimRight(cfg.Cache.ExternalServer, "/") + "/"
		} else {
			warnIgnoredCacheSecret(cfg)
			handler, err := artifactcache.StartHandler(artifactcache.Options{
				Dir:        cfg.Cache.Dir,
				OutboundIP: cfg.Cache.Host,
				Port:       cfg.Cache.Port,
				Policy:     CachePolicy(cfg),
				Logger:     log.StandardLogger().WithField("module", "cache_request"),
			})
			if err != nil {
				log.Errorf("cannot init cache server, it will be disabled: %v", err)
				// go on
			} else {
				cacheHandler = handler
				envs["ACTIONS_CACHE_URL"] = handler.ExternalURL() + "/"
			}
		}
	}

	// set artifact gitea api
	artifactGiteaAPI := strings.TrimSuffix(cli.Address(), "/") + "/api/actions_pipeline/"
	envs["ACTIONS_RUNTIME_URL"] = artifactGiteaAPI
	envs["ACTIONS_RESULTS_URL"] = strings.TrimSuffix(cli.Address(), "/")

	// Set specific environments to distinguish between Gitea and GitHub
	envs["GITEA_ACTIONS"] = "true"
	envs["GITEA_ACTIONS_RUNNER_VERSION"] = ver.Version()

	runner := &Runner{
		name:           reg.Name,
		uuid:           reg.UUID,
		cfg:            cfg,
		client:         cli,
		labels:         ls,
		envs:           envs,
		cacheHandler:   cacheHandler,
		now:            time.Now,
		runHealthCheck: executeHealthCheck,
	}
	runner.isolatedCacheNetwork = sync.OnceValue(runner.detectIsolatedCacheNetwork)
	return runner
}

// Close shuts down the cache server this runner exposes to job containers.
func (r *Runner) Close() error {
	return r.cacheHandler.Close()
}

// removeOrphanNetworks is a variable so tests can substitute one that needs no Docker daemon.
var removeOrphanNetworks = container.RemoveOrphanNetworks

var removeOrphanJobVolumes = container.RemoveOrphanJobVolumes

// OnIdle performs lightweight maintenance during polling idle windows.
// It runs synchronously on the poller goroutine; shouldRunIdleCleanup
// throttles invocations to runner.idle_cleanup_interval so the impact on
// poll cadence is bounded even when the workdir root is large.
func (r *Runner) OnIdle(ctx context.Context) {
	if !r.shouldRunIdleCleanup() {
		return
	}
	// Bind-workdir mode: reclaim stale per-task workspace dirs (numeric task IDs).
	if r.cfg.Container.BindWorkdir {
		workdirParent := strings.TrimLeft(r.cfg.Container.WorkdirParent, "/")
		workdirRoot := filepath.FromSlash("/" + workdirParent)
		r.cleanupStaleDirs(ctx, workdirRoot, isTaskIDDir)
	}
	// Host mode: reclaim per-job scratch dirs left behind when HostEnvironment
	// cleanup timed out (e.g. a delete stalled by an AV/EDR filter driver). They
	// sit under the host workdir parent next to a shared tool_cache, which the name
	// match leaves untouched. No-op when no host-mode job ever ran.
	if hostRoot := filepath.FromSlash(r.cfg.Host.WorkdirParent); hostRoot != "" {
		r.cleanupStaleDirs(ctx, hostRoot, isHostScratchDir)
	}
	r.cleanupOrphanDockerResources(ctx)
}

func (r *Runner) cleanupOrphanDockerResources(ctx context.Context) {
	if r.uuid == "" || (!r.requiresDocker() && !dockerReachable(ctx)) {
		return
	}
	cutoff := r.now().Add(-r.cfg.Runner.WorkdirCleanupAge)
	if err := removeOrphanNetworks(ctx, r.uuid, cutoff); err != nil {
		log.Warnf("failed to clean up networks left behind by earlier jobs: %v", err)
	}
	if err := removeOrphanJobVolumes(ctx, r.uuid, cutoff); err != nil {
		log.Warnf("failed to clean up volumes left behind by earlier jobs: %v", err)
	}
}

func (r *Runner) shouldRunIdleCleanup() bool {
	if r.cfg.Runner.WorkdirCleanupAge <= 0 || r.cfg.Runner.IdleCleanupInterval <= 0 {
		return false
	}
	if r.RunningCount() != 0 {
		return false
	}
	now := r.now()
	interval := r.cfg.Runner.IdleCleanupInterval
	for {
		last := r.lastIdleCleanupUnixNano.Load()
		if last != 0 && now.Sub(time.Unix(0, last)) < interval {
			return false
		}
		if r.lastIdleCleanupUnixNano.CompareAndSwap(last, now.UnixNano()) {
			return true
		}
	}
}

// isTaskIDDir reports whether name is a per-task workspace dir (numeric task
// ID). Any other directory is skipped to avoid deleting operator-managed data
// under workdir_root.
func isTaskIDDir(name string) bool {
	_, err := strconv.ParseUint(name, 10, 64)
	return err == nil
}

// isHostScratchDir reports whether name is a per-job host-mode scratch dir:
// hex.EncodeToString of 8 random bytes, i.e. exactly 16 lowercase hex chars
// (see startHostEnvironment in act/runner/run_context.go). The narrow match
// leaves a sibling shared "tool_cache" dir and any operator data untouched.
func isHostScratchDir(name string) bool {
	if len(name) != 16 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// cleanupStaleDirs removes immediate child directories of root that match and
// whose mtime is older than WorkdirCleanupAge. It is a no-op when root does not
// exist yet (the runner has never written there).
func (r *Runner) cleanupStaleDirs(ctx context.Context, root string, match func(name string) bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		log.Warnf("failed to list directory %s for stale cleanup: %v", root, err)
		return
	}

	// A task may begin between shouldRunIdleCleanup's running-count check and
	// the loop below. That is safe because new dirs are created with the
	// current mtime and therefore fall on the keep side of cutoff.
	cutoff := r.now().Add(-r.cfg.Runner.WorkdirCleanupAge)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		if !entry.IsDir() {
			continue
		}
		if !match(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			log.Warnf("failed to stat %s: %v", filepath.Join(root, entry.Name()), err)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if err := os.RemoveAll(dir); err != nil {
			log.Warnf("failed to clean stale directory %s: %v", dir, err)
			continue
		}
		log.Infof("cleaned stale directory %s", dir)
	}
}

func (r *Runner) SetCapabilitiesFromDeclare(resp *connect.Response[runnerv1.DeclareResponse]) {
	if resp == nil {
		return
	}
	// Capability negotiation is done via response headers to avoid a hard proto bump.
	r.capabilities = strings.TrimSpace(resp.Header().Get("X-Gitea-Actions-Capabilities"))
}

func (r *Runner) Run(ctx context.Context, task *runnerv1.Task) error {
	if _, ok := r.runningTasks.LoadOrStore(task.Id, struct{}{}); ok {
		return fmt.Errorf("task %d is already running", task.Id)
	}
	defer r.runningTasks.Delete(task.Id)

	r.runningCount.Add(1)
	defer r.runningCount.Add(-1)

	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Runner.Timeout)
	defer cancel()
	ctx, endJob := telemetry.StartJob(ctx, task)
	// A proxy URL may carry credentials, and every job is given it; keep them out of the log.
	reporter := report.NewReporter(ctx, cancel, r.client, task, r.cfg, proxyPasswords()...)
	var volumeCleanup []common.Executor
	var volumeCleanupMu sync.Mutex
	if r.cfg.Runner.PostTaskScript == "" {
		ctx = runner.WithJobVolumeCleanup(ctx, func(cleanup common.Executor) {
			volumeCleanupMu.Lock()
			defer volumeCleanupMu.Unlock()
			volumeCleanup = append(volumeCleanup, cleanup)
		})
	}
	var runErr error
	defer func() {
		lastWords := ""
		if runErr != nil {
			lastWords = runErr.Error()
		}
		_ = reporter.Close(lastWords)
		if err := cleanupJobVolumes(ctx, volumeCleanup); err != nil {
			log.Warnf("task %d volume cleanup after reporting: %v", task.Id, err)
		}

		metrics.JobDuration.Observe(time.Since(start).Seconds())
		metrics.JobsTotal.WithLabelValues(metrics.ResultToStatusLabel(reporter.Result())).Inc()
		endJob(reporter.Result())
	}()
	reporter.RunDaemon()
	runErr = r.run(ctx, task, reporter)

	return nil
}

func cleanupJobVolumes(ctx context.Context, cleanups []common.Executor) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	var errs []error
	for _, cleanup := range cleanups {
		errs = append(errs, cleanup(ctx))
	}
	return errors.Join(errs...)
}

func (r *Runner) cloneEnvs() map[string]string {
	// Reserve space for the per-task keys injected by run():
	// ACTIONS_ID_TOKEN_REQUEST_URL, ACTIONS_ID_TOKEN_REQUEST_TOKEN, ACTIONS_RUNTIME_TOKEN,
	// GITEA_ACTIONS_CAPABILITIES, GITEA_RUN_ID.
	envs := make(map[string]string, len(r.envs)+5)
	maps.Copy(envs, r.envs)
	return envs
}

// getDefaultActionsURL
// when DEFAULT_ACTIONS_URL == "https://github.com" and GithubMirror is not blank,
// it should be set to GithubMirror first.
func (r *Runner) getDefaultActionsURL(task *runnerv1.Task) string {
	giteaDefaultActionsURL := task.Context.Fields["gitea_default_actions_url"].GetStringValue()
	if giteaDefaultActionsURL == "https://github.com" && r.cfg.Runner.GithubMirror != "" {
		return r.cfg.Runner.GithubMirror
	}
	return giteaDefaultActionsURL
}

// isSelfHostedActionsURL reports whether actions resolve to this self-hosted Gitea
// (DEFAULT_ACTIONS_URL=self), i.e. gitea_default_actions_url is AppURL rather than
// github.com (which may be mirror-substituted by getDefaultActionsURL). Only then may the
// task token be attached to action clone URLs on the actions instance host.
func (r *Runner) isSelfHostedActionsURL(task *runnerv1.Task) bool {
	giteaDefaultActionsURL := task.Context.Fields["gitea_default_actions_url"].GetStringValue()
	return giteaDefaultActionsURL != "" && giteaDefaultActionsURL != "https://github.com"
}

// dockerReachable is a variable so tests can substitute one that needs no Docker daemon. It
// probes the environment act connects through, not container.docker_host, which act ignores.
var dockerReachable = func(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return envcheck.CheckIfDockerRunning(ctx, "") == nil
}

func (r *Runner) requiresDocker() bool {
	return r.labels.RequireDocker() || r.cfg.Container.RequireDocker
}

// fallbackPlatform is where a job runs whose runs-on matches no label, as any job without a
// runs-on does, since Gitea sends those to every runner.
func (r *Runner) fallbackPlatform(ctx context.Context) string {
	if r.requiresDocker() || dockerReachable(ctx) {
		return r.cfg.Runner.DefaultImage
	}
	return labels.SelfHostedPlatform
}

func (r *Runner) run(ctx context.Context, task *runnerv1.Task, reporter *report.Reporter) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	r.reportSetup(reporter, task)

	workflow, jobID, err := generateWorkflow(task)
	if err != nil {
		return err
	}

	plan, err := model.CombineWorkflowPlanner(workflow).PlanJob(jobID)
	if err != nil {
		return err
	}
	job := workflow.GetJob(jobID)
	reporter.ResetSteps(len(job.Steps))
	telemetry.SetJobName(ctx, cmp.Or(job.Name, jobID))

	taskContext := task.Context.Fields
	envs := r.cloneEnvs()

	// Added per task because this job's service containers must be reached directly, and
	// act reaches them by their workflow key.
	var services map[string]yaml.Node
	_ = job.RawServices.Decode(&services) // a whole-value expression names no service before evaluation
	proxyEnv := JobProxyEnv(envs, r.builtInCacheURL(), slices.Sorted(maps.Keys(services)))
	maps.Copy(envs, proxyEnv)

	if r.capabilities != "" {
		envs["GITEA_ACTIONS_CAPABILITIES"] = r.capabilities
	}
	if v := taskContext["run_id"].GetStringValue(); v != "" {
		envs["GITEA_RUN_ID"] = v
	}
	telemetry.AddJobEnv(ctx, envs)

	log.Infof("task %v repo is %v %v %v", task.Id, taskContext["repository"].GetStringValue(),
		r.getDefaultActionsURL(task),
		r.client.Address())

	preset := model.GithubContextFromMap(task.Context.AsMap())
	if t := task.Secrets["GITEA_TOKEN"]; t != "" {
		preset.Token = t
	} else if t := task.Secrets["GITHUB_TOKEN"]; t != "" {
		preset.Token = t
	}

	if actionsIDTokenRequestURL := taskContext["actions_id_token_request_url"].GetStringValue(); actionsIDTokenRequestURL != "" {
		envs["ACTIONS_ID_TOKEN_REQUEST_URL"] = actionsIDTokenRequestURL
		envs["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = taskContext["actions_id_token_request_token"].GetStringValue()
		task.Secrets["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = envs["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]
	}

	giteaRuntimeToken := taskContext["gitea_runtime_token"].GetStringValue()
	if giteaRuntimeToken == "" {
		// use task token to action api token for previous Gitea Server Versions
		giteaRuntimeToken = preset.Token
	}
	envs["ACTIONS_RUNTIME_TOKEN"] = giteaRuntimeToken
	// Mask the runtime token so it cannot be echoed in user step output; it is
	// now also the cache server's bearer credential and leaking it would let
	// any reader of the log impersonate this job against the cache.
	if giteaRuntimeToken != "" {
		task.Secrets["ACTIONS_RUNTIME_TOKEN"] = giteaRuntimeToken
	}

	// Register this job's runtime token with the local cache server so that
	// cache requests from the job container can authenticate. The credential
	// is removed when the task finishes, so a leaked token stops working as
	// soon as the job ends rather than remaining valid for the runner's
	// lifetime. Only applies to the embedded cache server; when the operator
	// points the runner at an external cache via cfg.Cache.ExternalServer, it
	// is that server's responsibility to authenticate requests.
	revokeCache, resultsURL := r.registerCacheForTask(giteaRuntimeToken, preset.Repository, reporter)
	defer revokeCache()

	eventJSON, err := json.Marshal(preset.Event)
	if err != nil {
		return err
	}

	maxLifetime := 3 * time.Hour
	if deadline, ok := ctx.Deadline(); ok {
		maxLifetime = time.Until(deadline)
	}

	// shallow clones the requested ref at depth 1, otherwise 0 means a full clone
	actionCloneDepth := 1
	if r.cfg.Runner.ActionShallowClone != nil && !*r.cfg.Runner.ActionShallowClone {
		actionCloneDepth = 0
	}

	workdirParent := strings.TrimLeft(r.cfg.Container.WorkdirParent, "/")
	if r.cfg.Container.BindWorkdir {
		// Append the task ID to isolate concurrent jobs from the same repo.
		workdirParent = fmt.Sprintf("%s/%d", workdirParent, task.Id)
	}
	workdir := filepath.FromSlash(fmt.Sprintf("/%s/%s", workdirParent, preset.Repository))
	if runtime.GOOS == "windows" {
		if abs, err := filepath.Abs(workdir); err == nil {
			workdir = abs
		}
	}
	// Without bind_workdir, the workspace path omits the task id; concurrent host-mode jobs
	// for the same repository would share this directory and can race with per-job cleanup.

	// act asks for the platform once per step, so resolve the fallback at most once per task.
	fallbackPlatform := sync.OnceValue(func() string { return r.fallbackPlatform(ctx) })
	platformPicker := func(runsOn []string) string {
		if platform := r.labels.PickPlatform(runsOn); platform != "" {
			return platform
		}
		return fallbackPlatform()
	}

	if resultsURL != "" && r.cacheIsolatedFrom(job, platformPicker) {
		reporter.Logf("::warning::%s", runner.EscapeCommandData(fmt.Sprintf("jobs cannot reach the cache server at %s on docker network %q, so caching fails and artifacts go to Gitea directly, set cache.host and cache.port to an address jobs reach, or container.network to %[2]q",
			r.cacheHandler.ExternalURL(), r.isolatedCacheNetwork())))
		resultsURL = ""
	}
	r.setResultsService(envs, resultsURL)

	runnerConfig := &runner.Config{
		// On Linux, Workdir will be like "/<parent_directory>/<owner>/<repo>"
		// On Windows, Workdir will be like "\<parent_directory>\<owner>\<repo>"
		Workdir:           workdir,
		BindWorkdir:       r.cfg.Container.BindWorkdir,
		ActionCacheDir:    filepath.FromSlash(r.cfg.Host.WorkdirParent),
		AllocatePTY:       r.cfg.Runner.AllocatePTY,
		ActionOfflineMode: r.cfg.Cache.OfflineMode,
		ActionCloneDepth:  actionCloneDepth,
		NoActionPatch:     r.cfg.Runner.PatchActions != nil && !*r.cfg.Runner.PatchActions,

		ForcePull:            r.cfg.Container.ForcePull,
		ForceRebuild:         r.cfg.Container.ForceRebuild,
		JSONLogger:           false,
		Env:                  envs,
		ProxyEnv:             proxyEnv,
		Secrets:              task.Secrets,
		ExtraMasks:           append(proxyPasswords(), preset.Token),
		GitHubInstance:       strings.TrimSuffix(r.client.Address(), "/"),
		NoSkipCheckout:       true,
		DisableActEnv:        r.cfg.Runner.SetActEnv != nil && !*r.cfg.Runner.SetActEnv,
		PresetGitHubContext:  preset,
		EventJSON:            string(eventJSON),
		ContainerNamePrefix:  fmt.Sprintf("GITEA-ACTIONS-TASK-%d", task.Id),
		ContainerMaxLifetime: maxLifetime,
		CleanWorkdir:         true,
		ContainerNetworkMode: docker_container.NetworkMode(r.cfg.Container.Network),
		ContainerNetworkCreateOptions: container.NewDockerNetworkCreateExecutorInput{
			EnableIPv4: r.cfg.Container.NetworkCreateOptions.EnableIPv4,
			EnableIPv6: r.cfg.Container.NetworkCreateOptions.EnableIPv6,
			// so a network this job leaks, if the runner dies before its teardown, can be
			// told apart from one belonging to another runner on the same daemon
			RunnerUUID: r.uuid,
		},
		ContainerOptions:                  r.cfg.Container.Options,
		ServiceReadyTimeout:               r.cfg.Container.ServiceReadyTimeout,
		ContainerDaemonSocket:             r.cfg.Container.DockerHost,
		Privileged:                        r.cfg.Container.Privileged,
		DefaultActionInstance:             r.getDefaultActionsURL(task),
		DefaultActionInstanceIsSelfHosted: r.isSelfHostedActionsURL(task),
		PlatformPicker:                    platformPicker,
		JobStartedHook:                    r.cfg.Runner.Hooks.JobStarted,
		JobCompletedHook:                  r.cfg.Runner.Hooks.JobCompleted,
		Vars:                              task.Vars,
		ValidVolumes:                      r.cfg.Container.ValidVolumes,
		SharedToolCache:                   r.cfg.Runner.ToolCacheMode == config.ToolCacheModeShared,
		InsecureSkipTLS:                   r.cfg.Runner.Insecure,
		RunnerName:                        r.name,
	}

	rr, err := runner.New(runnerConfig)
	if err != nil {
		return err
	}
	executor := rr.NewPlanExecutor(plan)

	reporter.Logf("workflow prepared")

	// add logger recorders
	ctx = common.WithLoggerHook(ctx, reporter)

	if !log.IsLevelEnabled(log.DebugLevel) {
		ctx = runner.WithJobLoggerFactory(ctx, NullLogger{})
	}

	execErr := executor(ctx)
	reporter.SetOutputs(job.Outputs)

	if r.cfg.Container.BindWorkdir {
		// Remove the entire task-specific directory (e.g. /workspace/<task_id>).
		taskDir := filepath.FromSlash("/" + workdirParent)
		if err := os.RemoveAll(taskDir); err != nil {
			log.Warnf("failed to clean up workspace %s: %v", taskDir, err)
		}
	}

	reporter.StopHeartbeats()
	r.runPostTaskScript(ctx, reporter, task, workdir)

	return execErr
}

func (r *Runner) builtInCacheURL() string {
	if r.cacheHandler == nil {
		return ""
	}
	return r.envs["ACTIONS_CACHE_URL"]
}

func cacheEnabled(cfg *config.Config) bool {
	return cfg.Cache.Enabled == nil || *cfg.Cache.Enabled
}

// cacheServiceV2 reports whether jobs are told the cache service speaks v2. It is all cache.v2
// turns off: the bundle edit that reaches it is what the artifact actions need too.
func (r *Runner) cacheServiceV2() bool {
	return r.cfg.Cache.V2 == nil || *r.cfg.Cache.V2
}

func (r *Runner) setResultsService(envs map[string]string, resultsURL string) {
	v2 := resultsURL != "" && r.cacheServiceV2()
	if v2 {
		envs[runner.CacheServiceV2Env] = "true"
	} else {
		delete(envs, runner.CacheServiceV2Env)
	}
	// Cache v2 clients read no other variable for it, and some cannot use the instance address.
	if v2 || (resultsURL != "" && r.instanceOutOfReach(envs["ACTIONS_RESULTS_URL"])) {
		envs["ACTIONS_RESULTS_URL"] = resultsURL
	}
}

// These clients keep only the origin they are handed, and reach it on the job's own trust.
func (r *Runner) instanceOutOfReach(instance string) bool {
	parsed, err := url.Parse(instance)
	return err != nil || strings.Trim(parsed.Path, "/") != "" ||
		(r.cfg.Runner.Insecure && parsed.Scheme == "https")
}

// registerCacheForTask tells the cache server to accept requests authenticated
// with the given runtime token for the duration of this task. Returns a
// function the caller must invoke (typically via defer) to revoke the
// credential when the task finishes.
//
// Two modes:
//   - Embedded handler: register in-process via RegisterJob.
//   - external_server: POST to the remote server's /_internal/register, defer a
//     POST to /_internal/revoke. This is what enables full per-job auth and
//     repo scoping over the network.
//
// Safe with an empty token (older Gitea did not issue one).
// It also returns the URL to advertise as ACTIONS_RESULTS_URL, which is the cache server itself
// when it agreed to forward this instance's artifact service, and "" when it did not.
func (r *Runner) registerCacheForTask(token, repo string, reporter *report.Reporter) (func(), string) {
	if token == "" {
		return func() {}, ""
	}
	cred := artifactcache.JobCredential{
		Repo:        repo,
		Results:     r.envs["ACTIONS_RESULTS_URL"], // the instance as the job would reach it
		InsecureTLS: r.cfg.Runner.Insecure,
	}
	if r.cacheHandler != nil {
		return r.cacheHandler.RegisterJob(token, cred), r.cacheHandler.ResultsURL(cred)
	}
	if cacheEnabled(r.cfg) && r.cfg.Cache.ExternalServer != "" && r.cfg.Cache.ExternalSecret != "" {
		return r.registerExternalCacheJob(token, cred, reporter)
	}
	// No cache server to register against: caching is disabled, or the built-in server failed to start.
	return func() {}, ""
}

// registerExternalCacheJob POSTs to the remote cache-server's control-plane.
// Failures are logged but not fatal: if registration fails, the cache will
// 401 the job's requests — better than failing the whole task for a cache
// outage. The warning is mirrored to the job log so users can see why their
// cache calls 401, instead of having to read the runner daemon's stderr.
func (r *Runner) registerExternalCacheJob(token string, cred artifactcache.JobCredential, reporter *report.Reporter) (func(), string) {
	base := strings.TrimRight(r.cfg.Cache.ExternalServer, "/")
	resultsURL := ""
	if body, err := postInternalCache(base+"/_internal/register", r.cfg.Cache.ExternalSecret, map[string]any{
		"token": token, "repo": cred.Repo, "results": cred.Results, "insecure_tls": cred.InsecureTLS,
		"public_url": base,
	}); err != nil {
		log.Warnf("cache external_server register failed (%s): %v", base, err)
		if reporter != nil {
			reporter.Logf("::warning::%s", runner.EscapeCommandData(fmt.Sprintf(
				"cache external_server register failed (%s): %v — cache requests from this job will be unauthenticated and likely return 401", base, err)))
		}
	} else if forwarded, _ := body["results_url"].(string); forwarded != "" {
		resultsURL = base // the answer only says it forwards, its own address need not be the job's
	}
	return func() {
		if _, err := postInternalCache(base+"/_internal/revoke", r.cfg.Cache.ExternalSecret,
			map[string]any{"token": token}); err != nil {
			log.Warnf("cache external_server revoke failed (%s): %v", base, err)
			if reporter != nil {
				reporter.Logf("::warning::%s", runner.EscapeCommandData(fmt.Sprintf(
					"cache external_server revoke failed (%s): %v", base, err)))
			}
		}
	}, resultsURL
}

func postInternalCache(url, secret string, body map[string]any) (map[string]any, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	answer := map[string]any{}
	// A server too old to answer with a body is not an error, it simply tells us nothing.
	_ = json.UnmarshalRead(resp.Body, &answer)
	return answer, nil
}

func (r *Runner) RunningCount() int64 {
	return r.runningCount.Load()
}

// CanAcceptTask checks local admission conditions without consuming a task. It is
// called only from the poll loop, so the cached health fields need no lock.
func (r *Runner) CanAcceptTask(ctx context.Context) (bool, string) {
	if !r.cfg.HealthCheck.Enabled {
		return true, "ok"
	}

	if r.RunningCount() > 0 {
		if !r.healthStatusSet {
			return true, "health checks deferred while jobs are running"
		}
		return r.healthStatusReady, r.healthStatusReason
	}

	if ready, reason := checkFreeDisk(r.cfg); !ready {
		r.setHealthStatus(ready, reason)
		return false, reason
	}
	ready, reason := r.checkConfiguredHealth(ctx)
	r.setHealthStatus(ready, reason)
	return ready, reason
}

func (r *Runner) setHealthStatus(ready bool, reason string) {
	r.healthStatusSet = true
	r.healthStatusReady = ready
	r.healthStatusReason = reason
}

// checkFreeDisk evaluates the configured task-admission disk threshold.
func checkFreeDisk(cfg *config.Config) (bool, string) {
	root := cfg.Host.WorkdirParent
	if cfg.Container.BindWorkdir {
		root = filepath.FromSlash("/" + strings.TrimLeft(cfg.Container.WorkdirParent, "/"))
	}
	root = nearestExistingPath(root)
	available, err := disk.FreeBytes(root)
	if err != nil {
		return false, fmt.Sprintf("cannot determine free disk space for %s: %v", root, err)
	}
	availableMB := available / (1024 * 1024)
	if availableMB < uint64(cfg.HealthCheck.MinFreeDiskSpaceMB) {
		return false, fmt.Sprintf("low disk space on %s: %d MiB available, %d MiB required", root, availableMB, cfg.HealthCheck.MinFreeDiskSpaceMB)
	}
	return true, "ok"
}

func nearestExistingPath(path string) string {
	for path != "" {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return parent
		}
		path = parent
	}
	return "."
}

func (r *Runner) Declare(ctx context.Context, labels []string) (*connect.Response[runnerv1.DeclareResponse], error) {
	return r.client.Declare(ctx, connect.NewRequest(&runnerv1.DeclareRequest{
		Version:      ver.Version(),
		Labels:       labels,
		Capabilities: RunnerCapabilities(),
	}))
}

// minFreeDisk keeps the cache from growing past the point where the runner stops taking
// work, but only when health checks are on, since the key is documented as opt-in.
func minFreeDisk(cfg *config.Config) int64 {
	if !cfg.HealthCheck.Enabled {
		return 0
	}
	return cfg.HealthCheck.MinFreeDiskSpaceMB * 1024 * 1024
}

// CachePolicy maps the cache config onto the cache server's own type, in bytes not MiB.
func CachePolicy(cfg *config.Config) artifactcache.Policy {
	return artifactcache.Policy{
		Retention:     cfg.Cache.Retention,
		RepoSizeLimit: int64(cfg.Cache.RepoSizeLimit),
		SizeLimit:     int64(cfg.Cache.SizeLimit),
		SweepInterval: cfg.Cache.SweepInterval,
		MinFreeDisk:   minFreeDisk(cfg),
	}
}

// warnIgnoredCachePolicy flags eviction settings configured on a runner that points at an external cache server.
func warnIgnoredCachePolicy(cfg *config.Config) {
	defaults := config.DefaultCache()
	if cfg.Cache.Retention != defaults.Retention || cfg.Cache.RepoSizeLimit != defaults.RepoSizeLimit ||
		cfg.Cache.SizeLimit != defaults.SizeLimit || cfg.Cache.SweepInterval != defaults.SweepInterval {
		log.Warn("cache eviction settings are ignored when cache.external_server is set; configure them on that server instead")
	}
}

// warnIgnoredCacheSecret flags an external cache server secret configured on a runner that uses the built-in cache server.
func warnIgnoredCacheSecret(cfg *config.Config) {
	if cfg.Cache.ExternalServer != "" {
		return
	}
	// Not using an external cache server, so any configured secret is ignored.
	if cfg.Cache.ExternalSecret == "" {
		return
	}
	// LoadDefault resolves external_secret_file into ExternalSecret, so report whichever key the operator actually wrote.
	key := "cache.external_secret"
	if cfg.Cache.ExternalSecretFile != "" {
		key = "cache.external_secret_file"
	}
	log.Warnf("%s is set but cache.external_server is not; the built-in cache server does not use a shared secret, so the value is ignored", key)
}

func (r *Runner) cacheIsolatedFrom(job *model.Job, pickPlatform func([]string) string) bool {
	jobContainer := job.Container()
	return (jobContainer != nil && jobContainer.Image != "" || pickPlatform(job.RunsOn()) != labels.SelfHostedPlatform) && r.isolatedCacheNetwork() != ""
}

func (r *Runner) detectIsolatedCacheNetwork() string {
	if r.cacheHandler == nil {
		return ""
	}
	addr, err := netip.ParseAddr(hostOf(r.cacheHandler.ExternalURL()))
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	network, err := container.IsolatedNetwork(ctx, addr, r.cfg.Container.Network)
	if err != nil {
		log.Warnf("cannot check whether jobs reach the cache server: %v", err)
	}
	return network
}
