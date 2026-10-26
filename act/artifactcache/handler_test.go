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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"
)

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

const testRetention = 7 * 24 * time.Hour

func signArtifactURL(h *Handler, id int64) string {
	return h.signedArtifactURL(JobCredential{}, uint64(id), time.Now().Add(artifactURLTTL))
}

func TestHandler_ExternalURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "http://[2001:db8::1]:8080", (&Handler{outboundIP: "2001:db8::1", port: 8080}).ExternalURL())
}

func TestHandler_Upstream(t *testing.T) {
	t.Parallel()
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/cache%20server"+blobPath+"/%34%32", r.URL.EscapedPath())
		assert.Equal(t, "sig=opaque%2Fvalue&raw=a;b", r.URL.RawQuery)
		assert.Equal(t, strings.TrimPrefix(upstream.URL, "http://"), r.Host)
		assert.Equal(t, "Bearer unregistered", r.Header.Get("Authorization"))
		_, err := io.Copy(w, r.Body)
		assert.NoError(t, err)
	}))
	t.Cleanup(upstream.Close)
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{
		Dir:        dir,
		OutboundIP: "127.0.0.1",
		Upstream:   upstream.URL + "/cache%20server//",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handler.Close()) })

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut,
		handler.ExternalURL()+blobPath+"/%34%32?sig=opaque%2Fvalue&raw=a;b", strings.NewReader("cache content"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer unregistered")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	forwarded, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, "cache content", string(forwarded))

	resp, err = http.Post(handler.ExternalURL()+internalPath+"/register", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	upstream.Close()
	resp, err = http.Post(handler.ExternalURL()+cacheServiceV2Path+"/CreateCacheEntry", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	var failure map[string]string
	err = json.UnmarshalRead(resp.Body, &failure)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, map[string]string{"code": "unavailable", "msg": "cache upstream is unavailable"}, failure)
	assert.NoDirExists(t, dir)
}

func TestHandler(t *testing.T) {
	t.Parallel()
	const version = "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})

	base := fmt.Sprintf("%s%s", handler.ExternalURL(), apiPath)

	t.Run("database open recovers from failure, times out while held and close is idempotent", func(t *testing.T) {
		dir := t.TempDir()
		synctest.Test(t, func(t *testing.T) {
			handler := &Handler{dir: dir, dbLock: make(chan struct{}, 1)}
			require.NoError(t, os.Mkdir(filepath.Join(handler.dir, "bolt.db"), 0o755))
			_, err := handler.openDB()
			require.Error(t, err)
			require.NoError(t, os.Remove(filepath.Join(handler.dir, "bolt.db")))
			first, err := handler.openDB()
			require.NoError(t, err)
			_, err = handler.openDB()
			require.ErrorIs(t, err, berrors.ErrTimeout)
			require.NoError(t, first.Close())
			require.NoError(t, first.Close())
			second, err := handler.openDB()
			require.NoError(t, err)
			require.NoError(t, second.Close())
		})
	})

	defer func() {
		t.Run("inpect db", func(t *testing.T) {
			db, err := handler.openDB()
			require.NoError(t, err)
			defer db.Close()
			require.NoError(t, db.bolt.View(func(tx *bbolt.Tx) error {
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
		resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 204, resp.StatusCode)
	})

	t.Run("reserve and upload", func(t *testing.T) {
		key := strings.ToLower(t.Name())
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
		resp, err := testClient.Post(base+"/caches", "application/json", strings.NewReader(`invalid json`))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, 400, resp.StatusCode)
	})

	t.Run("duplicate reserve", func(t *testing.T) {
		var reserved [2]uint64
		for i := range reserved {
			reserved[i] = reserveCache(t, testClient, base, strings.ToLower(t.Name()), version, 100)
		}
		assert.NotEqual(t, reserved[0], reserved[1])
	})

	for _, tc := range []struct {
		name   string
		method string
		id     string
	}{
		{"upload with bad id", http.MethodPatch, "invalid_id"},
		{"upload without reserve", http.MethodPatch, "1000"},
		{"commit with bad id", http.MethodPost, "invalid_id"},
		{"commit with not exist id", http.MethodPost, "100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+"/caches/"+tc.id, nil)
			require.NoError(t, err)
			if tc.method == http.MethodPatch {
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("Content-Range", "bytes 0-99/*")
			}
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		})
	}

	for _, tc := range []struct {
		name   string
		method string
	}{
		{"upload with complete", http.MethodPatch},
		{"duplicate commit", http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []byte("cache content")
			id := uploadCacheNormally(t, base, strings.ToLower(t.Name()), version, content)
			req, err := http.NewRequest(tc.method, fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", "bytes 0-12/*")
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		})
	}

	t.Run("upload with invalid range", func(t *testing.T) {
		id := reserveCache(t, testClient, base, strings.ToLower(t.Name()), version, 100)
		for _, contentRange := range []string{"bytes xx-99/*", "bytes 0-xx/*"} {
			req, err := http.NewRequest(http.MethodPatch,
				fmt.Sprintf("%s/caches/%d", base, id), nil)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Range", contentRange)
			resp, err := testClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, 400, resp.StatusCode)
		}
	})

	t.Run("upload write failure returns only error", func(t *testing.T) {
		id := reserveCache(t, testClient, base, strings.ToLower(t.Name()), version, 100)

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
		content := make([]byte, 100)
		_, err := rand.Read(content)
		require.NoError(t, err)
		id := reserveCache(t, testClient, base, strings.ToLower(t.Name()), version, 100)
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

	t.Run("chunked commit validates size omitted at reservation", func(t *testing.T) {
		for _, size := range []int64{5, 10} {
			t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
				id := reserveCache(t, testClient, base, strings.ToLower(t.Name()), version, 0)
				req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, id), strings.NewReader("cache"))
				require.NoError(t, err)
				req.Header.Set("Content-Range", "bytes 0-4/*")
				resp, err := testClient.Do(req)
				require.NoError(t, err)
				resp.Body.Close()
				require.Equal(t, http.StatusOK, resp.StatusCode)

				resp, err = testClient.Post(fmt.Sprintf("%s/caches/%d", base, id), "application/json", io.MultiReader(strings.NewReader(fmt.Sprintf(`{"size":%d}`, size))))
				require.NoError(t, err)
				resp.Body.Close()
				if size == 5 {
					require.Equal(t, http.StatusOK, resp.StatusCode)
				} else {
					require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
				}

				db, err := handler.openDB()
				require.NoError(t, err)
				defer db.Close()
				cache, err := db.get(id)
				require.NoError(t, err)
				require.Equal(t, size == 5, cache.Complete)
			})
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

	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{"get with multiple keys", []string{"_a_b_x", "_a_b", "_a"}, "_a_b"},
		{"exact keys are preferred (key 0)", []string{"_a", "_a_b"}, "_a"},
		{"exact keys are preferred (key 1)", []string{"_missing", "_a", "_a_b"}, "_a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := strings.ToLower(t.Name())
			for i, suffix := range []string{"_a", "_a_b_c", "_a_b"} {
				uploadCacheNormally(t, base, key+suffix, version, []byte(suffix))
				backdateCache(t, handler, key+suffix, time.Duration(3-i)*time.Second)
			}
			keys := make([]string, len(tc.keys))
			for i, suffix := range tc.keys {
				keys[i] = key + suffix
			}
			resp, err := testClient.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, strings.Join(keys, ","), version))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			var got struct {
				Result          string `json:"result"`
				ArchiveLocation string `json:"archiveLocation"`
				CacheKey        string `json:"cacheKey"`
			}
			require.NoError(t, json.UnmarshalRead(resp.Body, &got))
			assert.Equal(t, "hit", got.Result)
			assert.Equal(t, key+tc.want, got.CacheKey)
			contentResp, err := testClient.Get(got.ArchiveLocation)
			require.NoError(t, err)
			defer contentResp.Body.Close()
			require.Equal(t, 200, contentResp.StatusCode)
			content, err := io.ReadAll(contentResp.Body)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.want), content)
		})
	}

	t.Run("case preserved", func(t *testing.T) {
		uploadCacheNormally(t, base, strings.ToLower(t.Name())+"_ABC", version, []byte("cache content"))
	})
}

func backdateCache(t *testing.T, handler *Handler, key string, age time.Duration) {
	db, err := handler.openDB()
	require.NoError(t, err)
	defer db.Close()

	caches, err := db.find(func(cache *Cache) bool { return cache.Key == key })
	require.NoError(t, err)
	require.Len(t, caches, 1)
	caches[0].CreatedAt = time.Now().Add(-age).Unix()
	require.NoError(t, db.save(caches[0]))
}

func reserveCache(t *testing.T, client *http.Client, base, key, version string, size int64) uint64 {
	t.Helper()
	body, err := json.Marshal(&Request{Key: key, Version: version, Size: size})
	require.NoError(t, err)
	resp, err := client.Post(base+"/caches", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var reserved struct {
		CacheID uint64 `json:"cacheId"`
	}
	require.NoError(t, json.UnmarshalRead(resp.Body, &reserved))
	require.NotZero(t, reserved.CacheID)
	return reserved.CacheID
}

func uploadCacheNormally(t *testing.T, base, key, version string, content []byte) uint64 {
	id := reserveCache(t, testClient, base, key, version, int64(len(content)))
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
	return id
}

func TestHandler_gcCache(t *testing.T) {
	t.Parallel()
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
		require.NoError(t, handler.insertCache(db, c.Cache))
	}
	require.NoError(t, db.Close())

	handler.gcAt = time.Time{}
	handler.gcCache()

	db, err = handler.openDB()
	require.NoError(t, err)
	for i, v := range cases {
		t.Run(fmt.Sprintf("%d_%s", i, v.Cache.Key), func(t *testing.T) {
			_, err = db.get(v.Cache.ID)
			if v.Kept {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, errCacheNotFound)
			}
		})
	}
	require.NoError(t, db.Close())
}

func TestHandler_evictPolicy(t *testing.T) {
	t.Parallel()
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
				e.Complete = true
			}
			handler := newTestHandler(t, tc.policy, tc.entries...)
			handler.gcAt = time.Time{}
			handler.gcCache()
			assert.ElementsMatch(t, tc.kept, keptKeys(t, handler, tc.entries))
		})
	}
}

func TestHandler_evictForFreeSpace(t *testing.T) {
	t.Parallel()
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

func TestHandler_SweepKeepsEntryWhenBlobSurvives(t *testing.T) {
	t.Parallel()
	cache := &Cache{Key: "stuck", Complete: true, UsedAt: time.Now().Add(-(testRetention + time.Hour)).Unix()}
	handler := newTestHandler(t, Policy{Retention: testRetention}, cache)

	blob := handler.storage.(*Storage).filename(cache.ID)
	require.NoError(t, os.MkdirAll(blob, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blob, "held"), []byte("x"), 0o600))

	handler.gcAt = time.Time{}
	handler.gcCache()

	assert.Equal(t, []string{"stuck"}, keptKeys(t, handler, []*Cache{cache}), "the entry must outlive a blob that could not be removed")
}

func TestHandler_FindProtectsFromEviction(t *testing.T) {
	t.Parallel()
	wanted := &Cache{Repo: testRepo, Key: "wanted", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-3 * time.Hour).Unix()}
	other := &Cache{Repo: testRepo, Key: "other", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-2 * time.Hour).Unix()}
	newest := &Cache{Repo: testRepo, Key: "newest", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-time.Hour).Unix()}
	handler := newTestHandler(t, Policy{RepoSizeLimit: 10 * miB}, wanted, other, newest)
	writeBlob(t, handler, wanted.ID)

	resp, err := testClient.Get(fmt.Sprintf("%s%s/cache?keys=wanted&version=v", handler.ExternalURL(), apiPath))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	db, err := handler.openDB()
	require.NoError(t, err)
	handler.evictOversized(db)
	require.NoError(t, db.Close())

	assert.Equal(t, []string{wanted.Key}, keptKeys(t, handler, []*Cache{wanted, other}), "the entry just promised to a job survives, the next least recently used goes instead")
}

func TestHandler_PrefixUsesNewestReservationWithinSecond(t *testing.T) {
	t.Parallel()
	created := time.Now().Add(-time.Hour).Unix()
	older := &Cache{Repo: testRepo, Key: "deps-old", Version: "v", Complete: true, CreatedAt: created}
	newer := &Cache{Repo: testRepo, Key: "deps-new", Version: "v", Complete: true, CreatedAt: created}
	handler := newTestHandler(t, Policy{}, older, newer)

	db, err := handler.openDB()
	require.NoError(t, err)
	defer db.Close()
	cache, err := findCache(db, testRepo, []string{"deps-"}, "v")
	require.NoError(t, err)
	require.Equal(t, newer, cache)
}

func TestHandler_evictOnCommit(t *testing.T) {
	t.Parallel()
	full := &Cache{Repo: testRepo, Key: "full", Version: "v", Complete: true, Size: 4 * miB, UsedAt: time.Now().Add(-time.Hour).Unix()}
	handler := newTestHandler(t, Policy{RepoSizeLimit: 4 * miB}, full)

	uploadCacheNormally(t, handler.ExternalURL()+apiPath, "new", "v", []byte("some content"))

	assert.Empty(t, keptKeys(t, handler, []*Cache{full}))
}

func TestHandler_gcCacheInterval(t *testing.T) {
	t.Parallel()
	cache := &Cache{Key: "temp", UsedAt: time.Now().Add(-time.Hour).Unix()}
	handler := newTestHandler(t, Policy{SweepInterval: 30 * time.Minute}, cache)

	handler.gcAt = time.Now().Add(-45 * time.Minute)
	handler.gcCache()
	assert.Empty(t, keptKeys(t, handler, []*Cache{cache}))
}

func TestHandler_touchCacheSkipsFreshCompletedEntries(t *testing.T) {
	t.Parallel()
	fresh := &Cache{Repo: testRepo, Key: "fresh", Version: "v", Complete: true, UsedAt: time.Now().Unix()}
	pending := &Cache{Repo: testRepo, Key: "pending", Version: "v", UsedAt: time.Now().Add(-time.Second).Unix()}
	handler := newTestHandler(t, Policy{}, fresh, pending)
	txID := func() int {
		db, err := handler.openDB()
		require.NoError(t, err)
		defer db.Close()
		var id int
		require.NoError(t, db.bolt.View(func(tx *bbolt.Tx) error { id = tx.ID(); return nil }))
		return id
	}

	before := txID()
	require.NoError(t, handler.touchCache(fresh.ID, false))
	assert.Equal(t, before, txID())
	require.NoError(t, handler.touchCache(pending.ID, false))
	assert.Greater(t, txID(), before)
}

func newTestHandler(t *testing.T, policy Policy, entries ...*Cache) *Handler {
	t.Helper()
	handler := startTestHandler(t, Options{Policy: policy})

	db, err := handler.openDB()
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, handler.insertCache(db, e))
	}
	require.NoError(t, db.Close())
	return handler
}

func startTestHandler(t *testing.T, opts Options) *Handler {
	t.Helper()
	opts.Dir = filepath.Join(t.TempDir(), "artifactcache")
	opts.OutboundIP = "127.0.0.1"
	handler, err := StartHandler(opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handler.Close()) })
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})
	return handler
}

func keptKeys(t *testing.T, handler *Handler, entries []*Cache) []string {
	t.Helper()
	db, err := handler.openDB()
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	var kept []string
	for _, e := range entries {
		if _, err := db.get(e.ID); err == nil {
			kept = append(kept, e.Key)
		}
	}
	return kept
}

func writeBlob(t *testing.T, handler *Handler, id uint64) {
	t.Helper()
	require.NoError(t, handler.storage.Write(id, 0, strings.NewReader("a")))
	_, err := handler.storage.Commit(id, 1)
	require.NoError(t, err)
}

func TestHandler_RejectsBearer(t *testing.T) {
	t.Parallel()
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
		token  string
	}{
		{"find without bearer", http.MethodGet, "/cache?keys=x&version=y", "", ""},
		{"reserve without bearer", http.MethodPost, "/caches", "{}", ""},
		{"upload without bearer", http.MethodPatch, "/caches/1", "", ""},
		{"commit without bearer", http.MethodPost, "/caches/1", "", ""},
		{"clean without bearer", http.MethodPost, "/clean", "", ""},
		{"find with unknown bearer", http.MethodGet, "/cache?keys=x&version=y", "", "not-a-registered-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, base+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
}

func TestHandler_UnregisterRevokes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		token         string
		version       string
		registrations int
	}{
		{"single registration", "tmp-token", "y", 1},
		{"reference counted", "shared", "v", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := StartHandler(Options{Dir: filepath.Join(t.TempDir(), "artifactcache")})
			require.NoError(t, err)
			defer handler.Close()
			revokers := make([]func(), tc.registrations)
			for i := range revokers {
				revokers[i] = handler.RegisterJob(tc.token, JobCredential{Repo: testRepo})
			}
			probe := func() int {
				req, err := http.NewRequest(http.MethodGet, handler.ExternalURL()+apiPath+"/cache?keys=x&version="+tc.version, nil)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+tc.token)
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				resp.Body.Close()
				return resp.StatusCode
			}

			require.NotEqual(t, http.StatusUnauthorized, probe())
			for i, revoke := range revokers {
				revoke()
				if i == len(revokers)-1 {
					assert.Equal(t, http.StatusUnauthorized, probe(), "token is revoked only after every revoker has run")
				} else {
					assert.NotEqual(t, http.StatusUnauthorized, probe(), "token must stay valid while another registration holds the refcount")
				}
			}
		})
	}
}

func TestHandler_CrossRepoIsolation(t *testing.T) {
	t.Parallel()
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

	id := reserveCache(t, clientA, base, key, version, int64(len(content)))

	req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader(content))
	require.NoError(t, err)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(content)-1))
	resp, err := clientA.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, err = clientA.Post(fmt.Sprintf("%s/caches/%d", base, id), "", nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, err = clientB.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, err = clientA.Get(fmt.Sprintf("%s/cache?keys=%s&version=%s", base, key, version))
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	req, err = http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/caches/%d", base, id), bytes.NewReader([]byte("poison")))
	require.NoError(t, err)
	req.Header.Set("Content-Range", "bytes 0-5/*")
	resp, err = clientB.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestHandler_ArtifactSignature(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()
	handler.RegisterJob(testToken, JobCredential{Repo: testRepo})

	base := handler.ExternalURL() + apiPath
	good := signArtifactURL(handler, 1)
	other, err := StartHandler(Options{Dir: filepath.Join(t.TempDir(), "artifactcache2")})
	require.NoError(t, err)
	defer other.Close()

	for _, tc := range []struct {
		name string
		url  string
	}{
		{"missing signature", fmt.Sprintf("%s/artifacts/%d", base, 1)},
		{"tampered signature", good[:len(good)-4] + "dead"},
		{"expired signature", handler.signedArtifactURL(JobCredential{}, 1, time.Now().Add(-time.Second))},
		{"signature from a different server", base + strings.SplitN(signArtifactURL(other, 1), apiPath, 2)[1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := testClient.Get(tc.url)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
}

func TestHandler_SecretPersistsAcrossRestarts(t *testing.T) {
	t.Parallel()
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

func TestHandler_SeesWritesOfOtherProcesses(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, Policy{})
	insert := func(by *Handler, cache *Cache) {
		db, err := by.openDB()
		require.NoError(t, err)
		require.NoError(t, by.insertCache(db, cache))
		require.NoError(t, db.Close())
	}
	other := &Handler{dir: handler.dir, dbLock: make(chan struct{}, 1)}
	theirs, later, ours := &Cache{Key: "theirs"}, &Cache{Key: "later"}, &Cache{Key: "ours"}
	insert(other, theirs)
	assert.Equal(t, []string{"theirs"}, keptKeys(t, handler, []*Cache{theirs}))
	insert(other, later)
	insert(handler, ours)
	assert.Equal(t, []string{"theirs", "later", "ours"}, keptKeys(t, handler, []*Cache{theirs, later, ours}))
}

func TestHandler_ArtifactSignatureDownload(t *testing.T) {
	t.Parallel()
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

	dl, err := http.Get(hit.ArchiveLocation)
	require.NoError(t, err)
	body, err := io.ReadAll(dl.Body)
	dl.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, dl.StatusCode)
	assert.Equal(t, content, body)
}

func TestHandler_GC_PerRepoDedup(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "artifactcache")
	handler, err := StartHandler(Options{Dir: dir})
	require.NoError(t, err)
	defer handler.Close()
	handler.RegisterJob("tok-a", JobCredential{Repo: "owner/repoA"})
	handler.RegisterJob("tok-b", JobCredential{Repo: "owner/repoB"})

	key := "shared-dedup-key"
	version := "c19da02a2bd7e77277f1ac29ab45c09b7d46a4ee758284e26bb3045ad11d9d20"

	db, err := handler.openDB()
	require.NoError(t, err)
	now := time.Now().Unix()
	stale := time.Now().Add(-inUseGrace - time.Minute).Unix()
	a := &Cache{Repo: "owner/repoA", Key: key, Version: version, Complete: true, CreatedAt: stale, UsedAt: stale, Size: 1}
	b := &Cache{Repo: "owner/repoB", Key: key, Version: version, Complete: true, CreatedAt: now, UsedAt: now, Size: 1}
	require.NoError(t, handler.insertCache(db, a))
	require.NoError(t, handler.insertCache(db, b))
	require.NoError(t, handler.storage.Write(a.ID, 0, strings.NewReader("a")))
	_, err = handler.storage.Commit(a.ID, 1)
	require.NoError(t, err)
	require.NoError(t, handler.storage.Write(b.ID, 0, strings.NewReader("b")))
	_, err = handler.storage.Commit(b.ID, 1)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	handler.gcAt = time.Time{}
	handler.gcCache()

	db, err = handler.openDB()
	require.NoError(t, err)
	defer db.Close()
	after, err := db.find(func(cache *Cache) bool { return cache.Key == key && cache.Version == version })
	require.NoError(t, err)

	repos := make(map[string]bool)
	for _, c := range after {
		repos[c.Repo] = true
	}
	assert.True(t, repos["owner/repoA"], "repoA's cache must survive dedup against repoB")
	assert.True(t, repos["owner/repoB"], "repoB's cache must survive dedup against repoA")
}

func TestHandler_InternalAPI_Disabled(t *testing.T) {
	t.Parallel()
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

func TestHandler_InternalAPI_AuthAndUsage(t *testing.T) {
	t.Parallel()
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
		assert.Equal(t, http.StatusOK, post("/_internal/register", secret, `{"token":"via-internal-api","repo":"owner/repo","public_url":"http://runner.example","results":"https://gitea.example","insecure_tls":true}`))
		cred, ok := handler.lookupCredential("via-internal-api")
		require.True(t, ok)
		assert.Equal(t, JobCredential{Repo: testRepo, PublicURL: "http://runner.example"}, cred)
		assert.NotEqual(t, http.StatusUnauthorized, probe("via-internal-api"))
		assert.Equal(t, http.StatusOK, post("/_internal/revoke", secret, `{"token":"via-internal-api"}`))
		assert.Equal(t, http.StatusUnauthorized, probe("via-internal-api"))
	})
}
