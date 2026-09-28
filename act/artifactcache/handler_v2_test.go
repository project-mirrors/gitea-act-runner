// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v2Call posts a twirp request to the cache service and returns the decoded response.
// Field names are the proto ones, which is what the toolkit's client sends.
func v2Call(t *testing.T, handler *Handler, client *http.Client, method string, request any) map[string]any {
	t.Helper()

	body, err := json.Marshal(request)
	require.NoError(t, err)

	resp, err := client.Post(handler.ExternalURL()+cacheServiceV2Path+"/"+method, "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := map[string]any{}
	require.NoError(t, json.UnmarshalRead(resp.Body, &got))
	return got
}

// putBlob uploads to a signed URL and returns the status, so a test can assert a refusal.
func putBlob(t *testing.T, url string, content []byte) int {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(content))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		// The Azure SDK client dereferences this header without checking, so a blob upload that
		// omits it panics the caller rather than failing it.
		require.NotEmpty(t, resp.Header.Get("x-ms-request-id"))
	}
	return resp.StatusCode
}

func getURL(t *testing.T, url string) []byte {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body
}

// saveV2 runs the reserve/upload/finalize sequence and returns the finalize response along
// with the upload URL it used.
func saveV2(t *testing.T, handler *Handler, key, version string, content []byte) (finalized map[string]any, uploadURL string) {
	t.Helper()

	created := v2Call(t, handler, testClient, "CreateCacheEntry", map[string]any{"key": key, "version": version})
	require.Equal(t, true, created["ok"])
	uploadURL, _ = created["signed_upload_url"].(string)
	require.NotEmpty(t, uploadURL)
	require.Equal(t, http.StatusCreated, putBlob(t, uploadURL, content))

	return v2Call(t, handler, testClient, "FinalizeCacheEntryUpload", map[string]any{
		"key": key, "version": version,
		"size_bytes": strconv.Itoa(len(content)),
	}), uploadURL
}

// The whole round trip an actions/cache v2 client makes, plus the guarantees on the signed
// URLs it is handed: unsigned requests are refused, an upload URL cannot be replayed to read
// or to replace a finalized entry.
func TestCacheServiceV2RoundTrip(t *testing.T) {
	handler := newTestHandler(t, Policy{})
	content := []byte("the cached archive")

	unsigned := fmt.Sprintf("%s%s/1", handler.ExternalURL(), blobPath)
	assert.Equal(t, http.StatusUnauthorized, putBlob(t, unsigned, content))

	finalized, uploadURL := saveV2(t, handler, "deps-v1", "abc123", content)
	require.Equal(t, true, finalized["ok"])
	assert.NotEmpty(t, finalized["entry_id"])

	// The upload URL outlives the finalize call, so replaying it must not poison the entry,
	// and it is an upload URL only: nothing reads a blob back through it.
	assert.Equal(t, http.StatusBadRequest, putBlob(t, uploadURL, []byte("poisoned")))
	resp, err := http.Get(uploadURL) //nolint:noctx // the URL is the server under test
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)

	got := v2Call(t, handler, testClient, "GetCacheEntryDownloadURL", map[string]any{"key": "deps-v1", "version": "abc123"})
	require.Equal(t, true, got["ok"])
	assert.Equal(t, "deps-v1", got["matched_key"])
	downloadURL, _ := got["signed_download_url"].(string)
	require.NotEmpty(t, downloadURL)
	assert.Equal(t, content, getURL(t, downloadURL))
}

// A large archive is staged as blocks and only put in order by the final block list, so
// blocks that arrive out of order must still be assembled the way the client asked.
func TestCacheServiceV2BlockUpload(t *testing.T) {
	handler := newTestHandler(t, Policy{})

	created := v2Call(t, handler, testClient, "CreateCacheEntry", map[string]any{"key": "blocks", "version": "v1"})
	uploadURL, _ := created["signed_upload_url"].(string)
	require.NotEmpty(t, uploadURL)

	blocks := map[string][]byte{}
	var order []string
	for i, part := range []string{"hello ", "world", "!"} {
		blockID := base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "block-%d", i))
		blocks[blockID] = []byte(part)
		order = append(order, blockID)
	}
	// Upload in an order that is not the block list order.
	for _, blockID := range []string{order[2], order[0], order[1]} {
		require.Equal(t, http.StatusCreated, putBlob(t, uploadURL+"&comp=block&blockid="+blockID, blocks[blockID]))
	}

	var list bytes.Buffer
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, blockID := range order {
		fmt.Fprintf(&list, "<Latest>%s</Latest>", blockID)
	}
	list.WriteString(`</BlockList>`)
	require.Equal(t, http.StatusCreated, putBlob(t, uploadURL+"&comp=blocklist", list.Bytes()))
	require.Equal(t, http.StatusCreated, putBlob(t, uploadURL+"&comp=blocklist", list.Bytes()),
		"a client that lost the first answer retries the list it already sent")

	var reordered bytes.Buffer
	reordered.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, blockID := range []string{order[1], order[0], order[2]} {
		fmt.Fprintf(&reordered, "<Latest>%s</Latest>", blockID)
	}
	reordered.WriteString(`</BlockList>`)
	assert.Equal(t, http.StatusInternalServerError, putBlob(t, uploadURL+"&comp=blocklist", reordered.Bytes()),
		"the blocks are already assembled, so a different order would silently disagree with them")

	finalized := v2Call(t, handler, testClient, "FinalizeCacheEntryUpload", map[string]any{
		"key": "blocks", "version": "v1", "size_bytes": len("hello world!"),
	})
	require.Equal(t, true, finalized["ok"])

	got := v2Call(t, handler, testClient, "GetCacheEntryDownloadURL", map[string]any{"key": "blocks", "version": "v1"})
	require.Equal(t, true, got["ok"])
	assert.Equal(t, "hello world!", string(getURL(t, got["signed_download_url"].(string))))
}

func TestCacheServiceV2Lookups(t *testing.T) {
	handler := newTestHandler(t, Policy{})
	saved, _ := saveV2(t, handler, "deps-abc", "v1", []byte("x"))
	require.Equal(t, true, saved["ok"])

	t.Run("reports a miss for an unknown key", func(t *testing.T) {
		got := v2Call(t, handler, testClient, "GetCacheEntryDownloadURL", map[string]any{"key": "nothing", "version": "v1"})
		assert.Equal(t, false, got["ok"])
	})

	// The toolkit serialises with the proto field names; the camelCase spellings of the same
	// proto JSON mapping are accepted alongside them.
	for _, field := range []string{"restore_keys", "restoreKeys"} {
		t.Run("restore keys match by prefix, spelled "+field, func(t *testing.T) {
			got := v2Call(t, handler, testClient, "GetCacheEntryDownloadURL", map[string]any{
				"key": "deps-zzz", field: []string{"deps-"}, "version": "v1",
			})
			require.Equal(t, true, got["ok"])
			assert.Equal(t, "deps-abc", got["matched_key"])
		})
	}

	t.Run("an existing entry is not reserved twice", func(t *testing.T) {
		again := v2Call(t, handler, testClient, "CreateCacheEntry", map[string]any{"key": "deps-abc", "version": "v1"})
		assert.Equal(t, false, again["ok"])
	})

	// A key that is only a prefix of an existing one is a different entry, so the
	// reservation check must be exact and not a restore-key prefix match, or the shorter
	// key would be reported as existing and silently never saved.
	t.Run("a prefix of an existing key is still reserved", func(t *testing.T) {
		reserved := v2Call(t, handler, testClient, "CreateCacheEntry", map[string]any{"key": "deps", "version": "v1"})
		require.Equal(t, true, reserved["ok"])
		assert.NotEmpty(t, reserved["signed_upload_url"])
	})

	t.Run("a proxied job is handed the address its runner registered", func(t *testing.T) {
		const proxy = "https://cache.example.invalid"
		handler.RegisterJob("proxied", JobCredential{Repo: testRepo, PublicURL: proxy + "/"})
		client := &http.Client{Transport: &bearerTransport{token: "proxied"}}

		created := v2Call(t, handler, client, "CreateCacheEntry", map[string]any{"key": "proxied-key", "version": "v1"})
		assert.True(t, strings.HasPrefix(created["signed_upload_url"].(string), proxy+blobPath+"/"))

		got := v2Call(t, handler, client, "GetCacheEntryDownloadURL", map[string]any{"key": "deps-abc", "version": "v1"})
		assert.True(t, strings.HasPrefix(got["signed_download_url"].(string), proxy+apiPath+"/artifacts/"))
	})

	t.Run("finalizing without a reservation is not ok", func(t *testing.T) {
		got := v2Call(t, handler, testClient, "FinalizeCacheEntryUpload", map[string]any{
			"key": "never-reserved", "version": "v1", "size_bytes": 1,
		})
		assert.Equal(t, false, got["ok"])
	})

	// The size the client declares is what Commit validates the assembled archive against.
	t.Run("finalizing with the wrong size is not ok", func(t *testing.T) {
		created := v2Call(t, handler, testClient, "CreateCacheEntry", map[string]any{"key": "wrong-size", "version": "v1"})
		require.Equal(t, http.StatusCreated, putBlob(t, created["signed_upload_url"].(string), []byte("four")))

		got := v2Call(t, handler, testClient, "FinalizeCacheEntryUpload", map[string]any{
			"key": "wrong-size", "version": "v1", "size_bytes": 99,
		})
		assert.Equal(t, false, got["ok"])
	})

	// Both API versions are served from one store, so an entry written through v2 is a hit for
	// a v1 client asking for the same key and version.
	t.Run("a v1 client sees an entry written through v2", func(t *testing.T) {
		resp, err := testClient.Get(fmt.Sprintf("%s%s/cache?keys=deps-abc&version=v1", handler.ExternalURL(), apiPath))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		got := map[string]any{}
		require.NoError(t, json.UnmarshalRead(resp.Body, &got))
		assert.Equal(t, "deps-abc", got["cacheKey"])
		assert.NotEmpty(t, got["archiveLocation"])
	})

	// The cache of one repository must stay invisible to another, as it does for the v1 API.
	t.Run("another repository sees nothing", func(t *testing.T) {
		handler.RegisterJob("other-runtime-token", JobCredential{Repo: "other/repo"})
		otherClient := &http.Client{Transport: &bearerTransport{token: "other-runtime-token"}}

		got := v2Call(t, handler, otherClient, "GetCacheEntryDownloadURL", map[string]any{"key": "deps-abc", "version": "v1"})
		assert.Equal(t, false, got["ok"])
	})
}

func TestCacheServiceV2RefusalsAreTwirp(t *testing.T) {
	handler := newTestHandler(t, Policy{})

	tests := []struct {
		name   string
		token  string
		body   string
		status int
		code   string
	}{
		{"no bearer at all", "", `{"key":"k","version":"v"}`, http.StatusUnauthorized, twirpUnauthenticated},
		{"a bearer nobody registered", "not-a-job", `{"key":"k","version":"v"}`, http.StatusUnauthorized, twirpUnauthenticated},
		{"a body that is not the request", testToken, `{`, http.StatusBadRequest, "malformed"},
		{"a request missing its key", testToken, `{"version":"v"}`, http.StatusBadRequest, "invalid_argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				handler.ExternalURL()+cacheServiceV2Path+"/CreateCacheEntry", strings.NewReader(tt.body))
			require.NoError(t, err)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			require.Equal(t, tt.status, resp.StatusCode)
			got := map[string]string{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			assert.Equal(t, tt.code, got["code"])
			assert.NotEmpty(t, got["msg"])
		})
	}
}

func TestCacheServiceV2ConcurrentSaveAndRetries(t *testing.T) {
	handler := newTestHandler(t, Policy{})
	content := []byte("the cached archive")

	const otherJob = "another-jobs-token"
	defer handler.RegisterJob(otherJob, JobCredential{Repo: testRepo})()
	reserve := func(client *http.Client) map[string]any {
		return v2Call(t, handler, client, "CreateCacheEntry", map[string]any{"key": "shared", "version": "v1"})
	}
	first := reserve(testClient)
	require.Equal(t, true, first["ok"])
	retry := reserve(testClient)
	assert.Equal(t, true, retry["ok"], "a job retrying its own reservation is not a conflict")
	entryOf := func(reserved map[string]any) string {
		parsed, err := url.Parse(reserved["signed_upload_url"].(string))
		require.NoError(t, err)
		return parsed.Path
	}
	assert.Equal(t, entryOf(first), entryOf(retry), "the same reservation, however freshly its URL is signed")
	assert.Equal(t, false, reserve(&http.Client{Transport: &bearerTransport{token: otherJob}})["ok"],
		"another job's reservation would be finalized against the first upload")

	otherClient := &http.Client{Transport: &bearerTransport{token: otherJob}}
	reserved := v2Call(t, handler, otherClient, "CreateCacheEntry", map[string]any{"key": "owned", "version": "v1"})
	require.Equal(t, true, reserved["ok"])
	require.Equal(t, http.StatusCreated, putBlob(t, reserved["signed_upload_url"].(string), content))
	assert.Equal(t, false, v2Call(t, handler, testClient, "FinalizeCacheEntryUpload",
		map[string]any{"key": "owned", "version": "v1", "size_bytes": strconv.Itoa(len(content))})["ok"],
		"another job's upload is not this job's to finalize")

	base := handler.ExternalURL() + apiPath
	reserveV1, err := json.Marshal(&Request{Key: "v1-made", Version: "v1", Size: int64(len(content))})
	require.NoError(t, err)
	resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(reserveV1))
	require.NoError(t, err)
	var madeByV1 struct {
		CacheID uint64 `json:"cacheId"`
	}
	require.NoError(t, json.UnmarshalRead(resp.Body, &madeByV1))
	resp.Body.Close()
	upload, err := http.NewRequestWithContext(t.Context(), http.MethodPatch,
		fmt.Sprintf("%s/caches/%d", base, madeByV1.CacheID), bytes.NewReader(content))
	require.NoError(t, err)
	upload.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(content)-1))
	resp, err = testClient.Do(upload)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, false, v2Call(t, handler, testClient, "FinalizeCacheEntryUpload",
		map[string]any{"key": "v1-made", "version": "v1", "size_bytes": strconv.Itoa(len(content))})["ok"],
		"a reservation this job did not make through v2 is not its to commit")

	finalized, _ := saveV2(t, handler, "deps", "v1", content)
	require.Equal(t, true, finalized["ok"])
	again := v2Call(t, handler, testClient, "FinalizeCacheEntryUpload", map[string]any{
		"key": "deps", "version": "v1", "size_bytes": strconv.Itoa(len(content)),
	})
	assert.Equal(t, true, again["ok"], "a retry whose first answer was lost must not report a failed save")
	assert.Equal(t, finalized["entry_id"], again["entry_id"])
}
