// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"cmp"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"gitea.com/gitea/runner/internal/app/poll"
	"gitea.com/gitea/runner/internal/app/run"
	"gitea.com/gitea/runner/internal/pkg/client"
	"gitea.com/gitea/runner/internal/pkg/config"
	"gitea.com/gitea/runner/internal/pkg/labels"

	pingv1 "gitea.dev/actionslib/ping/v1"
	runnerv1 "gitea.dev/actionslib/runner/v1"
)

type runnerOptions struct {
	capacity   int
	ephemeral  bool
	cacheV2    *bool
	cacheHost  string
	image      string
	kubernetes *config.Kubernetes
}

func startRunner(t *testing.T, repo, labelName string, options runnerOptions) *poll.Poller {
	t.Helper()
	ctx := t.Context()
	token, err := fixture.RegistrationToken(ctx, repo)
	if err != nil {
		t.Fatalf("get registration token: %v", err)
	}

	cfg, err := config.LoadDefault("")
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if options.cacheV2 != nil {
		cfg.Cache.V2 = options.cacheV2
	}
	cfg.Container.DockerHost = os.Getenv("DOCKER_HOST")
	if cfg.Container.DockerHost == "" {
		cfg.Container.DockerHost = "unix:///var/run/docker.sock"
	}
	cfg.Cache.Dir = t.TempDir() + "/cache"
	cfg.Cache.Host = options.cacheHost
	cfg.Runner.Insecure = true
	cfg.Runner.FetchInterval = 250 * time.Millisecond // faster than prod defaults for local fixture
	cfg.Runner.FetchIntervalMax = 250 * time.Millisecond
	cfg.Runner.StateReportInterval = 500 * time.Millisecond // so cancel reaches the job quickly
	cfg.Runner.LogReportInterval = 500 * time.Millisecond
	cfg.Runner.Capacity = max(options.capacity, 1)
	if fixture.network != "" {
		cfg.Container.Network = fixture.network
	}

	scheme := labels.SchemeDocker
	if options.kubernetes != nil {
		scheme, cfg.Kubernetes = labels.SchemeKubernetes, *options.kubernetes
	}
	rawLabel := labelName + ":" + scheme + "://" + cmp.Or(options.image, os.Getenv("E2E_JOB_IMAGE"))
	label, err := labels.Parse(rawLabel)
	if err != nil {
		t.Fatalf("parse label %q: %v", rawLabel, err)
	}
	labelNames := []string{label.Name}

	pingCli := client.New(fixture.baseURL, cfg.Runner.TLSConfig(), "", "", config.RequestTimeout, cfg.Runner.ExtraHeaders)
	if _, err := pingCli.Ping(ctx, &pingv1.PingRequest{Data: t.Name()}); err != nil {
		t.Fatalf("ping %s: %v", fixture.baseURL, err)
	}

	regResp, err := pingCli.Register(ctx, &runnerv1.RegisterRequest{
		Name:         t.Name(),
		Token:        token,
		Version:      "e2e",
		Labels:       labelNames,
		Ephemeral:    options.ephemeral,
		Capabilities: run.RunnerCapabilities(),
	})
	if err != nil {
		t.Fatalf("register runner: %v", err)
	}
	if options.ephemeral && !regResp.Runner.Ephemeral {
		t.Fatal("gitea did not grant ephemeral registration")
	}

	reg := &config.Registration{
		ID:        regResp.Runner.Id,
		UUID:      regResp.Runner.Uuid,
		Name:      regResp.Runner.Name,
		Token:     regResp.Runner.Token,
		Address:   fixture.baseURL,
		Labels:    []string{rawLabel},
		Ephemeral: regResp.Runner.Ephemeral,
	}

	cli := client.New(fixture.baseURL, cfg.Runner.TLSConfig(), reg.UUID, reg.Token, config.RequestTimeout, cfg.Runner.ExtraHeaders)

	runner := run.NewRunner(cfg, reg, cli)
	if _, err := runner.Declare(ctx, labelNames); err != nil {
		_ = runner.Close()
		t.Fatalf("declare runner: %v", err)
	}

	poller := poll.New(cfg, cli, runner)
	go poller.Poll()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := errors.Join(poller.Shutdown(ctx), runner.Close()); err != nil {
			t.Logf("runner shutdown: %v", err)
		}
	})
	return poller
}
