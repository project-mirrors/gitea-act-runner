// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_S3SharesEntriesAndDropsExpiredOnes(t *testing.T) {
	var objects sync.Map
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		key := strings.TrimPrefix(request.URL.Path, "/cache/")
		switch {
		case request.Method == http.MethodPut:
			if strings.Contains(key, "/blobs/") {
				uploads.Add(1)
			}
			body, err := io.ReadAll(request.Body)
			assert.NoError(t, err)
			objects.Store(key, body)
		case request.URL.Query().Has("list-type"):
			_, _ = io.WriteString(writer, "<ListBucketResult>")
			objects.Range(func(stored, _ any) bool {
				_, _ = io.WriteString(writer, "<Contents><Key>"+stored.(string)+"</Key></Contents>")
				return true
			})
			_, _ = io.WriteString(writer, "</ListBucketResult>")
		default:
			if body, ok := objects.Load(key); ok {
				_, _ = writer.Write(body.([]byte))
			} else {
				writer.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	t.Cleanup(server.Close)
	opts := Options{S3: &S3Options{Endpoint: server.URL, Bucket: "cache", Prefix: "runners", AccessKey: "key", SecretKey: "secret"}}
	saver, restorer := startTestHandler(t, opts), startTestHandler(t, opts)
	entry := map[string]any{"key": "deps", "version": "v1"}

	finalized, _ := saveV2(t, saver, "deps", "v1", []byte("archive"))
	require.Equal(t, true, finalized["ok"])
	assert.EqualValues(t, 1, uploads.Load())
	require.NoError(t, restorer.importMetadata(t.Context()))
	found := v2Call(t, restorer, testClient, "GetCacheEntryDownloadURL", entry)
	require.Equal(t, true, found["ok"])
	assert.Equal(t, []byte("archive"), getURL(t, found["signed_download_url"].(string)))

	objects.Clear()
	require.NoError(t, restorer.importMetadata(t.Context()))
	assert.Equal(t, true, v2Call(t, restorer, testClient, "CreateCacheEntry", entry)["ok"])
}

type s3TestTransport func(*http.Request) (*http.Response, error)

func (transport s3TestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func TestS3StorageUploadsLargeObjectsInPartsAndAbortsOnFailure(t *testing.T) {
	for _, failingPart := range []string{"", "2"} {
		var parts, aborts atomic.Int32
		var completion struct {
			Parts []s3Part `xml:"Part"`
		}
		storage := &s3Storage{bucketURL: httptest.NewRequest(http.MethodGet, "http://cache.local/bucket", nil).URL}
		storage.client = &http.Client{Transport: s3TestTransport(func(req *http.Request) (*http.Response, error) {
			query, response := req.URL.Query(), &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}
			switch {
			case query.Has("uploads"):
				response.Body = io.NopCloser(strings.NewReader("<InitiateMultipartUploadResult><UploadId>upload</UploadId></InitiateMultipartUploadResult>"))
			case req.Method == http.MethodPut:
				parts.Add(1)
				response.Header.Set("ETag", "etag-"+query.Get("partNumber"))
				if query.Get("partNumber") == failingPart {
					response.StatusCode, response.Body = http.StatusBadRequest, io.NopCloser(strings.NewReader("<Error><Code>InvalidPart</Code></Error>"))
				}
			case req.Method == http.MethodPost:
				require.NoError(t, xml.NewDecoder(req.Body).Decode(&completion))
				response.Body = io.NopCloser(strings.NewReader("<CompleteMultipartUploadResult/>"))
			case req.Method == http.MethodDelete:
				aborts.Add(1)
			}
			return response, nil
		})}
		err := storage.upload(t.Context(), "blobs/1", strings.NewReader(""), 2*int64(s3PartSize)+1)
		assert.EqualValues(t, 3, parts.Load())
		if failingPart == "" {
			require.NoError(t, err)
			assert.Equal(t, []s3Part{{1, "etag-1"}, {2, "etag-2"}, {3, "etag-3"}}, completion.Parts)
			assert.Zero(t, aborts.Load())
		} else {
			require.ErrorContains(t, err, "InvalidPart")
			assert.EqualValues(t, 1, aborts.Load())
		}
	}
}

func TestS3StorageSignsLikeAWS(t *testing.T) {
	storage := &s3Storage{opts: S3Options{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}}
	req := httptest.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil)
	req.Header.Set("X-Amz-Content-Sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	storage.sign(req, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	assert.Equal(t, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7", req.Header.Get("Authorization"))
}
