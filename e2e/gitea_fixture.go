// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/container"

	"github.com/moby/moby/api/pkg/stdcopy"
	apicontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
)

const (
	giteaAdminUser = "e2e-admin"
	giteaAdminMail = "e2e-admin@example.com"
)

type GiteaFixture struct {
	cli        mobyclient.APIClient
	id         string
	image      string
	version    string
	baseURL    string
	network    string // the user-defined network every container of the suite joins
	selfIP     string // the test process's address on network, which job containers can reach
	adminToken string
}

func dockerClient(ctx context.Context) (mobyclient.APIClient, error) {
	cli, err := container.GetDockerClient(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx, mobyclient.PingOptions{}); err != nil {
		return nil, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	return cli, nil
}

// When tests run inside a container (sibling docker daemon), join that network and address by name.
func selfNetwork(ctx context.Context, cli mobyclient.APIClient) (string, string, bool) {
	if _, err := os.Stat("/.dockerenv"); err != nil {
		return "", "", false
	}

	id, err := os.ReadFile("/proc/sys/kernel/hostname")
	if err != nil {
		id, err = os.ReadFile("/etc/hostname")
	}
	if err != nil {
		return "", "", false
	}

	inspected, err := cli.ContainerInspect(ctx, strings.TrimSpace(string(id)), mobyclient.ContainerInspectOptions{})
	if err != nil || inspected.Container.NetworkSettings == nil {
		return "", "", false
	}
	for name, endpoint := range inspected.Container.NetworkSettings.Networks {
		if name != "host" && name != "none" && name != "bridge" { // only user-defined nets have DNS
			return name, endpoint.IPAddress.String(), true
		}
	}
	return "", "", false
}

func StartGitea(ctx context.Context, cli mobyclient.APIClient) (*GiteaFixture, error) {
	closeClient := true
	defer func() {
		if closeClient {
			_ = cli.Close()
		}
	}()

	image := os.Getenv("E2E_GITEA_IMAGE")
	if image == "" {
		return nil, errors.New("E2E_GITEA_IMAGE is not set")
	}

	sharedNet, selfIP, ok := selfNetwork(ctx, cli)
	if !ok {
		return nil, errors.New("e2e tests must run in a container on a user-defined network, `make test-e2e` provides one")
	}
	name := fmt.Sprintf("gitea-runner-e2e-%d", time.Now().UnixNano())
	baseURL := "http://" + net.JoinHostPort(name, "3000")

	if _, err := cli.ImageInspect(ctx, image); err != nil {
		return nil, fmt.Errorf("inspect gitea image %s: %w", image, err)
	}

	resp, err := cli.ContainerCreate(ctx, mobyclient.ContainerCreateOptions{
		Config: &apicontainer.Config{
			Image: image,
			Env: []string{
				"GITEA__security__INSTALL_LOCK=true",
				"GITEA__database__DB_TYPE=sqlite3",
				"GITEA__actions__ENABLED=true",
				"GITEA__server__LFS_START_SERVER=true",
				"GITEA__server__ROOT_URL=" + baseURL + "/",
			},
		},
		HostConfig:       &apicontainer.HostConfig{Tmpfs: map[string]string{"/data": "exec"}},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{sharedNet: {}}},
		Name:             name,
	})
	if err != nil {
		return nil, fmt.Errorf("create gitea container: %w", err)
	}

	f := &GiteaFixture{cli: cli, id: resp.ID, image: image, baseURL: baseURL, network: sharedNet, selfIP: selfIP}
	closeClient = false

	if _, err := cli.ContainerStart(ctx, f.id, mobyclient.ContainerStartOptions{}); err != nil {
		_ = f.Close(ctx)
		return nil, fmt.Errorf("start gitea container: %w", err)
	}

	if err := f.waitHealthy(ctx); err != nil {
		_ = f.Close(ctx)
		return nil, fmt.Errorf("gitea did not become healthy: %w", err)
	}

	if err := f.bootstrapAdmin(ctx); err != nil {
		_ = f.Close(ctx)
		return nil, fmt.Errorf("bootstrap gitea admin: %w", err)
	}

	if err := f.readVersion(ctx); err != nil {
		_ = f.Close(ctx)
		return nil, fmt.Errorf("read gitea version: %w", err)
	}

	return f, nil
}

func (f *GiteaFixture) readVersion(ctx context.Context) error {
	var body struct {
		Version string `json:"version"`
	}
	if err := f.doJSON(ctx, http.MethodGet, "/api/v1/version", nil, &body); err != nil {
		return err
	}
	f.version = body.Version
	return nil
}

func (f *GiteaFixture) waitHealthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	url := f.baseURL + "/api/healthz"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (f *GiteaFixture) bootstrapAdmin(ctx context.Context) error {
	password := randomToken(16)
	if _, err := f.exec(ctx, []string{
		"gitea", "admin", "user", "create",
		"--username", giteaAdminUser,
		"--password", password,
		"--email", giteaAdminMail,
		"--admin",
		"--must-change-password=false",
	}); err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}

	out, err := f.exec(ctx, []string{
		"gitea", "admin", "user", "generate-access-token",
		"--username", giteaAdminUser,
		"--scopes", "all",
		"-t", "e2e-admin-token",
	})
	if err != nil {
		return fmt.Errorf("generate admin token: %w", err)
	}
	token := extractToken(out)
	if token == "" {
		return fmt.Errorf("could not parse access token from CLI output: %q", out)
	}
	f.adminToken = token
	return nil
}

// Last field of `gitea admin user generate-access-token` output (format varies by release).
func extractToken(cliOutput string) string {
	fields := strings.Fields(cliOutput)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *GiteaFixture) exec(ctx context.Context, cmd []string) (string, error) {
	return containerExec(ctx, f.cli, f.id, "git", nil, cmd...) // gitea refuses root
}

func containerExec(ctx context.Context, cli mobyclient.APIClient, id, user string, stdin io.Reader, cmd ...string) (string, error) {
	created, err := cli.ExecCreate(ctx, id, mobyclient.ExecCreateOptions{
		Cmd:          cmd,
		User:         user,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", err
	}

	attached, err := cli.ExecAttach(ctx, created.ID, mobyclient.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer attached.Close()
	if stdin != nil {
		go func() {
			_, _ = io.Copy(attached.Conn, stdin)
			_ = attached.CloseWrite()
		}()
	}

	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, attached.Reader); err != nil {
		return "", err
	}

	inspected, err := cli.ExecInspect(ctx, created.ID, mobyclient.ExecInspectOptions{})
	if err != nil {
		return "", err
	}
	if inspected.ExitCode != 0 {
		return "", fmt.Errorf("exec %v exited %d: %s", cmd, inspected.ExitCode, out.String())
	}
	return out.String(), nil
}

func (f *GiteaFixture) RegistrationToken(ctx context.Context, repo string) (string, error) {
	var body struct {
		Token string `json:"token"`
	}
	path := "/api/v1/admin/actions/runners/registration-token"
	if repo != "" {
		path = fmt.Sprintf("/api/v1/repos/%s/%s/actions/runners/registration-token", giteaAdminUser, repo)
	}
	if err := f.doJSON(ctx, http.MethodPost, path, nil, &body); err != nil {
		return "", err
	}
	return body.Token, nil
}

func (f *GiteaFixture) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	api := &GiteaAPI{baseURL: f.baseURL, token: f.adminToken}
	return api.doJSON(ctx, method, path, reqBody, respBody)
}

func (f *GiteaFixture) Close(ctx context.Context) error {
	if f.id == "" {
		return f.cli.Close()
	}
	_, removeErr := f.cli.ContainerRemove(ctx, f.id, mobyclient.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return errors.Join(removeErr, f.cli.Close())
}
