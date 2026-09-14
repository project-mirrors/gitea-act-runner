// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package runner

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/container"

	"gitea.dev/actionslib/pkg/model"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDockerProxyMountedJob(t *testing.T) {
	mode := os.Getenv("ACT_TEST_DOCKER_PROXY")
	if mode == "" {
		t.Skip("set ACT_TEST_DOCKER_PROXY=proxy or direct to verify mounted Docker access")
	}
	require.Contains(t, []string{"proxy", "direct"}, mode)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	dockerClient, err := container.GetDockerClient(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, dockerClient.Close()) })
	require.True(t, strings.HasPrefix(dockerClient.DaemonHost(), "unix://"), "mounted Docker coverage requires a Unix daemon socket")

	fixtureDir, err := filepath.Abs("testdata/docker-proxy")
	require.NoError(t, err)
	resourceName := "gitea-proxy-test-" + strings.ToLower(rand.Text())
	runner, err := New(&Config{
		Workdir:               fixtureDir,
		ActionCacheDir:        t.TempDir(),
		EventName:             "push",
		PlatformPicker:        mapPlatformPicker(platforms),
		ContainerNamePrefix:   resourceName,
		ContainerDaemonSocket: dockerClient.DaemonHost(),
		ContainerMaxLifetime:  2 * time.Minute,
		ContainerOptions:      "-v /usr/local/bin/docker:/usr/local/bin/docker:ro -v /usr/local/libexec/docker/cli-plugins:/usr/local/libexec/docker/cli-plugins:ro",
		ValidVolumes:          []string{"/usr/local/bin/docker", "/usr/local/libexec/docker/cli-plugins"},
		Env:                   map[string]string{"PROXY_TEST_RESOURCE": resourceName, "PROXY_TEST_MODE": mode, "PROXY_TEST_IMAGE": baseImage, "COMPOSE_PROJECT_NAME": resourceName},
	})
	require.NoError(t, err)
	planner, err := model.NewWorkflowPlanner(filepath.Join(fixtureDir, "push.yml"), true)
	require.NoError(t, err)
	plan, err := planner.PlanEvent("push")
	if mode == "direct" {
		plan, err = planner.PlanJob("proxy")
	}
	require.NoError(t, err)
	runContext, err := runner.newRunContext(ctx, plan.Stages[0].Runs[0], nil)
	require.NoError(t, err)
	jobName := runContext.jobContainerName()
	jobNetwork, _ := runContext.networkNameForGitea()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanCancel()
		if _, err := dockerClient.ContainerRemove(cleanCtx, jobName, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); !cerrdefs.IsNotFound(err) {
			assert.NoError(t, err)
		}
		for _, name := range []string{resourceName, jobNetwork} {
			if _, err := dockerClient.NetworkRemove(cleanCtx, name, client.NetworkRemoveOptions{}); !cerrdefs.IsNotFound(err) {
				assert.NoError(t, err)
			}
		}
		for _, name := range []string{resourceName, jobName, jobName + "-env"} {
			if _, err := dockerClient.VolumeRemove(cleanCtx, name, client.VolumeRemoveOptions{}); !cerrdefs.IsNotFound(err) {
				assert.NoError(t, err)
			}
		}
	})

	hook := &test.Hook{}
	require.NoError(t, runner.NewPlanExecutor(plan)(common.WithLoggerHook(ctx, hook)))
	var messages []string
	for _, entry := range hook.AllEntries() {
		messages = append(messages, strings.TrimSpace(entry.Message))
	}
	require.Contains(t, messages, "docker proxy post verified")
	if mode == "proxy" {
		require.Contains(t, messages, "docker binds verified")
	}
	_, err = dockerClient.ContainerInspect(ctx, jobName, client.ContainerInspectOptions{})
	assert.True(t, cerrdefs.IsNotFound(err), "job container survived cleanup: %v", err)
	_, err = dockerClient.NetworkInspect(ctx, resourceName, client.NetworkInspectOptions{})
	if mode == "proxy" {
		assert.True(t, cerrdefs.IsNotFound(err), "labelled network survived cleanup: %v", err)
	} else {
		require.NoError(t, err)
	}
	_, err = dockerClient.VolumeInspect(ctx, resourceName, client.VolumeInspectOptions{})
	if mode == "proxy" {
		assert.True(t, cerrdefs.IsNotFound(err), "labelled volume survived cleanup: %v", err)
	} else {
		require.NoError(t, err)
	}
}
