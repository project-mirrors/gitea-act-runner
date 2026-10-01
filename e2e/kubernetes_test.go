// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	k3sID, kubernetes := startK3s(t)

	images := preloadImages(t, k3sID, os.Getenv("E2E_JOB_IMAGE"), os.Getenv("SERVICE_IMAGE"))

	api, repo := newScenario(t)
	if err := api.CreateVariable(ctx, repo, "E2E_SERVICE_IMAGE", images[1]); err != nil {
		t.Fatalf("create service image variable: %v", err)
	}
	startRunner(t, repo, "e2e-kubernetes", runnerOptions{image: images[0], kubernetes: kubernetes})
	pushWorkflow(t, api, repo, "kubernetes.yml")

	wfRun := waitForRun(t, api, repo)
	requireSuccess(t, api, repo, wfRun.ID)

	if out, err := containerExec(ctx, fixture.cli, k3sID, "", nil,
		"kubectl", "wait", "--for=delete", "pods,secrets", "--selector=app.kubernetes.io/managed-by=gitea-runner", "--timeout=1m",
	); err != nil {
		t.Errorf("job pods or secrets left behind: %v: %s", err, out)
	}
}

func startK3s(t *testing.T) (string, *config.Kubernetes) {
	t.Helper()
	ctx := t.Context()
	cli := fixture.cli

	name := fmt.Sprintf("gitea-runner-e2e-k3s-%d", time.Now().UnixNano())
	apiPort := network.MustParsePort("6443/tcp")
	server := "https://" + name + ":6443"
	hostConfig := &apicontainer.HostConfig{Privileged: true}
	var netConfig *network.NetworkingConfig
	if fixture.network != "" {
		netConfig = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{fixture.network: {}}}
	} else {
		loopback := netip.MustParseAddr("127.0.0.1")
		port, err := freeHostPort(loopback.String())
		if err != nil {
			t.Fatalf("find a free host port: %v", err)
		}
		server = "https://" + net.JoinHostPort(loopback.String(), strconv.Itoa(port))
		hostConfig.PortBindings = network.PortMap{apiPort: []network.PortBinding{{HostIP: loopback, HostPort: strconv.Itoa(port)}}}
	}

	created, err := cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &apicontainer.Config{
			Image:        os.Getenv("E2E_K3S_IMAGE"),
			Cmd:          []string{"server", "--disable=traefik,metrics-server,servicelb,local-storage,coredns", "--tls-san=" + name, "--kubelet-arg=fail-cgroupv1=false"},
			ExposedPorts: network.PortSet{apiPort: struct{}{}},
		},
		HostConfig:       hostConfig,
		NetworkingConfig: netConfig,
		Name:             name,
	})
	if err != nil {
		t.Fatalf("create k3s container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})
	if _, err := cli.ContainerStart(ctx, created.ID, mobyclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start k3s container: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		_, err := containerExec(waitCtx, cli, created.ID, "", nil, "kubectl", "get", "serviceaccount", "default")
		if err == nil {
			break
		}
		inspected, inspectErr := cli.ContainerInspect(ctx, created.ID, mobyclient.ContainerInspectOptions{})
		if inspectErr != nil || !inspected.Container.State.Running || waitCtx.Err() != nil {
			var logs bytes.Buffer
			if reader, logsErr := cli.ContainerLogs(ctx, created.ID, mobyclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "30"}); logsErr == nil {
				_, _ = stdcopy.StdCopy(&logs, &logs, reader)
				_ = reader.Close()
			}
			t.Fatalf("k3s did not start: %v, state %+v %v, last log lines:\n%s", err, inspected.Container.State, inspectErr, logs.String())
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(pollInterval):
		}
	}

	kubeconfig, err := containerExec(ctx, cli, created.ID, "", nil, "cat", "/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(kubeconfig, "https://127.0.0.1:6443", server)), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	kubernetes := &config.Kubernetes{Kubeconfig: path, Namespace: "default"}

	if fixture.network != "" {
		gitea, err := cli.ContainerInspect(ctx, fixture.id, mobyclient.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("inspect gitea container: %v", err)
		}
		giteaURL, err := url.Parse(fixture.baseURL)
		if err != nil {
			t.Fatalf("parse gitea url: %v", err)
		}
		kubernetes.PodTemplate = map[string]any{"spec": map[string]any{"hostAliases": []any{map[string]any{
			"ip":        gitea.Container.NetworkSettings.Networks[fixture.network].IPAddress.String(),
			"hostnames": []any{giteaURL.Hostname()},
		}}}}
	}
	return created.ID, kubernetes
}

func preloadImages(t *testing.T, k3sID string, images ...string) []string {
	t.Helper()
	ctx := t.Context()
	var aliases []string
	for index, image := range images {
		alias := fmt.Sprintf("gitea-runner-e2e/preload-%d:%d", index, time.Now().UnixNano())
		if _, err := fixture.cli.ImageTag(ctx, mobyclient.ImageTagOptions{Source: image, Target: alias}); err != nil {
			t.Fatalf("tag %s: %v", image, err)
		}
		t.Cleanup(func() { _, _ = fixture.cli.ImageRemove(context.Background(), alias, mobyclient.ImageRemoveOptions{}) })
		aliases = append(aliases, alias)
	}
	archive, err := fixture.cli.ImageSave(ctx, aliases)
	if err != nil {
		t.Fatalf("save %v: %v", images, err)
	}
	defer archive.Close()
	if out, err := containerExec(ctx, fixture.cli, k3sID, "", archive, "ctr", "--namespace=k8s.io", "images", "import", "-"); err != nil {
		t.Fatalf("import %v into k3s: %v: %s", images, err, out)
	}
	return aliases
}
