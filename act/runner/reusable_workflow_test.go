// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common/git"

	"gitea.dev/actionslib/pkg/model"
	"github.com/stretchr/testify/require"
)

// Regression test for go-gitea/gitea#37483: a remote reusable workflow at a moving
// ref (branch/tag) must reflect the new tip on every invocation, not stay pinned
// to the cache populated on the first run.
func TestReusableWorkflowCachedBranchRefRefreshes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH")
	}

	remoteDir := t.TempDir()
	gitMust(t, "", "init", "--bare", "--initial-branch=master", remoteDir)

	workDir := t.TempDir()
	gitMust(t, "", "clone", remoteDir, workDir)
	gitMust(t, workDir, "config", "user.email", "test@test")
	gitMust(t, workDir, "config", "user.name", "test")
	gitMust(t, workDir, "checkout", "-b", "master")

	const workflowPath = ".gitea/workflows/reusable.yml"
	tmpl := func(tag string) string {
		return "name: reusable\non:\n  workflow_call:\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo " + tag + "\n"
	}

	require.NoError(t, os.MkdirAll(filepath.Join(workDir, ".gitea/workflows"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, workflowPath), []byte(tmpl("v1")), 0o644))
	gitMust(t, workDir, "add", workflowPath)
	gitMust(t, workDir, "commit", "-m", "v1")
	gitMust(t, workDir, "push", "-u", "origin", "master")

	rc := &RunContext{
		Config: &Config{},
		Run: &model.Run{
			JobID: "j1",
			Workflow: &model.Workflow{
				Name: "wf",
				Jobs: map[string]*model.Job{"j1": {}},
			},
		},
	}
	cacheDir := t.TempDir()

	require.NoError(t, cloneRemoteReusableWorkflow(rc, remoteDir, "master", cacheDir)(context.Background()))
	got, err := os.ReadFile(filepath.Join(cacheDir, workflowPath))
	require.NoError(t, err)
	require.Equal(t, tmpl("v1"), string(got))

	// Branch tip moves; cache key (cacheDir) does not.
	require.NoError(t, os.WriteFile(filepath.Join(workDir, workflowPath), []byte(tmpl("v2")), 0o644))
	gitMust(t, workDir, "commit", "-am", "v2")
	gitMust(t, workDir, "push", "origin", "master")

	require.NoError(t, cloneRemoteReusableWorkflow(rc, remoteDir, "master", cacheDir)(context.Background()))
	got, err = os.ReadFile(filepath.Join(cacheDir, workflowPath))
	require.NoError(t, err)
	require.Equal(t, tmpl("v2"), string(got), "cached workflow file must reflect the updated branch tip")
}

func TestNewReusableWorkflowExecutorHoldsCloneLock(t *testing.T) {
	workflowDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workflowDir, "reusable.yml"), []byte(":"), 0o644))

	unlock, err := git.AcquireCloneLock(t.Context(), workflowDir)
	require.NoError(t, err)
	unlockOnce := sync.OnceFunc(unlock)
	defer unlockOnce()

	rc := &RunContext{
		Config: &Config{},
		Run:    &model.Run{Workflow: &model.Workflow{Jobs: map[string]*model.Job{}}},
	}
	exec := newReusableWorkflowExecutor(rc, workflowDir, "reusable.yml", true)

	done := make(chan error, 1)
	go func() { done <- exec(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("executor returned while clone lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	unlockOnce()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("executor did not return after lock was released")
	}
}

func TestNewLocalReusableWorkflowExecutorFindsSameRepositoryPaths(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(workdir, ".gitea", "workflows"), 0o755))
	require.NoError(t, os.Mkdir(workdir+".lock", 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workdir, ".gitea", "workflows", "reusable.yml"), []byte(":"), 0o644))

	for _, uses := range []string{"./.gitea/workflows/reusable.yml", "$/.gitea/workflows/reusable.yml"} {
		rc := &RunContext{
			Config: &Config{Workdir: workdir},
			Run:    &model.Run{JobID: "job", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"job": {Uses: uses}}}},
		}
		err := newLocalReusableWorkflowExecutor(rc)(t.Context())
		require.ErrorContains(t, err, "workflow is not valid")
	}
}

func TestCloneRemoteReusableWorkflowSendsInstanceTokenAndClientCert(t *testing.T) {
	var gotCert bool
	var gotToken string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCert = len(r.TLS.PeerCertificates) > 0
		_, gotToken, _ = r.BasicAuth()
		w.WriteHeader(http.StatusNotFound)
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	server.StartTLS()
	defer server.Close()
	pair := server.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	rc := &RunContext{
		Config: &Config{
			GitHubInstance:  server.URL,
			Secrets:         map[string]string{"GITEA_TOKEN": "token"},
			InsecureSkipTLS: true,
			ClientCertFile:  certFile,
			ClientKeyFile:   keyFile,
		},
		Run: &model.Run{JobID: "j1", Workflow: &model.Workflow{Jobs: map[string]*model.Job{"j1": {}}}},
	}

	require.Error(t, cloneRemoteReusableWorkflow(rc, server.URL+"/owner/repo", "main", t.TempDir())(t.Context()))
	require.True(t, gotCert)
	require.Equal(t, "token", gotToken)
}

func TestInstanceAuthAndTransportOnlyApplyToTheInstance(t *testing.T) {
	for _, tt := range []struct {
		name, instanceURL, trustedActionInstance, cloneURL string
		token, cert, insecure                              bool
	}{
		{"same host with schemaless instance", "gitea.example.net", "", "https://gitea.example.net/actions/tools", true, true, true},
		{"same host with schemaless instance and port", "gitea.example.net:3000", "", "https://gitea.example.net:3000/actions/tools", true, true, true},
		{"different host", "gitea.example.net", "", "https://github.com/actions/tools", false, false, false},
		{"embedded basic auth keeps its own credentials", "gitea.example.net", "", "https://user:pass@gitea.example.net/actions/tools", false, true, true},
		{"invalid clone URL", "gitea.example.net", "", "://gitea.example.net/actions/tools", false, false, false},
		{"self-hosted action instance on different host skips no verification", "gitea.local", "https://gitea.my-nas.lan", "https://gitea.my-nas.lan/owner/action", true, true, false},
		{"self-hosted action instance with embedded basic auth", "gitea.local", "https://gitea.my-nas.lan", "https://user:pass@gitea.my-nas.lan/owner/action", false, true, false},
		{"github mode does not trust mirror host", "gitea.local", "", "https://mirror.example.com/owner/action", false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conf := Config{
				GitHubInstance:                    tt.instanceURL,
				DefaultActionInstance:             tt.trustedActionInstance,
				DefaultActionInstanceIsSelfHosted: tt.trustedActionInstance != "",
				InsecureSkipTLS:                   true,
				ClientCertFile:                    "/cert",
				ClientKeyFile:                     "/key",
			}
			want := git.NewGitCloneExecutorInput{URL: tt.cloneURL, InsecureSkipTLS: tt.insecure}
			if tt.token {
				want.Token = "token"
			}
			if tt.cert {
				want.ClientCertFile, want.ClientKeyFile = conf.ClientCertFile, conf.ClientKeyFile
			}
			require.Equal(t, want, conf.withInstanceAuth(git.NewGitCloneExecutorInput{URL: tt.cloneURL}, "token"))
		})
	}

	transport := &http.Transport{}
	conf := Config{GitHubInstance: "https://gitea.local", InstanceTransport: transport}
	require.Same(t, transport, conf.instanceTransportFor("https://gitea.local/api/actions_pipeline/"))
	require.Nil(t, conf.instanceTransportFor("https://gitea.local.example/api/actions_pipeline/"))
	require.Nil(t, conf.instanceTransportFor("https://other.example/x#/api/actions_pipeline/"))
}

func gitMust(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
}
