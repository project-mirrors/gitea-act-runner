// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gitea.com/gitea/runner/act/artifactcache"
	clientmocks "gitea.com/gitea/runner/internal/pkg/client/mocks"
	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func emptyCfg() *config.Config { return &config.Config{} }

func TestRunner_registerCacheForTask_NoOps(t *testing.T) {
	for token, handler := range map[string]*artifactcache.Handler{"tok": nil, "": {}} {
		r := &Runner{cfg: emptyCfg(), cacheHandler: handler}
		unregister, _ := r.registerCacheForTask(token, "owner/repo", "", nil)
		require.NotNil(t, unregister, token)
		unregister()
	}
}

// Locks in @actions/cache's wire protocol: bearer on reserve/upload/commit
// /find, no auth on the signed archiveLocation download.
func TestRunner_CacheFullFlow_MatchesToolkit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := artifactcache.StartHandler(artifactcache.Options{Dir: dir, OutboundIP: "127.0.0.1"})
	require.NoError(t, err)
	defer handler.Close()

	r := &Runner{cfg: emptyCfg(), cacheHandler: handler, envs: map[string]string{"ACTIONS_RESULTS_URL": "https://gitea.example"}}
	const publicURL = "http://a1b2c3d4e5f6:8088"
	token := "full-flow-token"
	unregister, resultsURL := r.registerCacheForTask(token, "owner/repo", publicURL, nil)
	assert.Equal(t, publicURL, resultsURL)

	base := handler.ExternalURL() + "/_apis/artifactcache"
	do := func(method, url, contentType, contentRange, body string) *http.Response {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if contentRange != "" {
			req.Header.Set("Content-Range", contentRange)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	key := "toolkit-flow"
	version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
	body := `hello-cache-body`

	// reserve
	resp := do(http.MethodPost, base+"/caches", "application/json", "",
		fmt.Sprintf(`{"key":"%s","version":"%s","cacheSize":%d}`, key, version, len(body)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var reserved struct {
		CacheID uint64 `json:"cacheId"`
	}
	require.NoError(t, decodeJSON(resp, &reserved))
	require.NotZero(t, reserved.CacheID)

	// upload
	resp = do(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, reserved.CacheID),
		"application/octet-stream", fmt.Sprintf("bytes 0-%d/*", len(body)-1), body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// commit
	resp = do(http.MethodPost, fmt.Sprintf("%s/caches/%d", base, reserved.CacheID), "", "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// find — @actions/cache always sends comma-separated keys here
	resp = do(http.MethodGet,
		fmt.Sprintf("%s/cache?keys=%s,fallback&version=%s", base, key, version), "", "", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var hit struct {
		ArchiveLocation string `json:"archiveLocation"`
		CacheKey        string `json:"cacheKey"`
	}
	require.NoError(t, decodeJSON(resp, &hit))
	require.Equal(t, key, hit.CacheKey)
	require.True(t, strings.HasPrefix(hit.ArchiveLocation, publicURL+"/"), hit.ArchiveLocation)

	// download — toolkit does NOT attach Authorization here; the signature
	// in the URL must be enough.
	dl, err := http.Get(strings.Replace(hit.ArchiveLocation, publicURL, handler.ExternalURL(), 1))
	require.NoError(t, err)
	defer dl.Body.Close()
	require.Equal(t, http.StatusOK, dl.StatusCode)
	got := make([]byte, 64)
	n, _ := dl.Body.Read(got)
	assert.Equal(t, body, string(got[:n]))

	unregister()
	resp = do(http.MethodGet, fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version), "", "", "")
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.UnmarshalRead(resp.Body, v)
}

// End-to-end through a runner to a remote cache-server: token unknown → 401, register →
// reserve/upload/commit/find/download all OK, revoke → 401 again.
func TestRunner_ExternalCacheServer_RegisterRevoke(t *testing.T) {
	const secret = "shared-secret-for-tests"
	remote, err := artifactcache.StartHandler(artifactcache.Options{Dir: t.TempDir(), OutboundIP: "127.0.0.1", InternalSecret: secret})
	require.NoError(t, err)
	defer remote.Close()
	gitea := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", req.URL.Path)
		assert.Equal(t, "Bearer external-task-token", req.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer gitea.Close()

	cli := clientmocks.NewClient(t)
	cli.AddressValue = gitea.URL
	r := NewRunner(&config.Config{
		Runner: config.Runner{Insecure: true},
		Cache: config.Cache{
			Host:           "127.0.0.1",
			ExternalServer: remote.ExternalURL() + "//",
			ExternalSecret: secret,
		},
	}, &config.Registration{}, cli)
	t.Cleanup(func() { _ = r.Close() })
	require.NotNil(t, r.cacheHandler)
	require.Equal(t, r.cacheHandler.ExternalURL()+"/", r.envs["ACTIONS_CACHE_URL"])

	const publicURL = "http://a1b2c3d4e5f6:8088"
	token := "external-task-token"
	repo := "owner/repoX"
	base := r.envs["ACTIONS_CACHE_URL"] + "_apis/artifactcache"
	probe := func() int {
		req, _ := http.NewRequest(http.MethodGet, base+"/cache?keys=k&version=v", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	require.Equal(t, http.StatusUnauthorized, probe(),
		"token must be unknown to the remote server before registration")

	unregister, resultsURL := r.registerCacheForTask(token, repo, publicURL, nil)
	require.NotEqual(t, http.StatusUnauthorized, probe(),
		"token must be accepted after registerCacheForTask")

	require.Equal(t, publicURL, resultsURL)
	artifact, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		r.cacheHandler.ExternalURL()+"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", nil)
	require.NoError(t, err)
	artifact.Header.Set("Authorization", "Bearer "+token)
	forwarded, err := http.DefaultClient.Do(artifact)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, forwarded.StatusCode)
	var artifactResult map[string]bool
	require.NoError(t, decodeJSON(forwarded, &artifactResult))
	assert.True(t, artifactResult["ok"])

	// Full reserve→upload→commit→find→download cycle, identical to what
	// @actions/cache does, through the runner to the remote server.
	body := []byte("payload-from-task")
	reserveBody, _ := json.Marshal(&artifactcache.Request{Key: "ext-key", Version: "v", Size: int64(len(body))})
	req, _ := http.NewRequest(http.MethodPost, base+"/caches", bytes.NewReader(reserveBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var reserved struct {
		CacheID uint64 `json:"cacheId"`
	}
	require.NoError(t, decodeJSON(resp, &reserved))
	require.NotZero(t, reserved.CacheID)

	req, _ = http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, reserved.CacheID), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(body)-1))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, _ = http.NewRequest(http.MethodPost, fmt.Sprintf("%s/caches/%d", base, reserved.CacheID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, _ = http.NewRequest(http.MethodGet, base+"/cache?keys=ext-key&version=v", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var hit struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	require.NoError(t, decodeJSON(resp, &hit))
	require.True(t, strings.HasPrefix(hit.ArchiveLocation, publicURL+"/"), hit.ArchiveLocation)

	dl, err := http.Get(strings.Replace(hit.ArchiveLocation, publicURL, r.cacheHandler.ExternalURL(), 1))
	require.NoError(t, err)
	defer dl.Body.Close()
	require.Equal(t, http.StatusOK, dl.StatusCode)
	downloaded, err := io.ReadAll(dl.Body)
	require.NoError(t, err)
	assert.Equal(t, body, downloaded)

	unregister()
	assert.Equal(t, http.StatusUnauthorized, probe(),
		"token must be rejected after the revoker runs")

	forwarded, err = http.DefaultClient.Do(artifact)
	require.NoError(t, err)
	forwarded.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, forwarded.StatusCode)
}
