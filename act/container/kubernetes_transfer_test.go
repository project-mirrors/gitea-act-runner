// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const transferHelperEnv = "GITEA_RUNNER_TEST_TRANSFER_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(transferHelperEnv) == "1" && len(os.Args) == 4 && os.Args[1] == KubernetesTransferCommand && os.Args[2] == "fetch" {
		if err := FetchKubernetesTransfer(context.Background(), os.Stdin, os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fetch(t *testing.T, request transferRequest) error {
	t.Helper()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	return FetchKubernetesTransfer(t.Context(), bytes.NewReader(body), t.TempDir())
}

func TestTransferServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("extracts with tar")
	}
	server, err := startTransferServer("127.0.0.1", 0)
	require.NoError(t, err)
	again, err := startTransferServer("127.0.0.1", 0)
	require.NoError(t, err)
	assert.Same(t, server, again)
	loopback := []net.IP{net.ParseIP("127.0.0.1")}
	nothing := func(io.Writer) error { return nil }

	t.Run("serves an offer gzipped once to an IP of its job pod", func(t *testing.T) {
		request, done := server.offer([]net.IP{net.ParseIP("fd00::1"), loopback[0]}, nothing)
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableCompression: true}}
		response, err := client.Get(request.URL)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, "gzip", response.Header.Get("Content-Encoding"))
		require.ErrorContains(t, fetch(t, request), "404")
		taken, _ := done()
		assert.True(t, taken)
	})

	t.Run("another pod can neither take nor block an offer", func(t *testing.T) {
		request, done := server.offer([]net.IP{net.ParseIP("10.1.2.3")}, nothing)
		require.ErrorContains(t, fetch(t, request), "404")
		taken, _ := done()
		assert.False(t, taken)
	})

	t.Run("a failed write fails the fetch even past the end blocks tar stops at, and done reports it", func(t *testing.T) {
		request, done := server.offer(loopback, func(w io.Writer) error {
			_, _ = w.Write(make([]byte, 1024))
			_, _ = io.CopyN(w, rand.Reader, 1<<20)
			return errors.New("source gone")
		})
		require.Error(t, fetch(t, request))
		taken, err := done()
		assert.True(t, taken)
		require.EqualError(t, err, "source gone")
	})

	t.Run("the helper refuses a runner whose certificate does not match its pin", func(t *testing.T) {
		request, done := server.offer(loopback, nothing)
		request.Pin = strings.Repeat("0", 64)
		require.ErrorContains(t, fetch(t, request), "does not match the pin")
		taken, _ := done()
		assert.False(t, taken)
	})
}

func TestKubernetesPodTransferImage(t *testing.T) {
	newPod := func(image string, fallback bool) *KubernetesPod {
		pod := &KubernetesPod{options: KubernetesOptions{Transfer: &KubernetesTransfer{Host: "127.0.0.1", Image: image, Fallback: fallback}}}
		pod.JobContainer(&NewContainerInput{})
		return pod
	}

	pod := newPod("", false)
	require.NoError(t, pod.setupTransfer("gitea/runner:latest"))
	assert.Equal(t, "gitea/runner:latest", pod.transferImage)

	pod = newPod("registry.example/runner:1", false)
	require.NoError(t, pod.setupTransfer("gitea/runner:latest"))
	assert.Equal(t, "registry.example/runner:1", pod.transferImage)

	pod = newPod("", true)
	require.NoError(t, pod.setupTransfer(""))
	assert.Empty(t, pod.transferHelper)

	pod = newPod("", false)
	require.ErrorContains(t, pod.setupTransfer(""), "kubernetes.transfer.image")
}

func TestKubernetesPodCopiesLargeFilesOverPodNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	ctx := t.Context()
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	kubeconfig, cluster := fakeKubernetes(t, `{"spec":{"containers":[{}]},"status":{"phase":"Running","podIPs":[{"ip":"127.0.0.1"}],"containerStatuses":[
		{"name":"job","ready":true,"state":{"running":{}}}]}}`, transferHelperEnv+"=1")
	pod, err := NewKubernetesPod(KubernetesOptions{Kubeconfig: kubeconfig, ForcePull: true, Transfer: &KubernetesTransfer{Host: "127.0.0.1", Image: "gitea/runner:latest", Fallback: true}})
	require.NoError(t, err)
	job := pod.JobContainer(&NewContainerInput{Name: "job", Image: "node", WorkingDir: workdir, Stdout: io.Discard, Stderr: io.Discard})
	require.NoError(t, job.Create(nil, nil)(ctx))

	var manifest struct {
		Spec struct {
			InitContainers []struct {
				Image, ImagePullPolicy string
				Command                []string
				VolumeMounts           []struct{ Name, MountPath string }
			}
		}
	}
	require.NoError(t, json.Unmarshal([]byte(cluster.requests["POST /api/v1/namespaces/ci/pods"][0]), &manifest))
	require.Len(t, manifest.Spec.InitContainers, 1)
	initContainer := manifest.Spec.InitContainers[0]
	assert.Equal(t, "gitea/runner:latest", initContainer.Image)
	assert.Equal(t, "Always", initContainer.ImagePullPolicy)
	assert.Equal(t, []string{"gitea-runner", KubernetesTransferCommand, "install", "/var/run/act/bin/gitea-runner"}, initContainer.Command)
	assert.Equal(t, []struct{ Name, MountPath string }{{"act", "/var/run/act"}}, initContainer.VolumeMounts)
	require.NoError(t, job.Start(false)(ctx))

	helper := filepath.Join(t.TempDir(), "bin", "gitea-runner")
	require.NoError(t, InstallKubernetesTransferHelper(helper))
	pod.transferHelper = helper
	lastExec := func() string { return cluster.requests["exec"][len(cluster.requests["exec"])-1] }
	large := &FileEntry{Name: "large", Mode: 0o644, Body: strings.Repeat("a", compressCopyMin)}
	copyLarge := func(dir string) error { return job.Copy(filepath.Join(workdir, dir)+"/", large)(ctx) }
	require.NoError(t, copyLarge("transferred"))
	content, err := os.ReadFile(filepath.Join(workdir, "transferred", "large"))
	require.NoError(t, err)
	assert.Equal(t, large.Body, string(content))
	assert.Contains(t, lastExec(), "command="+KubernetesTransferCommand)
	assert.NotContains(t, lastExec(), "127.0.0.1")

	require.NoError(t, job.Copy(workdir+"/act/", &FileEntry{Name: "event.json", Mode: 0o644, Body: `{}`})(ctx))
	assert.NotContains(t, lastExec(), "command="+KubernetesTransferCommand)

	require.NoError(t, os.WriteFile(filepath.Join(workdir, "file"), nil, 0o644))
	require.ErrorContains(t, copyLarge("file"), "over the pod network")
	assert.Equal(t, helper, pod.transferHelper)

	unreachable := filepath.Join(workdir, "unreachable")
	require.NoError(t, os.WriteFile(unreachable, []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"), 0o755))
	pod.transferHelper = unreachable
	pod.options.Transfer.Fallback = false
	require.ErrorContains(t, copyLarge("direct"), "connection refused")
	assert.NoFileExists(t, filepath.Join(workdir, "direct", "large"))

	pod.options.Transfer.Fallback = true
	require.NoError(t, copyLarge("auto"))
	assert.FileExists(t, filepath.Join(workdir, "auto", "large"))
	assert.Empty(t, pod.transferHelper)
}
