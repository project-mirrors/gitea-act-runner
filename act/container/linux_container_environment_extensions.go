// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gitea.com/gitea/runner/act/common/logging"
)

type LinuxContainerEnvironmentExtensions struct{}

var windowsPathRegex = regexp.MustCompile(`^([a-zA-Z]):\\(.+)$`)

// Resolves the equivalent host path inside the container
// This is required for windows and WSL 2 to translate things like C:\Users\Myproject to /mnt/users/Myproject
// For use in docker volumes and binds
func (*LinuxContainerEnvironmentExtensions) ToContainerPath(path string) string {
	if runtime.GOOS == "windows" && strings.Contains(path, "/") {
		logging.Error("You cannot specify linux style local paths (/mnt/etc) on Windows as it does not understand them.")
		return ""
	}

	abspath, err := filepath.Abs(path)
	if err != nil {
		logging.Error(err)
		return ""
	}

	windowsPathComponents := windowsPathRegex.FindStringSubmatch(abspath)

	// Return as-is if no match
	if windowsPathComponents == nil {
		return abspath
	}

	// Convert to WSL2-compatible path if it is a windows path
	// NOTE: Cannot use filepath because it will use the wrong path separators assuming we want the path to be windows
	// based if running on Windows, and because we are feeding this to Docker, GoLang auto-path-translate doesn't work.
	return "/mnt/" + strings.ToLower(windowsPathComponents[1]) + "/" + strings.ReplaceAll(windowsPathComponents[2], `\`, `/`)
}

func (*LinuxContainerEnvironmentExtensions) GetActPath() string {
	return "/var/run/act"
}

func (*LinuxContainerEnvironmentExtensions) GetPathVariableName() string {
	return "PATH"
}

func (*LinuxContainerEnvironmentExtensions) DefaultPathVariable() string {
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

func (*LinuxContainerEnvironmentExtensions) JoinPathVariable(paths ...string) string {
	return strings.Join(paths, ":")
}

// DefaultToolCache is where the runner mounts the tool cache inside job containers.
const DefaultToolCache = "/opt/hostedtoolcache"

type runnerArchKey struct{}

func WithRunnerArch(ctx context.Context) context.Context { // shares one architecture lookup per run
	if ctx.Value(runnerArchKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, runnerArchKey{}, &runnerArchCache{lock: make(chan struct{}, 1)})
}

type runnerArchCache struct {
	lock chan struct{} // lets cancelled jobs stop waiting for the cache
	arch string
}

func cachedRunnerArch(ctx context.Context) string {
	cache, ok := ctx.Value(runnerArchKey{}).(*runnerArchCache)
	if !ok {
		return RunnerArch(ctx)
	}
	select {
	case cache.lock <- struct{}{}:
		defer func() { <-cache.lock }()
	case <-ctx.Done():
		return ""
	}
	if cache.arch == "" {
		cache.arch = RunnerArch(ctx)
	}
	return cache.arch
}

func (*LinuxContainerEnvironmentExtensions) GetRunnerContext(ctx context.Context) map[string]any {
	return map[string]any{
		"os":         "Linux",
		"arch":       cachedRunnerArch(ctx),
		"temp":       "/tmp",
		"tool_cache": DefaultToolCache,
	}
}

func (*LinuxContainerEnvironmentExtensions) IsEnvironmentCaseInsensitive() bool {
	return false
}
