// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"
	"gitea.com/gitea/runner/act/lookpath"
	"gitea.com/gitea/runner/internal/pkg/process"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type HostEnvironment struct {
	Path      string
	TmpDir    string
	ToolCache string
	Workdir   string
	// CleanWorkdir means teardown owns Workdir and may delete it. Leave false
	// when Workdir points at a caller-owned checkout (e.g. `act` local mode).
	CleanWorkdir bool
	ActPath      string
	Root         string
	TrackingID   string // RUNNER_TRACKING_ID of the job, a step with another value keeps its leftovers
	StdOut       io.Writer
	AllocatePTY  bool // allocate a pseudo-TTY for each step's process

	// procGroup owns every process the job's steps start. Atomic: Remove may read
	// it while a step is still starting.
	procGroupOnce sync.Once
	procGroup     atomic.Pointer[process.Group]
}

// processGroup returns the job-scoped process group, creating it on first use.
// Returns nil if the job object could not be created; Group is nil-safe.
func (e *HostEnvironment) processGroup(ctx context.Context) *process.Group {
	e.procGroupOnce.Do(func() {
		group, err := process.NewGroup()
		if err != nil {
			common.Logger(ctx).Warnf("could not create the job's process group, processes a step leaves behind may outlive the job: %v", err)
			return
		}
		e.procGroup.Store(group)
	})
	return e.procGroup.Load()
}

func (e *HostEnvironment) Create(_, _ []string) common.Executor {
	return common.NewPipelineExecutor()
}

func (e *HostEnvironment) Close() common.Executor {
	return common.NewPipelineExecutor()
}

func (e *HostEnvironment) Copy(destPath string, files ...*FileEntry) common.Executor {
	return func(ctx context.Context) error {
		for _, f := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(destPath, f.Name)), 0o777); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(destPath, f.Name), []byte(f.Body), fs.FileMode(f.Mode)); err != nil {
				return err
			}
		}
		return nil
	}
}

func (e *HostEnvironment) CopyDir(destPath, srcPath string, useGitIgnore, skipGitDir bool) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		srcPrefix := filepath.Dir(srcPath)
		if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
			srcPrefix += string(filepath.Separator)
		}
		logger.Debugf("Stripping prefix:%s src:%s", srcPrefix, srcPath)
		fc := &filecollector.FileCollector{
			SrcPath:      srcPath,
			SrcPrefix:    srcPrefix,
			SkipGitDir:   skipGitDir,
			UseGitIgnore: useGitIgnore,
			Handler: &filecollector.CopyCollector{
				DstDir: destPath,
			},
		}
		return filepath.Walk(srcPath, fc.CollectFiles(ctx))
	}
}

func (e *HostEnvironment) DumpLogs(_ context.Context) error {
	return nil
}

func (e *HostEnvironment) Inspect(_ context.Context) (*Info, error) {
	return &Info{Health: HealthNone, Ports: map[string]string{}}, nil
}

func (e *HostEnvironment) GetContainerArchive(ctx context.Context, srcPath string) (io.ReadCloser, error) {
	srcPath = filepath.Clean(srcPath)
	fi, err := os.Lstat(srcPath)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		tw := tar.NewWriter(writer)
		err := writeHostArchive(ctx, &filecollector.TarCollector{TarWriter: tw}, srcPath, fi)
		if err == nil {
			err = tw.Close()
		}
		writer.CloseWithError(err)
	}()
	return startedArchive(reader)
}

func writeHostArchive(ctx context.Context, tc *filecollector.TarCollector, srcPath string, fi fs.FileInfo) error {
	if fi.IsDir() {
		srcPrefix := srcPath
		if !strings.HasSuffix(srcPrefix, string(filepath.Separator)) {
			srcPrefix += string(filepath.Separator)
		}
		fc := &filecollector.FileCollector{
			SrcPath:   srcPath,
			SrcPrefix: srcPrefix,
			Handler:   tc,
		}
		return filepath.Walk(srcPath, fc.CollectFiles(ctx))
	}
	var f io.ReadCloser
	var linkname string
	var err error
	if fi.Mode()&fs.ModeSymlink != 0 {
		if linkname, err = os.Readlink(srcPath); err != nil {
			return err
		}
	} else {
		if f, err = os.Open(srcPath); err != nil {
			return err
		}
		defer f.Close()
	}
	return tc.WriteFile(fi.Name(), fi, linkname, f)
}

func (e *HostEnvironment) Pull(_ bool) common.Executor {
	return common.NewPipelineExecutor()
}

func (e *HostEnvironment) Start(_ bool) common.Executor {
	return common.NewPipelineExecutor()
}

type ptyWriter struct {
	Out       io.Writer
	AutoStop  atomic.Bool
	dirtyLine bool
}

func (w *ptyWriter) Write(buf []byte) (int, error) {
	if w.AutoStop.Load() && len(buf) > 0 && buf[len(buf)-1] == 4 {
		n, err := w.Out.Write(buf[:len(buf)-1])
		if err != nil {
			return n, err
		}
		if w.dirtyLine || len(buf) > 1 && buf[len(buf)-2] != '\n' {
			_, _ = w.Out.Write([]byte("\n"))
		}
		return n, io.EOF
	}
	w.dirtyLine = strings.LastIndex(string(buf), "\n") < len(buf)-1
	return w.Out.Write(buf)
}

func lookupPathHost(cmd string, env map[string]string, writer io.Writer) (string, error) {
	f, err := lookpath.LookPath2(cmd, env)
	if err != nil {
		err := "Cannot find: " + cmd + " in PATH"
		if _, _err := writer.Write([]byte(err + "\n")); _err != nil {
			return "", fmt.Errorf("%v: %w", err, _err)
		}
		return "", errors.New(err)
	}
	return f, nil
}

func setupPty(cmd *exec.Cmd, cmdline string) (*os.File, *os.File, error) {
	ppty, tty, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	if term.IsTerminal(int(tty.Fd())) {
		_, err := term.MakeRaw(int(tty.Fd()))
		if err != nil {
			ppty.Close()
			tty.Close()
			return nil, nil, err
		}
	}
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = process.SysProcAttr(cmdline, true)
	return ppty, tty, nil
}

func writeKeepAlive(ctx context.Context, ppty io.Writer) {
	for ctx.Err() == nil {
		if count, err := ppty.Write([]byte{4}); count != 1 || err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (e *HostEnvironment) UpdateFromImageEnv(_ *map[string]string) common.Executor {
	return common.NewPipelineExecutor()
}

func getEnvListFromMap(env map[string]string) []string {
	envList := make([]string, 0, len(env))
	for key, value := range env {
		envList = append(envList, key+"="+value)
	}
	return envList
}

func (e *HostEnvironment) exec(ctx context.Context, command []string, cmdline string, env map[string]string, workdir string) error {
	wd := e.Path
	if workdir != "" {
		wd = workdir
		if !filepath.IsAbs(workdir) {
			wd = filepath.Join(e.Path, workdir)
		}
	}
	// Flush any buffered, not-yet-newline-terminated trailing line, as the docker backend
	// does in waitForCommand, so the final line of a command's output is not lost.
	defer common.FlushWriter(e.StdOut)
	process.UTF8Console()

	f, err := lookupPathHost(command[0], env, e.StdOut)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, f)
	cmd.Path = f
	cmd.Args = command
	cmd.Stdout = e.StdOut
	trackingID := e.TrackingID
	if value, ok := common.LookupFold(env, "RUNNER_TRACKING_ID", e.IsEnvironmentCaseInsensitive()); ok {
		trackingID = value
	}
	tracked := trackingID == e.TrackingID
	cmd.Env = append(getEnvListFromMap(env), "RUNNER_TRACKING_ID="+trackingID)
	cmd.Stderr = e.StdOut
	cmd.Dir = wd
	cmd.SysProcAttr = process.SysProcAttr(cmdline, false)

	// Kills the step's whole tree on cancellation and bounds the post-exit I/O
	// wait, so an orphan holding cmd's stdout pipe cannot hang cmd.Wait().
	treeKill := process.NewTreeKill(cmd)
	treeKill.KeepLeftovers = !tracked

	var ppty *os.File
	var tty *os.File
	defer func() {
		if ppty != nil {
			ppty.Close()
		}
		if tty != nil {
			tty.Close()
		}
	}()
	if e.AllocatePTY {
		var err error
		ppty, tty, err = setupPty(cmd, cmdline)
		if err != nil {
			common.Logger(ctx).Debugf("Failed to setup Pty %v\n", err.Error())
		}
	}
	var writer *ptyWriter
	var logctx context.Context
	if ppty != nil {
		writer = &ptyWriter{Out: e.StdOut}
		var finishLog context.CancelFunc
		logctx, finishLog = context.WithCancel(context.Background())
		go func() {
			defer finishLog()
			_, _ = io.Copy(writer, ppty)
		}()
		go writeKeepAlive(logctx, ppty)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if tracked {
		if err := e.processGroup(ctx).Assign(cmd.Process); err != nil {
			common.Logger(ctx).Warnf("could not assign the step's process to the job's process group, a process it leaves behind may outlive the job: %v", err)
		}
	}
	if k, kerr := treeKill.Capture(cmd.Process); kerr != nil {
		common.Logger(ctx).Warnf("process tree kill setup failed, falling back to single-process kill: %v", kerr)
	} else {
		defer k.Close()
	}
	err = treeKill.Wait(ctx, cmd)
	if tty != nil {
		writer.AutoStop.Store(true)
		if _, err := tty.WriteString("\x04"); err != nil {
			common.Logger(ctx).Debug("Failed to write EOT")
		}
		select {
		case <-logctx.Done():
		case <-ctx.Done(): // a cancelled step does not wait on output nobody consumes
		}
		ppty.Close()
		ppty = nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return ExitCodeError(exitErr.ExitCode())
	}
	return err
}

func (e *HostEnvironment) Exec(command []string, env map[string]string, _user, workdir string) common.Executor {
	return e.ExecWithCmdLine(command, "", env, workdir)
}

func (e *HostEnvironment) ExecWithCmdLine(command []string, cmdline string, env map[string]string, workdir string) common.Executor {
	return func(ctx context.Context) error {
		if err := e.exec(ctx, command, cmdline, env, workdir); err != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("this step has been cancelled: %w", err)
			default:
				return err
			}
		}
		return nil
	}
}

func (e *HostEnvironment) UpdateFromEnv(srcPath string, env *map[string]string) common.Executor {
	return parseEnvFile(e, srcPath, env, runtime.GOOS == "windows")
}

// removeAll is a var so tests can substitute a blocking stub.
var removeAll = os.RemoveAll

// Returns on cancel while an uninterruptible delete keeps running, freeing the runner's capacity slot.
func removeAllWithContext(ctx context.Context, path string) error {
	done := make(chan error, 1)
	go func() { done <- removeAll(path) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func removePathWithRetry(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	attempts := 1
	delay := time.Duration(0)
	if runtime.GOOS == "windows" {
		attempts = 5
		delay = 200 * time.Millisecond
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		lastErr = removeAllWithContext(ctx, path)
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}
	}
	return lastErr
}

// hostCleanupTimeout bounds each teardown phase so one stalled delete cannot
// wedge the runner slot. A var so tests can shrink it.
var hostCleanupTimeout = 30 * time.Second

func (e *HostEnvironment) Remove() common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)

		// End lingering processes before removing the workspace, on Windows their file locks block cleanup.
		if err := e.procGroup.Load().Close(); err != nil {
			logger.Debugf("closing the job's process group failed: %v", err)
		}
		if killed, err := process.KillTrackedProcesses(e.TrackingID); err != nil {
			logger.Debugf("killing the job's leftover processes reported errors: %v", err)
		} else if killed > 0 {
			logger.Debugf("killed %d leftover process(es) of the job", killed)
		}

		// Detach: a cancelled ctx would skip removePathWithRetry's retries,
		// which absorb Windows file-handle release lag after the kill above.
		rmCtx, rmCancel := context.WithTimeout(context.Background(), hostCleanupTimeout)
		defer rmCancel()

		var errs []error
		if err := removePathWithRetry(rmCtx, cmp.Or(e.Root, e.Path)); err != nil {
			logger.Warnf("failed to remove host misc state %s: %v", cmp.Or(e.Root, e.Path), err)
			errs = append(errs, err)
		}
		if e.CleanWorkdir {
			if err := removePathWithRetry(rmCtx, e.Workdir); err != nil {
				logger.Warnf("failed to remove host workspace %s: %v", e.Workdir, err)
				errs = append(errs, err)
			}
		}
		for _, err := range errs {
			if !errors.Is(err, context.DeadlineExceeded) {
				return errors.Join(errs...)
			}
		}
		// Teardown timed out; warned above. Do not fail job completion over it.
		return nil
	}
}

func (e *HostEnvironment) ToContainerPath(path string) string {
	if bp, err := filepath.Rel(e.Workdir, path); err != nil {
		return filepath.Join(e.Path, bp)
	} else if filepath.Clean(e.Workdir) == filepath.Clean(path) {
		return e.Path
	}
	return path
}

func (e *HostEnvironment) GetActPath() string {
	actPath := e.ActPath
	if runtime.GOOS == "windows" {
		actPath = strings.ReplaceAll(actPath, "\\", "/")
	}
	return actPath
}

func (*HostEnvironment) GetPathVariableName() string {
	switch runtime.GOOS {
	case "plan9":
		return "path"
	case "windows":
		return "Path" // Actually we need a case insensitive map
	}
	return "PATH"
}

func (e *HostEnvironment) DefaultPathVariable() string {
	return os.Getenv(e.GetPathVariableName())
}

func (*HostEnvironment) JoinPathVariable(paths ...string) string {
	return strings.Join(paths, string(filepath.ListSeparator))
}

// Reference for Arch values for runner.arch
// https://docs.github.com/en/actions/learn-github-actions/contexts#runner-context
func goArchToActionArch(arch string) string {
	return cmp.Or(map[string]string{
		"x86_64":  "X64",
		"amd64":   "X64",
		"386":     "X86",
		"arm":     "ARM",
		"aarch64": "ARM64",
		"arm64":   "ARM64",
	}[arch], arch)
}

func goOsToActionOs(os string) string {
	return cmp.Or(map[string]string{
		"linux":   "Linux",
		"darwin":  "macOS",
		"windows": "Windows",
		"freebsd": "FreeBSD",
	}[os], os)
}

func (e *HostEnvironment) GetRunnerContext(_ context.Context) map[string]any {
	return map[string]any{
		"os":         goOsToActionOs(runtime.GOOS),
		"arch":       goArchToActionArch(runtime.GOARCH),
		"temp":       e.TmpDir,
		"tool_cache": e.ToolCache,
	}
}

func (e *HostEnvironment) ReplaceLogWriter(stdout, _ io.Writer) (io.Writer, io.Writer) {
	org := e.StdOut
	e.StdOut = stdout
	return org, org
}

func (*HostEnvironment) IsEnvironmentCaseInsensitive() bool {
	return runtime.GOOS == "windows"
}
