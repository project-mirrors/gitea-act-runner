// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2023 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"bytes"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"
)

// testToken is registered with the cache server in every test that needs to
// make authenticated requests; testClient then attaches it as the
// Authorization: Bearer header. testRepo is the repository scope used when
// registering it; cross-repo isolation is exercised in its own test.
const (
	testToken = "test-runtime-token"
	testRepo  = "owner/repo"
)

type bearerTransport struct{ token string }

func (b *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

var testClient = &http.Client{Transport: &bearerTransport{token: testToken}}

// testRetention mirrors config.DefaultCacheRetention; Policy has no defaults of its own.
const testRetention = 7 * 24 * time.Hour

// signArtifactURL builds a signed download URL the same way the server does;
// tests use it to reach the get handler directly without going through a
// find/cache-hit round trip.
func signArtifactURL(h *Handler, id int64) string {
	return h.signedArtifactURL(JobCredential{}, uint64(id), time.Now().Add(artifactURLTTL))
}

func TestHandler_ExternalURL(t *testing.T) {
	assert.Equal(t, "http://[2001:db8::1]:8080", (&Handler{outboundIP: "2001:db8::1", port: 8080}).ExternalURL())
}

func TestHandler(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})

	base := fmt.Sprintf("%s%s", handler.ExternalURL(), apiPath)

	defer func() {
		t.Run("inpect db", func(t *testing.T) {
			db, err := handler.openDB()
			require.NoError(t, err)
			defer db.Close()
			require.NoError(t, db.Bolt().View(func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte("Cache")).ForEach(func(k, v []byte) error {
					t.Logf("%s: %s", k, v)
					return nil
				})
			}))
		})
		t.Run("close", func(t *testing.T) {
			require.NoError(t, handler.Close())
			assert.Nil(t, handler.server)
			assert.Nil(t, handler.listener)
			resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, 1), "", nil)
			if err == nil {
				resp.Body.Close()
			}
			assert.Error(t, err)
		})
	}()

	t.Run("get not exist", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 204, resp.StatusCode)
	})

	t.Run("reserve and upload", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		uploadCacheNormally(t, base, key, version, content)
	})

	t.Run("clean", func(t *testing.T) {
		resp, err := testClient.Post(base+"/clean", "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 200, resp.StatusCode)
	})

	t.Run("reserve with bad request", func(t *testing.T) {
		body := []byte(`invalid json`)
		require.NoError(t, err)
		resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 400, resp.StatusCode)
	})

	t.Run("duplicate reserve", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		var first, second struct {
			CacheID uint64 `json:"cacheId"`
		}
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)

			require.NoError(t, json.UnmarshalRead(resp.Body, &first))
			assert.NotZero(t, first.CacheID)
		}
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)

			require.NoError(t, json.UnmarshalRead(resp.Body, &second))
			assert.NotZero(t, second.CacheID)
		}

		assert.NotEqual(t, first.CacheID, second.CacheID)
	})

	t.Run("upload with bad id", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPatch,
			base+"/caches/invalid_id", bytes.NewReader(nil))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", "bytes 0-99/*")
		resp, err := testClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 400, resp.StatusCode)
	})

	t.Run("upload without reserve", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPatch,
			fmt.Sprintf("%s/caches/%d", base, 1000), bytes.NewReader(nil))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", "bytes 0-99/*")
		resp, err := testClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 400, resp.StatusCode)
	})

	t.Run("upload with complete", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		var id uint64
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)

			got := struct {
				CacheID uint64 `json:"cacheId"`
			}{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			id = got.CacheID
		}
		{
			req, err := http.NewRequest(http.MethodPatch,
				fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", "bytes 0-99/*")
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)
		}
		{
			resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)
		}
		{
			req, err := http.NewRequest(http.MethodPatch,
				fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", "bytes 0-99/*")
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		}
	})

	t.Run("upload with invalid range", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		var id uint64
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)

			got := struct {
				CacheID uint64 `json:"cacheId"`
			}{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			id = got.CacheID
		}
		{
			req, err := http.NewRequest(http.MethodPatch,
				fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", "bytes xx-99/*")
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		}
	})

	t.Run("commit with bad id", func(t *testing.T) {
		{
			resp, err := testClient.Post(base+"/caches/invalid_id", "", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		}
	})

	t.Run("commit with not exist id", func(t *testing.T) {
		{
			resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, 100), "", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		}
	})

	t.Run("duplicate commit", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		var id uint64
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)

			got := struct {
				CacheID uint64 `json:"cacheId"`
			}{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			id = got.CacheID
		}
		{
			req, err := http.NewRequest(http.MethodPatch,
				fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", "bytes 0-99/*")
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)
		}
		{
			resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)
		}
		{
			resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		}
	})

	t.Run("upload write failure returns only error", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		var id uint64
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)

			got := struct {
				CacheID uint64 `json:"cacheId"`
			}{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			id = got.CacheID
		}

		storageFile := filepath.Join(dir, "not-a-directory")
		require.NoError(t, os.WriteFile(storageFile, []byte("blocked"), 0o600))
		originalStorage := handler.storage
		handler.storage = &Storage{rootDir: storageFile}
		defer func() {
			handler.storage = originalStorage
		}()

		req, err := http.NewRequest(http.MethodPatch,
			fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(make([]byte, 100)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", "bytes 0-99/*")
		resp, err := testClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 500, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var got map[string]string
		require.NoError(t, json.Unmarshal(body, &got))
		assert.NotEmpty(t, got["error"])
	})

	t.Run("commit early", func(t *testing.T) {
		key := strings.ToLower(t.Name())
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		var id uint64
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		{
			body, err := json.Marshal(&Request{
				Key:     key,
				Version: version,
				Size:    100,
			})
			require.NoError(t, err)
			resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)

			got := struct {
				CacheID uint64 `json:"cacheId"`
			}{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			id = got.CacheID
		}
		{
			req, err := http.NewRequest(http.MethodPatch,
				fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content[:50]))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", "bytes 0-59/*")
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 200, resp.StatusCode)
		}
		{
			resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 500, resp.StatusCode)
		}
	})

	t.Run("get with bad id", func(t *testing.T) {
		resp, err := testClient.Get(base + "/artifacts/invalid_id")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 400, resp.StatusCode)
	})

	t.Run("get with not exist id", func(t *testing.T) {
		resp, err := testClient.Get(signArtifactURL(handler, 100))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 404, resp.StatusCode)
	})

	t.Run("get with multiple keys", func(t *testing.T) {
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		key := strings.ToLower(t.Name())
		keys := [3]string{
			key + "_a_b_c",
			key + "_a_b",
			key + "_a",
		}
		contents := [3][]byte{
			make([]byte, 100),
			make([]byte, 200),
			make([]byte, 300),
		}
		for i := range contents {
			_, err := rand.Read(contents[i])
			require.NoError(t, err)
			uploadCacheNormally(t, base, keys[i], version, contents[i])
			// ensure CreatedAt of caches are different, in upload order
			backdateCache(t, handler, keys[i], time.Duration(len(contents)-i)*time.Second)
		}

		reqKeys := strings.Join([]string{
			key + "_a_b_x",
			key + "_a_b",
			key + "_a",
		}, ",")

		resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, reqKeys, version))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)

		/*
			Expect `key_a_b` because:
			- `key_a_b_x" doesn't match any caches.
			- `key_a_b" matches `key_a_b` and `key_a_b_c`, but `key_a_b` is newer.
		*/
		except := 1

		got := struct {
			Result          string `json:"result"`
			ArchiveLocation string `json:"archiveLocation"`
			CacheKey        string `json:"cacheKey"`
		}{}
		require.NoError(t, json.UnmarshalRead(resp.Body, &got))
		assert.Equal(t, "hit", got.Result)
		assert.Equal(t, keys[except], got.CacheKey)

		contentResp, err := testClient.Get(got.ArchiveLocation)
		require.NoError(t, err)
		defer contentResp.Body.Close()
		require.Equal(t, 200, contentResp.StatusCode)
		content, err := io.ReadAll(contentResp.Body)
		require.NoError(t, err)
		assert.Equal(t, contents[except], content)
	})

	t.Run("case preserved", func(t *testing.T) {
		// Some actions (e.g. actions/setup-go, actions/setup-node) build cache keys that contain mixed-case fragments such as RUNNER_OS=Linux,
		// then compare the cacheKey returned by the cache server to their original key with case-sensitive equality to decide whether the
		// cache was a complete hit. The server must therefore preserve the original key case.

		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		key := strings.ToLower(t.Name()) + "_ABC"
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		uploadCacheNormally(t, base, key, version, content)

		{
			resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			got := struct {
				Result          string `json:"result"`
				ArchiveLocation string `json:"archiveLocation"`
				CacheKey        string `json:"cacheKey"`
			}{}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			assert.Equal(t, "hit", got.Result)
			assert.Equal(t, key, got.CacheKey)
			assert.NotEqual(t, strings.ToLower(key), got.CacheKey)
		}
	})

	t.Run("exact keys are preferred (key 0)", func(t *testing.T) {
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		key := strings.ToLower(t.Name())
		keys := [3]string{
			key + "_a",
			key + "_a_b_c",
			key + "_a_b",
		}
		contents := [3][]byte{
			make([]byte, 100),
			make([]byte, 200),
			make([]byte, 300),
		}
		for i := range contents {
			_, err := rand.Read(contents[i])
			require.NoError(t, err)
			uploadCacheNormally(t, base, keys[i], version, contents[i])
			// ensure CreatedAt of caches are different, in upload order
			backdateCache(t, handler, keys[i], time.Duration(len(contents)-i)*time.Second)
		}

		reqKeys := strings.Join([]string{
			key + "_a",
			key + "_a_b",
		}, ",")

		resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, reqKeys, version))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)

		/*
			Expect `key_a` because:
			- `key_a` matches `key_a`, `key_a_b` and `key_a_b_c`, but `key_a` is an exact match.
			- `key_a_b` matches `key_a_b` and `key_a_b_c`, but previous key had a match
		*/
		expect := 0

		got := struct {
			ArchiveLocation string `json:"archiveLocation"`
			CacheKey        string `json:"cacheKey"`
		}{}
		require.NoError(t, json.UnmarshalRead(resp.Body, &got))
		assert.Equal(t, keys[expect], got.CacheKey)

		contentResp, err := testClient.Get(got.ArchiveLocation)
		require.NoError(t, err)
		defer contentResp.Body.Close()
		require.Equal(t, 200, contentResp.StatusCode)
		content, err := io.ReadAll(contentResp.Body)
		require.NoError(t, err)
		assert.Equal(t, contents[expect], content)
	})

	t.Run("exact keys are preferred (key 1)", func(t *testing.T) {
		version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
		key := strings.ToLower(t.Name())
		keys := [3]string{
			key + "_a",
			key + "_a_b_c",
			key + "_a_b",
		}
		contents := [3][]byte{
			make([]byte, 100),
			make([]byte, 200),
			make([]byte, 300),
		}
		for i := range contents {
			_, err := rand.Read(contents[i])
			require.NoError(t, err)
			uploadCacheNormally(t, base, keys[i], version, contents[i])
			// ensure CreatedAt of caches are different, in upload order
			backdateCache(t, handler, keys[i], time.Duration(len(contents)-i)*time.Second)
		}

		reqKeys := strings.Join([]string{
			"------------------------------------------------------",
			key + "_a",
			key + "_a_b",
		}, ",")

		resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, reqKeys, version))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)

		/*
			Expect `key_a` because:
			- `------------------------------------------------------` doesn't match any caches.
			- `key_a` matches `key_a`, `key_a_b` and `key_a_b_c`, but `key_a` is an exact match.
			- `key_a_b` matches `key_a_b` and `key_a_b_c`, but previous key had a match
		*/
		expect := 0

		got := struct {
			ArchiveLocation string `json:"archiveLocation"`
			CacheKey        string `json:"cacheKey"`
		}{}
		require.NoError(t, json.UnmarshalRead(resp.Body, &got))
		assert.Equal(t, keys[expect], got.CacheKey)

		contentResp, err := testClient.Get(got.ArchiveLocation)
		require.NoError(t, err)
		defer contentResp.Body.Close()
		require.Equal(t, 200, contentResp.StatusCode)
		content, err := io.ReadAll(contentResp.Body)
		require.NoError(t, err)
		assert.Equal(t, contents[expect], content)
	})
}

// backdateCache rewrites a cache's CreatedAt. It has one-second resolution, so age-ordering
// tests set it directly instead of sleeping a second between uploads.
func backdateCache(t *testing.T, handler *Handler, key string, age time.Duration) {
	db, err := handler.openDB()
	require.NoError(t, err)
	defer db.Close()

	var caches []*Cache
	require.NoError(t, db.Find(&caches, bolthold.Where("Key").Eq(key)))
	require.Len(t, caches, 1)
	caches[0].CreatedAt = time.Now().Add(-age).Unix()
	require.NoError(t, db.Update(caches[0].ID, caches[0]))
}

func uploadCacheNormally(t *testing.T, base, key, version string, content []byte) {
	var id uint64
	{
		body, err := json.Marshal(&Request{
			Key:     key,
			Version: version,
			Size:    int64(len(content)),
		})
		require.NoError(t, err)
		resp, err := testClient.Post(base+"/caches", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 200, resp.StatusCode)

		got := struct {
			CacheID uint64 `json:"cacheId"`
		}{}
		require.NoError(t, json.UnmarshalRead(resp.Body, &got))
		id = got.CacheID
	}
	{
		req, err := http.NewRequest(http.MethodPatch,
			fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", "bytes 0-99/*")
		resp, err := testClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 200, resp.StatusCode)
	}
	{
		resp, err := testClient.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 200, resp.StatusCode)
	}
	var archiveLocation string
	{
		resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		got := struct {
			Result          string `json:"result"`
			ArchiveLocation string `json:"archiveLocation"`
			CacheKey        string `json:"cacheKey"`
		}{}
		require.NoError(t, json.UnmarshalRead(resp.Body, &got))
		assert.Equal(t, "hit", got.Result)
		assert.Equal(t, key, got.CacheKey)
		archiveLocation = got.ArchiveLocation
	}
	{
		resp, err := testClient.Get(archiveLocation)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		got, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, content, got)
	}
}

func TestHandler_gcCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir, Policy: Policy{Retention: testRetention}})
	require.NoError(t, err)

	defer func() {
		require.NoError(t, handler.Close())
	}()

	now := time.Now()

	cases := []struct {
		Cache *Cache
		Kept  bool
	}{
		{
			// should be kept, since it's used recently and not too old.
			Cache: &Cache{
				Key:       "test_key_1",
				Version:   "test_version",
				Complete:  true,
				UsedAt:    now.Unix(),
				CreatedAt: now.Add(-time.Hour).Unix(),
			},
			Kept: true,
		},
		{
			// should be removed, since it's not complete and not used for a while.
			Cache: &Cache{
				Key:       "test_key_2",
				Version:   "test_version",
				Complete:  false,
				UsedAt:    now.Add(-(inUseGrace + time.Second)).Unix(),
				CreatedAt: now.Add(-(inUseGrace + time.Hour)).Unix(),
			},
			Kept: false,
		},
		{
			// should be removed, since it's not used for a while.
			Cache: &Cache{
				Key:       "test_key_3",
				Version:   "test_version",
				Complete:  true,
				UsedAt:    now.Add(-(testRetention + touchStale + time.Second)).Unix(),
				CreatedAt: now.Add(-(testRetention + time.Hour)).Unix(),
			},
			Kept: false,
		},
		{
			// should be kept, since age alone does not retire an entry that is still used.
			Cache: &Cache{
				Key:       "test_key_3",
				Version:   "test_version",
				Complete:  true,
				UsedAt:    now.Unix(),
				CreatedAt: now.Add(-365 * 24 * time.Hour).Unix(),
			},
			Kept: true,
		},
		{
			// should be kept, since it has a newer edition but be used recently.
			Cache: &Cache{
				Key:       "test_key_1",
				Version:   "test_version",
				Complete:  true,
				UsedAt:    now.Add(-(inUseGrace - time.Minute)).Unix(),
				CreatedAt: now.Add(-(time.Hour + time.Second)).Unix(),
			},
			Kept: true,
		},
		{
			// should be removed, since it has a newer edition and not be used recently.
			Cache: &Cache{
				Key:       "test_key_1",
				Version:   "test_version",
				Complete:  true,
				UsedAt:    now.Add(-(inUseGrace + time.Second)).Unix(),
				CreatedAt: now.Add(-(time.Hour + time.Second)).Unix(),
			},
			Kept: false,
		},
	}

	db, err := handler.openDB()
	require.NoError(t, err)
	for _, c := range cases {
		require.NoError(t, insertCache(db, c.Cache))
	}
	require.NoError(t, db.Close())

	handler.gcAt = time.Time{} // ensure gcCache will not skip
	handler.gcCache()

	db, err = handler.openDB()
	require.NoError(t, err)
	for i, v := range cases {
		t.Run(fmt.Sprintf("%d_%s", i, v.Cache.Key), func(t *testing.T) {
			cache := &Cache{}
			err = db.Get(v.Cache.ID, cache)
			if v.Kept {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, bolthold.ErrNotFound)
			}
		})
	}
	require.NoError(t, db.Close())
}

// TestHandler_evictPolicy covers the non-default policies; TestHandler_gcCache covers the
// defaults across every pass.
func TestHandler_evictPolicy(t *testing.T) {
	now := time.Now()
	stale := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	mib := func(n int64) int64 { return n * miB }

	for _, tc := range []struct {
		name    string
		policy  Policy
		entries []*Cache
		kept    []string
	}{
		{
			name:   "a zero retention keeps an entry nothing has touched",
			policy: Policy{Retention: 0},
			entries: []*Cache{
				{Key: "idle", UsedAt: stale(testRetention + time.Hour)},
			},
			kept: []string{"idle"},
		},
		{
			name:   "evicts least recently accessed until the repository fits",
			policy: Policy{RepoSizeLimit: mib(10)},
			entries: []*Cache{
				{Repo: "o/a", Key: "oldest", Size: mib(4), UsedAt: stale(3 * time.Hour)},
				{Repo: "o/a", Key: "middle", Size: mib(4), UsedAt: stale(2 * time.Hour)},
				{Repo: "o/a", Key: "newest", Size: mib(4), UsedAt: stale(time.Hour)},
			},
			kept: []string{"middle", "newest"},
		},
		{
			name:   "spares entries that may still be downloading",
			policy: Policy{RepoSizeLimit: mib(10)},
			entries: []*Cache{
				{Repo: "o/a", Key: "fresh_1", Size: mib(6), UsedAt: stale(time.Minute)},
				{Repo: "o/a", Key: "fresh_2", Size: mib(6), UsedAt: stale(time.Minute)},
			},
			kept: []string{"fresh_1", "fresh_2"},
		},
		{
			name:   "one repository over its limit leaves another alone",
			policy: Policy{RepoSizeLimit: mib(10)},
			entries: []*Cache{
				{Repo: "o/a", Key: "a_old", Size: mib(6), UsedAt: stale(3 * time.Hour)},
				{Repo: "o/a", Key: "a_new", Size: mib(6), UsedAt: stale(time.Hour)},
				{Repo: "o/b", Key: "b_old", Size: mib(6), UsedAt: stale(4 * time.Hour)},
			},
			kept: []string{"a_new", "b_old"},
		},
		{
			name:   "the total limit evicts across repositories once each fits its own",
			policy: Policy{RepoSizeLimit: mib(10), SizeLimit: mib(12)},
			entries: []*Cache{
				{Repo: "o/a", Key: "a_old", Size: mib(8), UsedAt: stale(3 * time.Hour)},
				{Repo: "o/b", Key: "b_new", Size: mib(8), UsedAt: stale(time.Hour)},
			},
			kept: []string{"b_new"},
		},
		{
			// Retention below inUseGrace would otherwise drop an entry whose signed URL a job
			// is still holding.
			name:   "a retention shorter than the grace still spares a just-served entry",
			policy: Policy{Retention: time.Minute},
			entries: []*Cache{
				{Key: "just_served", UsedAt: stale(2 * time.Minute)},
				{Key: "idle", UsedAt: stale(time.Hour)},
			},
			kept: []string{"just_served"},
		},
		{
			name:   "an entry over the limit goes without emptying the repository",
			policy: Policy{RepoSizeLimit: mib(10)},
			entries: []*Cache{
				{Repo: "o/a", Key: "keeps", Size: mib(4), UsedAt: stale(3 * time.Hour)},
				{Repo: "o/a", Key: "huge", Size: mib(20), UsedAt: stale(2 * time.Hour)},
			},
			kept: []string{"keeps"},
		},
		{
			name:   "a zero limit keeps everything",
			policy: Policy{RepoSizeLimit: 0},
			entries: []*Cache{
				{Repo: "o/a", Key: "huge_1", Size: mib(100), UsedAt: stale(3 * time.Hour)},
				{Repo: "o/a", Key: "huge_2", Size: mib(100), UsedAt: stale(2 * time.Hour)},
			},
			kept: []string{"huge_1", "huge_2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, e := range tc.entries {
				e.Complete = true // only completed entries carry a measured size, so only they count
			}
			handler := newTestHandler(t, tc.policy, tc.entries...)
			handler.gcAt = time.Time{} // ensure gcCache will not skip
			handler.gcCache()
			assert.ElementsMatch(t, tc.kept, keptKeys(t, handler, tc.entries))
		})
	}
}

// TestHandler_evictForFreeSpace proves the volume backstop sheds only what it must, and only
// when the disk is actually short.
func TestHandler_evictForFreeSpace(t *testing.T) {
	free := func(n int64) func(string) (uint64, error) {
		return func(string) (uint64, error) { return uint64(n), nil }
	}

	for _, tc := range []struct {
		name     string
		freeDisk func(string) (uint64, error)
		kept     []string
	}{
		{"ample free space evicts nothing", free(defaultMinFreeDisk), []string{"oldest", "middle", "newest"}},
		{"a small shortfall sheds one entry", free(defaultMinFreeDisk - 4*miB), []string{"middle", "newest"}},
		{"a shortfall the cache cannot cover sheds all of it", free(0), nil},
		{
			"an unreadable volume is treated as unavailable, not as full",
			func(string) (uint64, error) { return 0, errors.New("unsupported") },
			[]string{"oldest", "middle", "newest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			entries := []*Cache{
				{Key: "oldest", Complete: true, Size: 4 * miB, UsedAt: now.Add(-3 * time.Hour).Unix()},
				{Key: "middle", Complete: true, Size: 4 * miB, UsedAt: now.Add(-2 * time.Hour).Unix()},
				{Key: "newest", Complete: true, Size: 4 * miB, UsedAt: now.Add(-time.Hour).Unix()},
			}
			handler := newTestHandler(t, Policy{}, entries...)
			handler.freeDisk = tc.freeDisk

			db, err := handler.openDB()
			require.NoError(t, err)
			handler.evictForFreeSpace(db)
			require.NoError(t, db.Close())

			assert.ElementsMatch(t, tc.kept, keptKeys(t, handler, entries))
		})
	}
}

// TestHandler_SweepKeepsEntryWhenBlobSurvives proves a failed unlink leaves the row in place,
// so the next sweep retries rather than orphaning bytes no row points at and no limit counts.
func TestHandler_SweepKeepsEntryWhenBlobSurvives(t *testing.T) {
	cache := &Cache{Key: "stuck", Complete: true, UsedAt: time.Now().Add(-(testRetention + time.Hour)).Unix()}
	handler := newTestHandler(t, Policy{Retention: testRetention}, cache)

	// A non-empty directory where the blob belongs makes os.Remove fail on every platform.
	blob := handler.storage.filename(cache.ID)
	require.NoError(t, os.MkdirAll(blob, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blob, "held"), []byte("x"), 0o600))

	handler.gcAt = time.Time{}
	handler.gcCache()

	assert.Equal(t, []string{"stuck"}, keptKeys(t, handler, []*Cache{cache}), "the entry must outlive a blob that could not be removed")
}

// TestHandler_FindProtectsFromEviction covers the window between a find handing out a signed
// download URL and the GET that redeems it: the entry promised to a job must not be the next
// eviction victim just because its last access predates the find.
func TestHandler_FindProtectsFromEviction(t *testing.T) {
	// 12 MiB against a 10 MiB limit, so exactly one entry has to go.
	wanted := &Cache{Repo: testRepo, Key: "wanted", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-3 * time.Hour).Unix()}
	other := &Cache{Repo: testRepo, Key: "other", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-2 * time.Hour).Unix()}
	newest := &Cache{Repo: testRepo, Key: "newest", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-time.Hour).Unix()}
	handler := newTestHandler(t, Policy{RepoSizeLimit: 10 * miB}, wanted, other, newest)
	writeBlob(t, handler, wanted.ID) // find only reports a hit when the blob is on disk

	resp, err := testClient.Get(fmt.Sprintf("%s%s/cache?keys=wanted&version=v", handler.ExternalURL(), apiPath))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	// Evict directly: the request above kicked off an async gcCache, and writing gcAt here
	// to drive gcCache would race its read.
	db, err := handler.openDB()
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	handler.evictOversized(db)

	require.NoError(t, db.Get(wanted.ID, &Cache{}), "the entry just promised to a job must survive")
	assert.ErrorIs(t, db.Get(other.ID, &Cache{}), bolthold.ErrNotFound, "the next least recently used goes instead")
}

// TestHandler_evictOnCommit proves a repository that goes over its limit gets space back at
// once, rather than waiting out the collection interval.
func TestHandler_evictOnCommit(t *testing.T) {
	full := &Cache{Repo: testRepo, Key: "full", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-time.Hour).Unix()}
	handler := newTestHandler(t, Policy{RepoSizeLimit: 4 * miB}, full)

	// StartHandler already stamped gcAt, so the periodic sweep stays rate-limited out and
	// only the commit path can evict.
	uploadCacheNormally(t, handler.ExternalURL()+apiPath, "new", "v", []byte("some content"))

	assert.Empty(t, keptKeys(t, handler, []*Cache{full}))
}

func TestHandler_gcCacheInterval(t *testing.T) {
	cache := &Cache{Key: "temp", UsedAt: time.Now().Add(-time.Hour).Unix()}
	// Half the default, so a sweep 45m ago is still inside the default but past this one.
	handler := newTestHandler(t, Policy{SweepInterval: 30 * time.Minute}, cache)

	handler.gcAt = time.Now().Add(-45 * time.Minute) // past the configured interval, still inside the default
	handler.gcCache()
	assert.Empty(t, keptKeys(t, handler, []*Cache{cache}))
}

func TestHandler_touchCacheSkipsFreshCompletedEntries(t *testing.T) {
	fresh := &Cache{Repo: testRepo, Key: "fresh", Version: "v", Complete: true, UsedAt: time.Now().Unix()}
	pending := &Cache{Repo: testRepo, Key: "pending", Version: "v", UsedAt: time.Now().Add(-time.Second).Unix()}
	handler := newTestHandler(t, Policy{}, fresh, pending)
	txID := func() int {
		db, err := handler.openDB()
		require.NoError(t, err)
		defer db.Close()
		var id int
		require.NoError(t, db.Bolt().View(func(tx *bbolt.Tx) error { id = tx.ID(); return nil }))
		return id
	}

	before := txID()
	require.NoError(t, handler.touchCache(fresh.ID, false))
	assert.Equal(t, before, txID())
	require.NoError(t, handler.touchCache(pending.ID, false))
	assert.Greater(t, txID(), before)
}

// newTestHandler starts a handler with testToken registered, seeded with entries.
func newTestHandler(t *testing.T, policy Policy, entries ...*Cache) *Handler {
	t.Helper()
	handler, err := StartHandler(Options{
		Dir:        filepath.Join(t.TempDir(), "artifactcache"),
		OutboundIP: "127.0.0.1",
		Policy:     policy,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handler.Close()) })
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})

	db, err := handler.openDB()
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, insertCache(db, e))
	}
	require.NoError(t, db.Close())
	return handler
}

// keptKeys reports which of entries are still in the store.
func keptKeys(t *testing.T, handler *Handler, entries []*Cache) []string {
	t.Helper()
	db, err := handler.openDB()
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	var kept []string
	for _, e := range entries {
		if err := db.Get(e.ID, &Cache{}); err == nil {
			kept = append(kept, e.Key)
		}
	}
	return kept
}

// writeBlob gives an entry the on-disk bytes that find and get require.
func writeBlob(t *testing.T, handler *Handler, id uint64) {
	t.Helper()
	require.NoError(t, handler.storage.Write(id, 0, strings.NewReader("a")))
	_, err := handler.storage.Commit(id, 1)
	require.NoError(t, err)
}

// TestHandler_RejectsMissingBearer covers the advisory's root cause:
// unauthenticated access to management endpoints is now refused with 401.
func TestHandler_RejectsMissingBearer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()

	base := handler.ExternalURL() + apiPath

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"find", http.MethodGet, "/cache?keys=x&version=y", ""},
		{"reserve", http.MethodPost, "/caches", "{}"},
		{"upload", http.MethodPatch, "/caches/1", ""},
		{"commit", http.MethodPost, "/caches/1", ""},
		{"clean", http.MethodPost, "/clean", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
}

// TestHandler_RejectsUnknownBearer verifies that a bearer token is only
// accepted after RegisterJob; stale/forged tokens cannot be replayed.
func TestHandler_RejectsUnknownBearer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()

	base := handler.ExternalURL() + apiPath

	req, err := http.NewRequest(http.MethodGet, base+"/cache?keys=x&version=y", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer not-a-registered-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestHandler_UnregisterRevokes ensures that the function returned by
// RegisterJob invalidates the credential, so a token leaked at job time stops
// working the moment the job ends instead of living for the runner's lifetime.
func TestHandler_UnregisterRevokes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()

	unregister := handler.RegisterJob("tmp-token", JobCredential{Repo: testRepo})

	base := handler.ExternalURL() + apiPath
	req, err := http.NewRequest(http.MethodGet, base+"/cache?keys=x&version=y", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tmp-token")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)

	unregister()

	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// TestHandler_CrossRepoIsolation addresses the intra-runner poisoning vector
// raised in GHSA-82g9-637c-2fx2: job containers can reach the cache server
// over the docker bridge, so IP allowlisting alone does not stop a malicious
// PR run from another repo. A cache entry created under repoA must be
// invisible to queries scoped to repoB.
func TestHandler_CrossRepoIsolation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()
	handler.RegisterJob("token-a", JobCredential{Repo: "owner/repoA"})
	handler.RegisterJob("token-b", JobCredential{Repo: "owner/repoB"})

	base := handler.ExternalURL() + apiPath
	key := "shared-key"
	version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
	content := []byte("repoA-payload")

	clientA := &http.Client{Transport: &bearerTransport{token: "token-a"}}
	clientB := &http.Client{Transport: &bearerTransport{token: "token-b"}}

	// repoA reserves + uploads + commits.
	reserveBody, err := json.Marshal(&Request{Key: key, Version: version, Size: int64(len(content))})
	require.NoError(t, err)
	resp, err := clientA.Post(base+"/caches", "application/json", bytes.NewReader(reserveBody))
	require.NoError(t, err)
	var reserved struct {
		CacheID uint64 `json:"cacheId"`
	}
	require.NoError(t, json.UnmarshalRead(resp.Body, &reserved))
	resp.Body.Close()
	require.NotZero(t, reserved.CacheID)

	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, reserved.CacheID), bytes.NewReader(content))
	require.NoError(t, err)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(content)-1))
	resp, err = clientA.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, err = clientA.Post(fmt.Sprintf("%s/caches/%d", base, reserved.CacheID), "", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// repoB with a matching key and version must NOT see repoA's cache.
	resp, err = clientB.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// repoA still sees its own cache.
	resp, err = clientA.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// repoB cannot upload to repoA's reserved id either (forbidden, not 401).
	req, err = http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, reserved.CacheID), bytes.NewReader([]byte("poison")))
	require.NoError(t, err)
	req.Header.Set("Content-Range", "bytes 0-5/*")
	resp, err = clientB.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// TestHandler_ArtifactSignature verifies that archive downloads reject
// missing / tampered / expired signatures, so a leaked archiveLocation stops
// working after artifactURLTTL even if the bearer token is still registered.
func TestHandler_ArtifactSignature(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})

	base := handler.ExternalURL() + apiPath

	t.Run("missing signature", func(t *testing.T) {
		resp, err := testClient.Get(fmt.Sprintf("%s/artifacts/%d", base, 1))
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("tampered signature", func(t *testing.T) {
		good := signArtifactURL(handler, 1)
		bad := good[:len(good)-4] + "dead"
		resp, err := testClient.Get(bad)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("expired signature", func(t *testing.T) {
		expired := handler.signedArtifactURL(JobCredential{}, 1, time.Now().Add(-time.Second))
		resp, err := testClient.Get(expired)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("signature from a different server", func(t *testing.T) {
		dir2 := filepath.Join(t.TempDir(), "artifactcache2")
		other, err := StartHandler(Options{Dir: dir2})
		require.NoError(t, err)
		defer other.Close()
		otherURL := signArtifactURL(other, 1)
		// Rewrite the host so the request still lands on our handler, but
		// the signature was computed with a different secret.
		parts := strings.SplitN(otherURL, apiPath, 2)
		forged := base + parts[1]
		resp, err := testClient.Get(forged)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

// TestHandler_SecretPersistsAcrossRestarts is the property that lets
// gitea-runner cache-server be pointed at via cfg.Cache.ExternalServer: a
// restart must not invalidate signed URLs the handler has already issued
// (within their expiry window).
func TestHandler_SecretPersistsAcrossRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")

	first, err := StartHandler(Options{Dir: dir, OutboundIP: "127.0.0.1"})
	require.NoError(t, err)
	exp := time.Now().Add(artifactURLTTL).Unix()
	sig := first.computeSignature("", 42, exp)
	require.NoError(t, first.Close())

	second, err := StartHandler(Options{Dir: dir, OutboundIP: "127.0.0.1"})
	require.NoError(t, err)
	defer second.Close()

	assert.Equal(t, sig, second.computeSignature("", 42, exp))
}

// TestHandler_ArtifactSignatureDownload is a happy-path round trip that
// ensures a real reserve/upload/commit/find/download flow still works after
// the auth refactor.
func TestHandler_ArtifactSignatureDownload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})

	base := handler.ExternalURL() + apiPath
	key := "download-key"
	version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
	content := []byte("hello")
	uploadCacheNormally(t, base, key, version, content)

	resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var hit struct {
		ArchiveLocation string `json:"archiveLocation"`
	}
	require.NoError(t, json.UnmarshalRead(resp.Body, &hit))
	resp.Body.Close()

	require.Contains(t, hit.ArchiveLocation, "sig=")
	require.Contains(t, hit.ArchiveLocation, "exp=")

	// Download without any Authorization header — the signature alone must
	// be enough, because @actions/cache downloads archiveLocation unauth'd.
	dl, err := http.Get(hit.ArchiveLocation)
	require.NoError(t, err)
	body, err := io.ReadAll(dl.Body)
	dl.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, dl.StatusCode)
	assert.Equal(t, content, body)
}

// TestHandler_RegisterJob_RefCounted verifies that a duplicate RegisterJob
// for the same token does not silently revoke the first registration on the
// first revoker call. This matters if a runner ever re-registers a token
// (restart mid-task, retry), which must not kill the live job's auth.
func TestHandler_RegisterJob_RefCounted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()

	first := handler.RegisterJob("shared", JobCredential{Repo: testRepo})
	second := handler.RegisterJob("shared", JobCredential{Repo: testRepo})

	base := handler.ExternalURL() + apiPath
	probe := func() int {
		req, err := http.NewRequest(http.MethodGet, base+"/cache?keys=x&version=v", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer shared")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	require.NotEqual(t, http.StatusUnauthorized, probe())
	first()
	assert.NotEqual(t, http.StatusUnauthorized, probe(),
		"token must stay valid while another registration holds the refcount")
	second()
	assert.Equal(t, http.StatusUnauthorized, probe(),
		"token is revoked only after every revoker has run")
}

// TestHandler_GC_PerRepoDedup ensures duplicate-pruning does not evict
// another repo's entry. Two repos reserve the same (key, version); after the
// inUseGrace window, GC must keep the one from each repo.
func TestHandler_GC_PerRepoDedup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()
	handler.RegisterJob("tok-a", JobCredential{Repo: "owner/repoA"})
	handler.RegisterJob("tok-b", JobCredential{Repo: "owner/repoB"})

	key := "shared-dedup-key"
	version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"

	// Seed one completed cache per repo directly via the DB, bypassing the
	// HTTP round trip so we can precisely control UsedAt.
	db, err := handler.openDB()
	require.NoError(t, err)
	now := time.Now().Unix()
	stale := time.Now().Add(-inUseGrace - time.Minute).Unix()
	a := &Cache{Repo: "owner/repoA", Key: key, Version: version, Complete: true, CreatedAt: stale, UsedAt: stale, Size: 1}
	b := &Cache{Repo: "owner/repoB", Key: key, Version: version, Complete: true, CreatedAt: now, UsedAt: now, Size: 1}
	require.NoError(t, insertCache(db, a))
	require.NoError(t, insertCache(db, b))
	// Write the backing blobs so the dedup deletion has something to remove.
	require.NoError(t, handler.storage.Write(a.ID, 0, strings.NewReader("a")))
	_, err = handler.storage.Commit(a.ID, 1)
	require.NoError(t, err)
	require.NoError(t, handler.storage.Write(b.ID, 0, strings.NewReader("b")))
	_, err = handler.storage.Commit(b.ID, 1)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Force GC to run regardless of the cooldown.
	handler.gcAt = time.Time{}
	handler.gcCache()

	db, err = handler.openDB()
	require.NoError(t, err)
	defer db.Close()
	var after []Cache
	require.NoError(t, db.Find(&after, bolthold.Where("Key").Eq(key).And("Version").Eq(version)))

	repos := make(map[string]bool)
	for _, c := range after {
		repos[c.Repo] = true
	}
	assert.True(t, repos["owner/repoA"], "repoA's cache must survive dedup against repoB")
	assert.True(t, repos["owner/repoB"], "repoB's cache must survive dedup against repoA")
}

// TestHandler_InternalAPI_Disabled verifies that without an internalSecret
// the control-plane routes are 404 — operators can't accidentally hit
// register/revoke when the feature is off.
func TestHandler_InternalAPI_Disabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()

	for _, ep := range []string{"/_internal/register", "/_internal/revoke"} {
		resp, err := http.Post(handler.ExternalURL()+ep, "application/json", strings.NewReader(`{}`))
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, ep)
	}
}

// TestHandler_InternalAPI_AuthAndUsage covers the control-plane: bad/missing
// secret → 401, malformed body → 400, happy path round-trips a token through
// register → cache-API accepts it → revoke → cache-API rejects it.
func TestHandler_InternalAPI_AuthAndUsage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifactcache")
	const secret = "internal-secret"
	handler, err := StartHandler(Options{Dir: dir, InternalSecret: secret})
	require.NoError(t, err)
	defer handler.Close()

	base := handler.ExternalURL()

	post := func(path, bearer, body string) int {
		req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
		require.NoError(t, err)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("missing secret 401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, post("/_internal/register", "", `{"token":"x","repo":"r"}`))
	})
	t.Run("wrong secret 401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, post("/_internal/register", "wrong", `{"token":"x","repo":"r"}`))
	})
	t.Run("malformed body 400", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, post("/_internal/register", secret, `not json`))
	})
	t.Run("missing token 400", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, post("/_internal/register", secret, `{"repo":"r"}`))
	})

	t.Run("register then revoke round-trip", func(t *testing.T) {
		probe := func(token string) int {
			req, _ := http.NewRequest(http.MethodGet, base+apiPath+"/cache?keys=k&version=v", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			return resp.StatusCode
		}

		assert.Equal(t, http.StatusUnauthorized, probe("via-internal-api"))
		assert.Equal(t, http.StatusOK, post("/_internal/register", secret, `{"token":"via-internal-api","repo":"owner/repo"}`))
		assert.NotEqual(t, http.StatusUnauthorized, probe("via-internal-api"))
		assert.Equal(t, http.StatusOK, post("/_internal/revoke", secret, `{"token":"via-internal-api"}`))
		assert.Equal(t, http.StatusUnauthorized, probe("via-internal-api"))
	})
}
