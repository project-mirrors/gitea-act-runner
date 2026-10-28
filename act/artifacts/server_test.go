// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"bytes"
	"compress/gzip"
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestArtifactFlow(t *testing.T) {
	artifactPath := t.TempDir()

	router := http.NewServeMux()
	uploads(router, artifactPath)
	downloads(router, artifactPath)
	registerV4(router, artifactPath)
	server := httptest.NewServer(router)
	defer server.Close()

	baseURL := server.URL
	client := server.Client()
	client.Timeout = 5 * time.Second

	request := func(t *testing.T, method, rawURL string, body io.Reader, header http.Header) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, rawURL, body)
		require.NoError(t, err)
		maps.Copy(req.Header, header)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, data
	}

	t.Run("no-artifacts", func(t *testing.T) {
		status, data := request(t, http.MethodGet, baseURL+"/_apis/pipelines/workflows/0/artifacts", nil, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		var list NamedFileContainerResourceURLResponse
		require.NoError(t, json.Unmarshal(data, &list))
		require.Zero(t, list.Count)
		require.Empty(t, list.Value)
	})

	t.Run("upload-and-download", func(t *testing.T) {
		const runID, item, content = "1", "my-artifact/data#1%25.txt", "hello artifact\n"

		status, data := request(t, http.MethodPost, baseURL+"/_apis/pipelines/workflows/"+runID+"/artifacts", nil, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		var prep FileContainerResourceURL
		require.NoError(t, json.Unmarshal(data, &prep))
		require.Equal(t, baseURL+"/upload/"+runID, prep.FileContainerResourceURL)

		status, data = request(t, http.MethodPut, prep.FileContainerResourceURL+"?itemPath="+url.QueryEscape(item), strings.NewReader(content), nil)
		require.Equal(t, http.StatusOK, status, string(data))
		var msg ResponseMessage
		require.NoError(t, json.Unmarshal(data, &msg))
		require.Equal(t, "success", msg.Message)

		status, data = request(t, http.MethodPatch, baseURL+"/_apis/pipelines/workflows/"+runID+"/artifacts", nil, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		require.NoError(t, json.Unmarshal(data, &msg))
		require.Equal(t, "success", msg.Message)

		status, data = request(t, http.MethodGet, baseURL+"/_apis/pipelines/workflows/"+runID+"/artifacts", nil, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		var list NamedFileContainerResourceURLResponse
		require.NoError(t, json.Unmarshal(data, &list))
		require.Equal(t, 1, list.Count)
		require.Equal(t, "my-artifact", list.Value[0].Name)

		status, data = request(t, http.MethodGet, list.Value[0].FileContainerResourceURL+"?itemPath=my-artifact", nil, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		var items ContainerItemResponse
		require.NoError(t, json.Unmarshal(data, &items))
		require.Len(t, items.Value, 1)
		require.Equal(t, "file", items.Value[0].ItemType)
		require.Equal(t, item, items.Value[0].Path)

		status, data = request(t, http.MethodGet, items.Value[0].ContentLocation, nil, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, content, string(data))

		stored, err := os.ReadFile(filepath.Join(artifactPath, runID, filepath.FromSlash(item)))
		require.NoError(t, err)
		require.Equal(t, content, string(stored))
	})

	t.Run("content-range", func(t *testing.T) {
		const rawURL = "/upload/4?itemPath=chunks.txt"
		status, data := request(t, http.MethodPut, baseURL+rawURL, strings.NewReader("first"),
			http.Header{"Content-Range": []string{"bytes 0-4/12"}})
		require.Equal(t, http.StatusOK, status, string(data))

		for range 2 {
			status, data = request(t, http.MethodPut, baseURL+rawURL, strings.NewReader("-second"),
				http.Header{"Content-Range": []string{"bytes 5-11/12"}})
			require.Equal(t, http.StatusOK, status, string(data))
		}

		stored, err := os.ReadFile(filepath.Join(artifactPath, "4", "chunks.txt"))
		require.NoError(t, err)
		require.Equal(t, "first-second", string(stored))

		status, data = request(t, http.MethodPut, baseURL+rawURL, strings.NewReader(""),
			http.Header{"Content-Range": []string{"bytes 0--1/0"}})
		require.Equal(t, http.StatusOK, status, string(data))
		stored, err = os.ReadFile(filepath.Join(artifactPath, "4", "chunks.txt"))
		require.NoError(t, err)
		require.Empty(t, stored)
	})

	t.Run("gzip-roundtrip", func(t *testing.T) {
		const runID, item, content = "2", "logs/app.log", "compressed payload\n"

		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, err := gz.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, gz.Close())

		status, data := request(t, http.MethodPut, baseURL+"/upload/"+runID+"?itemPath="+url.QueryEscape(item),
			&buf, http.Header{"Content-Encoding": []string{"gzip"}})
		require.Equal(t, http.StatusOK, status, string(data))

		// stored compressed, with the server's gzip marker suffix
		_, err = os.Stat(filepath.Join(artifactPath, runID, "logs", "app.log.gz__"))
		require.NoError(t, err)

		status, data = request(t, http.MethodGet, baseURL+"/download/"+runID+"?itemPath=logs", nil, nil)
		require.Equal(t, http.StatusOK, status, string(data))
		var items ContainerItemResponse
		require.NoError(t, json.Unmarshal(data, &items))
		require.Len(t, items.Value, 1)
		require.Equal(t, "logs/app.log", items.Value[0].Path)

		status, data = request(t, http.MethodGet, items.Value[0].ContentLocation, nil, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, content, string(data))
	})

	// GHSL-2023-004: an itemPath that climbs out of the run directory must be neutralised so the
	// blob cannot be written outside the artifact root.
	t.Run("GHSL-2023-004", func(t *testing.T) {
		const runID, content = "3", "contained\n"

		status, data := request(t, http.MethodPut, baseURL+"/upload/"+runID+"?itemPath="+url.QueryEscape("../../escape.txt"),
			strings.NewReader(content), nil)
		require.Equal(t, http.StatusOK, status, string(data))

		stored, err := os.ReadFile(filepath.Join(artifactPath, runID, "escape.txt"))
		require.NoError(t, err)
		require.Equal(t, content, string(stored))

		_, err = os.Stat(filepath.Join(filepath.Dir(artifactPath), "escape.txt"))
		require.True(t, os.IsNotExist(err), "upload escaped the artifact root")

		status, data = request(t, http.MethodGet, baseURL+"/artifact/"+runID+"/escape.txt", nil, nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, content, string(data))
	})

	t.Run("v4", func(t *testing.T) {
		const artifactRef = `"workflow_run_backend_id":"7","name":"50% art@v1"`
		call := func(method, body string) (int, map[string]any) {
			status, data := request(t, http.MethodPost, baseURL+artifactServicePath+"/"+method, strings.NewReader(body), nil)
			var out map[string]any
			require.NoError(t, json.Unmarshal(data, &out), string(data))
			return status, out
		}
		put := func(rawURL, body string) int {
			status, _ := request(t, http.MethodPut, rawURL, strings.NewReader(body), nil)
			return status
		}
		download := func() (string, http.Header) {
			status, signed := call("GetSignedArtifactURL", "{"+artifactRef+"}")
			require.Equal(t, http.StatusOK, status, signed)
			resp, err := client.Get(signed["signedUrl"].(string))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return string(body), resp.Header
		}

		status, created := call("CreateArtifact", "{"+artifactRef+"}")
		require.Equal(t, http.StatusOK, status, created)
		upload := created["signedUploadUrl"].(string)
		require.Equal(t, http.StatusCreated, put(upload+"&comp=block&blockid=Yg", "world"))
		require.Equal(t, http.StatusCreated, put(upload+"&comp=block&blockid=YQ", "hello "))
		for attempt := range 2 {
			require.Equal(t, http.StatusCreated, put(upload+"&comp=blocklist", `<?xml version="1.0"?><BlockList><Latest>YQ</Latest><Latest>Yg</Latest></BlockList>`), "commit attempt %d", attempt)
		}

		digest := "sha256:b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
		status, _ = call("FinalizeArtifact", "{"+artifactRef+`,"size":"10"}`)
		require.Equal(t, http.StatusBadRequest, status)
		status, finalized := call("FinalizeArtifact", "{"+artifactRef+`,"size":"11","hash":"`+digest+`"}`)
		require.Equal(t, http.StatusOK, status, finalized)
		id := finalized["artifactId"].(string)
		status, refinalized := call("FinalizeArtifact", "{"+artifactRef+`,"size":"10"}`)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, id, refinalized["artifactId"])
		status, conflict := call("CreateArtifact", "{"+artifactRef+"}")
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, "already_exists", conflict["code"])

		status, listed := call("ListArtifacts", `{"workflowRunBackendId":"7","idFilter":"`+id+`"}`)
		require.Equal(t, http.StatusOK, status)
		artifacts := listed["artifacts"].([]any)
		require.Len(t, artifacts, 1)
		artifact := artifacts[0].(map[string]any)
		require.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`, artifact["createdAt"])
		delete(artifact, "createdAt")
		require.Equal(t, map[string]any{"workflowRunBackendId": "7", "name": "50% art@v1", "size": "11", "digest": digest, "databaseId": id}, artifact)
		_, other := call("ListArtifacts", `{"workflow_run_backend_id":"8"}`)
		require.Empty(t, other["artifacts"])

		body, header := download()
		require.Equal(t, "hello world", body)
		require.Equal(t, "application/zip", header.Get("Content-Type"))
		require.Equal(t, "attachment; filename*=UTF-8''50%25%20art%40v1.zip", header.Get("Content-Disposition"))

		status, _ = call("DeleteArtifact", "{"+artifactRef+"}")
		require.Equal(t, http.StatusOK, status)
		status, missing := call("GetSignedArtifactURL", "{"+artifactRef+"}")
		require.Equal(t, http.StatusNotFound, status)
		require.Equal(t, "not_found", missing["code"])

		status, created = call("CreateArtifact", "{"+artifactRef+"}")
		require.Equal(t, http.StatusOK, status, created)
		status, _ = request(t, http.MethodPut, created["signedUploadUrl"].(string), strings.NewReader("gz"), http.Header{"X-Ms-Blob-Content-Type": {"application/gzip"}})
		require.Equal(t, http.StatusCreated, status)
		status, _ = call("FinalizeArtifact", "{"+artifactRef+`,"size":"2"}`)
		require.Equal(t, http.StatusOK, status)
		body, header = download()
		require.Equal(t, "gz", body)
		require.Equal(t, "application/gzip", header.Get("Content-Type"))
		require.Equal(t, "attachment; filename*=UTF-8''50%25%20art%40v1", header.Get("Content-Disposition"))

		rerun := http.NewServeMux()
		registerV4(rerun, artifactPath)
		recorder := httptest.NewRecorder()
		rerun.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, artifactServicePath+"/CreateArtifact", strings.NewReader("{"+artifactRef+"}")))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	})
}

func TestSafeResolve(t *testing.T) {
	baseDir := "/foo/bar"

	tests := map[string]struct {
		input string
		want  string
	}{
		"simple":         {input: "baz", want: "/foo/bar/baz"},
		"nested":         {input: "baz/blue", want: "/foo/bar/baz/blue"},
		"dots in middle": {input: "baz/../../blue", want: "/foo/bar/blue"},
		"leading dots":   {input: "../../parent", want: "/foo/bar/parent"},
		"root path":      {input: "/root", want: "/foo/bar/root"},
		"root":           {input: "/", want: "/foo/bar"},
		"empty":          {input: "", want: "/foo/bar"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, safeResolve(baseDir, tc.input))
		})
	}
}

func TestServeEmptyArtifactPathReturnsCancelableNoop(t *testing.T) {
	cancel := Serve(t.Context(), "", "127.0.0.1", "0")
	require.NotNil(t, cancel)
	cancel()
}
