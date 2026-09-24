// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_S3SharesEntriesAndDropsExpiredOnes(t *testing.T) {
	backend := s3mem.New()
	require.NoError(t, backend.CreateBucket("cache"))
	server := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(server.Close)
	opts := Options{S3: &S3Options{Endpoint: server.URL, Bucket: "cache", Prefix: "runners", AccessKey: "key", SecretKey: "secret"}}
	saver, restorer := startTestHandler(t, opts), startTestHandler(t, opts)
	entry := map[string]any{"key": "deps", "version": "v1"}

	finalized, _ := saveV2(t, saver, "deps", "v1", []byte("archive"))
	require.Equal(t, true, finalized["ok"])
	require.NoError(t, restorer.importMetadata(t.Context()))
	found := v2Call(t, restorer, testClient, "GetCacheEntryDownloadURL", entry)
	require.Equal(t, true, found["ok"])
	assert.Equal(t, []byte("archive"), getURL(t, found["signed_download_url"].(string)))

	require.NoError(t, backend.ForceDeleteBucket("cache"))
	require.NoError(t, backend.CreateBucket("cache"))
	require.NoError(t, restorer.importMetadata(t.Context()))
	assert.Equal(t, true, v2Call(t, restorer, testClient, "CreateCacheEntry", entry)["ok"])
}

func TestS3StorageSignsLikeAWS(t *testing.T) {
	storage := &s3Storage{opts: S3Options{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}}
	req := httptest.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	storage.sign(req, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7", req.Header.Get("Authorization"))
}
