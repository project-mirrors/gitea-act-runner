// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/moby/moby/api/pkg/stdcopy"
	apicontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
)

func testKubernetes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cluster := requireCluster(t)

	api, repo := newScenario(t)
	if err := api.CreateVariable(ctx, repo, "E2E_SERVICE_IMAGE", cluster.serviceImage); err != nil {
		t.Fatalf("create service image variable: %v", err)
	}
	startRunner(t, repo, "e2e-kubernetes", runnerOptions{image: cluster.jobImage, kubernetes: cluster.kubernetes})
	pushWorkflow(t, api, repo, "kubernetes.yml")

	wfRun := waitForRun(t, api, repo)
	requireSuccess(t, api, repo, wfRun.ID)
}

// checkClusterLeaks runs after every test, so a pod or secret any of them left behind fails the suite.
func checkClusterLeaks() error {
	if !clusterStarted.Load() {
		return nil
	}
	cluster, err := k3s()
	if err != nil {
		return nil // the tests that needed the cluster already failed
	}
	if out, err := containerExec(context.Background(), dockerCLI, cluster.id, "", nil,
		"kubectl", "wait", "--for=delete", "pods,secrets", "--selector=app.kubernetes.io/managed-by=gitea-runner", "--timeout=1m",
	); err != nil {
		return fmt.Errorf("job pods or secrets left behind: %w: %s", err, out)
	}
	return nil
}

type kubeCluster struct {
	id                     string
	kubernetes             *config.Kubernetes
	jobImage, serviceImage string
}

var (
	k3s            = sync.OnceValues(startK3s) // independent of Gitea, so it can boot alongside it
	clusterStarted atomic.Bool
	clusterCleanup []func()
	sharedCluster  = sync.OnceValues(func() (*kubeCluster, error) {
		cluster, err := k3s()
		if err != nil {
			return nil, err
		}
		gitea, err := fixture.cli.ContainerInspect(context.Background(), fixture.id, mobyclient.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect gitea container: %w", err)
		}
		giteaURL, err := url.Parse(fixture.baseURL)
		if err != nil {
			return nil, fmt.Errorf("parse gitea url: %w", err)
		}
		cluster.kubernetes.PodTemplate = map[string]any{"spec": map[string]any{"hostAliases": []any{map[string]any{
			"ip":        gitea.Container.NetworkSettings.Networks[fixture.network].IPAddress.String(),
			"hostnames": []any{giteaURL.Hostname()},
		}}}}
		return cluster, nil
	})
)

func requireCluster(t *testing.T) *kubeCluster {
	t.Helper()
	cluster, err := sharedCluster()
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

func stopCluster() {
	if !clusterStarted.Load() {
		return
	}
	_, _ = k3s()
	for _, cleanup := range slices.Backward(clusterCleanup) {
		cleanup()
	}
}

func startK3s() (*kubeCluster, error) {
	clusterStarted.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerCLI
	sharedNet, _, ok := selfNetwork(ctx, cli)
	if !ok {
		return nil, errors.New("e2e tests must run in a container on a user-defined network")
	}

	name := fmt.Sprintf("gitea-runner-e2e-k3s-%d", time.Now().UnixNano())
	server := "https://" + name + ":6443"

	created, err := cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &apicontainer.Config{
			Image: os.Getenv("E2E_K3S_IMAGE"),
			Cmd:   []string{"server", "--disable=traefik,metrics-server,servicelb,local-storage,coredns", "--tls-san=" + name, "--kubelet-arg=fail-cgroupv1=false"},
		},
		HostConfig:       &apicontainer.HostConfig{Privileged: true},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{sharedNet: {}}},
		Name:             name,
	})
	if err != nil {
		return nil, fmt.Errorf("create k3s container: %w", err)
	}
	clusterCleanup = append(clusterCleanup, func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})
	if _, err := cli.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		return nil, fmt.Errorf("start k3s container: %w", err)
	}

	if err := waitForContainerLog(ctx, created.ID, "containerd is now running"); err != nil {
		return nil, fmt.Errorf("wait for k3s containerd: %w", err)
	}
	sandboxPulled := make(chan struct{})
	go func() { // first pods would otherwise pull the sandbox image themselves
		defer close(sandboxPulled)
		pullCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, _ = containerExec(pullCtx, cli, created.ID, "", nil, "sh", "-c",
			`ctr --namespace=k8s.io images pull "docker.io/$(sed -n 's/^ *sandbox = "\(.*\)"$/\1/p' /var/lib/rancher/k3s/agent/etc/containerd/config.toml)"`)
	}()
	images, err := preloadImages(ctx, created.ID, os.Getenv("E2E_JOB_IMAGE"), os.Getenv("SERVICE_IMAGE"))
	<-sandboxPulled
	if err != nil {
		return nil, err
	}

	for {
		_, err := containerExec(ctx, cli, created.ID, "", nil, "kubectl", "get", "serviceaccount", "default")
		if err == nil {
			break
		}
		inspected, inspectErr := cli.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
		if inspectErr != nil || !inspected.Container.State.Running || ctx.Err() != nil {
			var logs bytes.Buffer
			if reader, logsErr := cli.ContainerLogs(context.Background(), created.ID, mobyclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "30"}); logsErr == nil {
				_, _ = stdcopy.StdCopy(&logs, &logs, reader)
				_ = reader.Close()
			}
			return nil, fmt.Errorf("k3s did not start: %w, state %+v %v, last log lines:\n%s", err, inspected.Container.State, inspectErr, logs.String())
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}

	kubeconfig, err := containerExec(ctx, cli, created.ID, "", nil, "cat", "/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	path := filepath.Join(binDir, "kubeconfig")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(kubeconfig, "https://127.0.0.1:6443", server)), 0o600); err != nil {
		return nil, fmt.Errorf("write kubeconfig: %w", err)
	}
	return &kubeCluster{id: created.ID, kubernetes: &config.Kubernetes{Kubeconfig: path, Namespace: "default"}, jobImage: images[0], serviceImage: images[1]}, nil
}

func waitForContainerLog(ctx context.Context, id, message string) error {
	logs, err := dockerCLI.ContainerLogs(ctx, id, mobyclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return err
	}
	defer logs.Close()

	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		_, err := stdcopy.StdCopy(writer, writer, logs)
		_ = writer.CloseWithError(err)
	}()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), message) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("container exited before logging %q", message)
}

func preloadImages(ctx context.Context, k3sID string, images ...string) ([]string, error) {
	var aliases []string
	for index, image := range images {
		alias := fmt.Sprintf("gitea-runner-e2e/preload-%d:%d", index, time.Now().UnixNano())
		if _, err := dockerCLI.ImageTag(ctx, mobyclient.ImageTagOptions{Source: image, Target: alias}); err != nil {
			return nil, fmt.Errorf("tag %s: %w", image, err)
		}
		clusterCleanup = append(clusterCleanup, func() { _, _ = dockerCLI.ImageRemove(context.Background(), alias, mobyclient.ImageRemoveOptions{}) })
		aliases = append(aliases, alias)
	}
	archive, err := dockerCLI.ImageSave(ctx, aliases)
	if err != nil {
		return nil, fmt.Errorf("save %v: %w", images, err)
	}
	defer archive.Close()
	if out, err := containerExec(ctx, dockerCLI, k3sID, "", archive, "ctr", "--namespace=k8s.io", "images", "import", "-"); err != nil {
		return nil, fmt.Errorf("import %v into k3s: %w: %s", images, err, out)
	}
	return aliases, nil
}
