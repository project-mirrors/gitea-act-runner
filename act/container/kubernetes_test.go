// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type podManifest struct {
	Metadata struct{ Labels map[string]string }
	Spec     struct {
		AutomountServiceAccountToken bool
		EnableServiceLinks           bool
		HostAliases                  []struct{ Hostnames []string }
		Volumes                      []map[string]any
		Containers                   []struct {
			Name, ImagePullPolicy string
			Resources             map[string]any
			StartupProbe          *struct{ PeriodSeconds, TimeoutSeconds, FailureThreshold int }
		}
	}
}

type fakeCluster struct {
	mu       sync.Mutex
	requests map[string][]string
}

func (f *fakeCluster) record(request, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[request] = append(f.requests[request], content)
}

func fakeKubernetes(t *testing.T, podStatus string) (string, *fakeCluster) {
	cluster := &fakeCluster{requests: map[string][]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		cluster.record(r.Method+" "+r.URL.Path, string(body))
		pods, secrets := "/api/v1/namespaces/ci/pods", "/api/v1/namespaces/ci/secrets"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == pods:
			cluster.record("list", r.URL.Query().Get("labelSelector"))
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"old","creationTimestamp":"2020-01-01T00:00:00Z"}},{"metadata":{"name":"new","creationTimestamp":"2999-01-01T00:00:00Z"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/exec"):
			cluster.record("exec", r.URL.RawQuery)
			serveLocalExec(t, w, r)
		case strings.HasSuffix(r.URL.Path, "/log"):
			_, _ = io.WriteString(w, "log of "+r.URL.Query().Get("container"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, pods+"/"):
			_, _ = io.WriteString(w, podStatus)
		case r.Method == http.MethodPost && (r.URL.Path == pods || r.URL.Path == secrets), r.Method == http.MethodDelete:
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(kubeconfig), "token"), []byte("test-token\n"), 0o600))
	require.NoError(t, os.WriteFile(kubeconfig, fmt.Appendf(nil, `
current-context: test
contexts: [{name: test, context: {cluster: test, user: test, namespace: ci}}]
clusters: [{name: test, cluster: {server: "http://kubernetes.invalid", proxy-url: %q}}]
users: [{name: test, user: {tokenFile: token}}]
`, server.URL), 0o600))
	return kubeconfig, cluster
}

func serveLocalExec(t *testing.T, w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{execProtocol}})
	if err != nil {
		t.Error(err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(-1)
	command := r.URL.Query()["command"]
	var stdin bytes.Buffer
	for r.URL.Query().Get("stdin") == "true" {
		_, frame, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		if frame[0] == 255 {
			break
		}
		stdin.Write(frame[1:])
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = &stdin, &stdout, &stderr
	status := `{"status":"Success"}`
	var exitErr *exec.ExitError
	if err := cmd.Run(); errors.As(err, &exitErr) {
		status = fmt.Sprintf(`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"%d"}]}}`, exitErr.ExitCode())
	} else {
		assert.NoError(t, err)
	}
	for _, frame := range [][]byte{append([]byte{1}, stdout.Bytes()...), append([]byte{2}, stderr.Bytes()...), append([]byte{3}, status...)} {
		_ = conn.Write(r.Context(), websocket.MessageBinary, frame)
	}
}

func TestKubernetesPodRunsJobThroughAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	ctx := t.Context()
	kubeconfig, cluster := fakeKubernetes(t, `{"spec":{"containers":[{},{}]},"status":{"phase":"Running","containerStatuses":[
		{"name":"job","ready":true,"state":{"running":{}}},
		{"name":"my-db","ready":true,"state":{"running":{}}}]}}`)
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	options := KubernetesOptions{Kubeconfig: kubeconfig, RunnerUUID: "runner-uuid", PodTemplates: []map[string]any{
		{"spec": map[string]any{
			"automountServiceAccountToken": true,
			"containers": []any{
				map[string]any{"name": "job", "resources": map[string]any{"limits": map[string]any{"cpu": "2"}}},
				map[string]any{
					"name": "$services", "imagePullPolicy": "Always", "resources": map[string]any{"limits": map[string]any{"memory": "256Mi"}},
					"env": []any{map[string]any{"name": "POSTGRES_PASSWORD", "value": "template"}},
				},
			},
		}},
		{"spec": map[string]any{
			"volumes":    []any{map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "1Gi"}}},
			"containers": []any{map[string]any{"name": "job", "resources": map[string]any{"limits": map[string]any{"memory": "1Gi"}}}},
		}},
	}}
	pod, err := NewKubernetesPod(options)
	require.NoError(t, err)
	var jobLog, serviceLog bytes.Buffer
	job := pod.JobContainer(&NewContainerInput{Name: "Job-Name", Image: "node", WorkingDir: workdir, Stdout: &jobLog, Stderr: &jobLog})
	service := pod.ServiceContainer("My_DB", &NewContainerInput{
		Image:           "postgres",
		Cmd:             []string{"postgres", "-D", "$(PGDATA)"},
		Env:             []string{"POSTGRES_PASSWORD=service-secret"},
		WorkflowOptions: "--health-cmd pg_isready --health-interval 2s --health-start-period 4s",
		Stdout:          &serviceLog,
	})

	require.NoError(t, job.Create(nil, nil)(ctx))
	var manifest podManifest
	require.NoError(t, json.Unmarshal([]byte(cluster.requests["POST /api/v1/namespaces/ci/pods"][0]), &manifest))
	assert.Equal(t, "gitea-runner", manifest.Metadata.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "runner-uuid", manifest.Metadata.Labels[runnerUUIDLabel])
	assert.True(t, manifest.Spec.AutomountServiceAccountToken)
	assert.False(t, manifest.Spec.EnableServiceLinks)
	assert.Equal(t, []string{"my-db"}, manifest.Spec.HostAliases[0].Hostnames)
	require.Len(t, manifest.Spec.Containers, 2)
	assert.Equal(t, map[string]any{"limits": map[string]any{"cpu": "2", "memory": "1Gi"}}, manifest.Spec.Containers[0].Resources)
	assert.Empty(t, manifest.Spec.Containers[0].ImagePullPolicy)
	assert.Equal(t, "my-db", manifest.Spec.Containers[1].Name)
	assert.Equal(t, "Always", manifest.Spec.Containers[1].ImagePullPolicy)
	assert.Equal(t, map[string]any{"limits": map[string]any{"memory": "256Mi"}}, manifest.Spec.Containers[1].Resources)
	assert.Equal(t, []map[string]any{
		{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
		{"name": "act", "emptyDir": map[string]any{}},
	}, manifest.Spec.Volumes)
	assert.Equal(t, struct{ PeriodSeconds, TimeoutSeconds, FailureThreshold int }{2, 30, 5}, *manifest.Spec.Containers[1].StartupProbe)
	assert.NotContains(t, cluster.requests["POST /api/v1/namespaces/ci/pods"][0], "service-secret")
	assert.Contains(t, cluster.requests["POST /api/v1/namespaces/ci/secrets"][0], "service-secret")
	assert.Contains(t, cluster.requests["POST /api/v1/namespaces/ci/secrets"][0], `"com.gitea.runner.uuid":"runner-uuid"`)
	assert.Contains(t, cluster.requests["POST /api/v1/namespaces/ci/pods"][0], `{"name":"POSTGRES_PASSWORD","valueFrom":{"secretKeyRef":{"key":"my-db.0","name":"`+pod.secretName+`"}}}`)
	assert.Equal(t, []any{"new"}, mergeManifest([]any{"old"}, []any{"new"}, ""))
	mount := func(path string) any { return map[string]any{"name": "config", "mountPath": path} }
	assert.Equal(t, []any{mount("/a"), mount("/b")}, mergeManifest([]any{mount("/a")}, []any{mount("/b")}, "volumeMounts"))
	tmpfs := map[string]any{"name": "workspace", "emptyDir": map[string]any{"medium": "Memory"}}
	ephemeral := map[string]any{"name": "workspace", "ephemeral": map[string]any{}}
	assert.Equal(t, []any{ephemeral}, mergeManifest([]any{tmpfs}, []any{ephemeral}, "volumes"))
	assert.Contains(t, cluster.requests["POST /api/v1/namespaces/ci/pods"][0], `"args":["postgres","-D","$$(PGDATA)"]`)

	require.NoError(t, os.WriteFile(filepath.Join(workdir, "env"), []byte("#!/bin/sh\necho \"$@\" > \"$0-args\"\nexec /usr/bin/env \"$@\"\n"), 0o755))
	t.Setenv("PATH", workdir+":"+os.Getenv("PATH"))
	require.NoError(t, job.Start(false)(ctx))

	stepEnv := map[string]string{"INPUT_WHO-TO-GREET": "it's\nme", "SECRET": "hunter2", "UID": "5"}
	require.NoError(t, job.Exec([]string{"printenv", "SECRET"}, stepEnv, "", "")(ctx))
	envArgs, err := os.ReadFile(filepath.Join(workdir, "env-args"))
	require.NoError(t, err)
	assert.NotContains(t, string(envArgs), "hunter2")
	require.NoError(t, job.Exec([]string{"printenv", "INPUT_WHO-TO-GREET"}, stepEnv, "", "")(ctx))
	require.NoError(t, job.Exec([]string{"printenv", "UID"}, stepEnv, "", "")(ctx))
	require.NoError(t, job.Exec([]string{"pwd", "-P"}, nil, "", workdir)(ctx))
	assert.Equal(t, "hunter2\nit's\nme\n5\n"+workdir+"\n", jobLog.String())
	assert.Equal(t, ExitCodeError(3), job.Exec([]string{"sh", "-c", "echo failing >&2; exit 3"}, nil, "", "")(ctx))
	assert.Contains(t, jobLog.String(), "failing\n")
	for _, query := range cluster.requests["exec"] {
		assert.NotContains(t, query, "hunter2")
	}

	require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "workflow/event.json", Mode: 0o644, Body: `{"ref":"main"}`})(ctx))
	archive, err := job.GetContainerArchive(ctx, workdir+"/act/workflow/event.json")
	require.NoError(t, err)
	reader := tar.NewReader(archive)
	header, err := reader.Next()
	require.NoError(t, err)
	content, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "event.json", header.Name)
	assert.JSONEq(t, `{"ref":"main"}`, string(content))
	require.NoError(t, archive.Close())
	_, err = job.GetContainerArchive(ctx, workdir+"/missing")
	require.Error(t, err)

	actionDir := filepath.Join(t.TempDir(), "action")
	require.NoError(t, os.MkdirAll(actionDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(actionDir, "index.js"), []byte("main()"), 0o644))
	require.NoError(t, job.CopyDir(workdir+"/act/actions/sha/", actionDir+"/", false, false)(ctx))
	script, err := os.ReadFile(workdir + "/act/actions/sha/index.js")
	require.NoError(t, err)
	assert.Equal(t, "main()", string(script))
	assert.Contains(t, cluster.requests["exec"][len(cluster.requests["exec"])-1], "command="+url.QueryEscape(workdir+"/act/actions/sha/"))

	pathEnv := map[string]string{}
	require.NoError(t, job.UpdateFromImageEnv(&pathEnv)(ctx))
	assert.Equal(t, os.Getenv("PATH"), pathEnv["PATH"])

	info, err := service.Inspect(ctx)
	require.NoError(t, err)
	assert.Equal(t, HealthHealthy, info.Health)
	require.NoError(t, service.DumpLogs(ctx))
	assert.Equal(t, "log of my-db", serviceLog.String())

	require.NoError(t, job.Remove()(ctx))
	assert.Len(t, cluster.requests["DELETE /api/v1/namespaces/ci/pods/"+pod.name], 1)
	assert.Len(t, cluster.requests["DELETE /api/v1/namespaces/ci/secrets/"+pod.secretName], 1)

	require.NoError(t, RemoveOrphanKubernetesResources(ctx, options, time.Now()))
	assert.Equal(t, []string{runnerUUIDLabel + "=runner-uuid"}, cluster.requests["list"])
	assert.Len(t, cluster.requests["DELETE /api/v1/namespaces/ci/pods/old"], 1)
	assert.Len(t, cluster.requests["DELETE /api/v1/namespaces/ci/secrets/old"], 1)
	assert.Empty(t, cluster.requests["DELETE /api/v1/namespaces/ci/pods/new"])
	assert.Empty(t, cluster.requests["DELETE /api/v1/namespaces/ci/secrets/new"])
}

func TestKubernetesPodFailsBeforeSteps(t *testing.T) {
	kubeconfig, _ := fakeKubernetes(t, `{"spec":{"containers":[{}]},"status":{"containerStatuses":[
		{"name":"job","state":{"waiting":{"reason":"ErrImagePull","message":"no such image"}}}]}}`)
	template := map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "sidecar"}}}}
	for _, test := range []struct {
		services     []string
		options, err string
	}{
		{[]string{"db"}, "--hostname db", "only --health-* options are supported"},
		{[]string{"sidecar"}, "", "named like another container"},
		{[]string{"my_db", "My-DB"}, "", `service My-DB maps to container "my-db"`},
	} {
		pod, err := NewKubernetesPod(KubernetesOptions{Kubeconfig: kubeconfig, Namespace: "ci", PodTemplates: []map[string]any{template}})
		require.NoError(t, err)
		job := pod.JobContainer(&NewContainerInput{Image: "node"})
		for _, service := range test.services {
			pod.ServiceContainer(service, &NewContainerInput{Image: "postgres", WorkflowOptions: test.options})
		}
		require.ErrorContains(t, job.Create(nil, nil)(t.Context()), test.err)
	}
	pod, err := NewKubernetesPod(KubernetesOptions{Kubeconfig: kubeconfig, Namespace: "ci"})
	require.NoError(t, err)
	job := pod.JobContainer(&NewContainerInput{Image: "missing"})
	require.NoError(t, job.Create(nil, nil)(t.Context()))
	require.ErrorContains(t, job.Start(false)(t.Context()), "ErrImagePull: no such image")

	plugin := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(plugin, []byte(`
current-context: eks
contexts: [{name: eks, context: {cluster: eks, user: eks}}]
clusters: [{name: eks, cluster: {server: "https://eks.example"}}]
users: [{name: eks, user: {exec: {command: aws}}}]
`), 0o600))
	_, err = NewKubernetesPod(KubernetesOptions{Kubeconfig: plugin})
	require.ErrorContains(t, err, "credential plugin")
}
