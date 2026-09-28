// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// S3Options configures the bucket that stores completed cache entries.
type S3Options struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	PathStyle *bool // nil uses virtual-hosted requests on AWS and path-style ones elsewhere
	AccessKey string
	SecretKey string
}

type cacheStorage interface {
	Exist(uint64) (bool, error)
	Write(uint64, int64, io.Reader) error
	WriteBlock(uint64, string, io.Reader) error
	OrderBlocks(uint64, []string) error
	Commit(uint64, int64) (int64, error)
	Serve(http.ResponseWriter, *http.Request, uint64)
	Remove(uint64) error
}

const s3PartSize = 5 << 30 // the largest part S3 accepts

// s3Storage stages uploads on disk and keeps completed entries and their metadata in a bucket.
type s3Storage struct {
	*Storage
	client    *http.Client
	bucketURL *url.URL
	opts      S3Options
}

type s3Part struct {
	PartNumber int
	ETag       string
}

func newS3Storage(staging *Storage, opts S3Options) (*s3Storage, error) {
	endpoint, err := url.Parse(opts.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("invalid S3 endpoint %q", opts.Endpoint)
	}
	transport, _ := http.DefaultTransport.(*http.Transport)
	transport = transport.Clone()
	transport.ResponseHeaderTimeout = time.Minute // a stalled endpoint must not hold the store forever
	pathStyle := !strings.HasSuffix(endpoint.Hostname(), ".amazonaws.com")
	if opts.PathStyle != nil {
		pathStyle = *opts.PathStyle
	}
	if pathStyle {
		endpoint.Path = "/" + opts.Bucket
	} else {
		endpoint.Host = opts.Bucket + "." + endpoint.Host
		endpoint.Path = "/"
	}
	opts.Region = cmp.Or(opts.Region, "us-east-1")
	return &s3Storage{Storage: staging, client: &http.Client{Transport: transport}, bucketURL: endpoint, opts: opts}, nil
}

func (s *s3Storage) blobKey(id uint64) string {
	return path.Join(s.opts.Prefix, "blobs", strconv.FormatUint(id, 10))
}

func (s *s3Storage) metadataDir() string {
	return path.Join(s.opts.Prefix, "metadata") + "/"
}

func (s *s3Storage) metadataKey(id uint64) string {
	return s.metadataDir() + strconv.FormatUint(id, 10) + ".json"
}

func (s *s3Storage) url(key string, query url.Values) *url.URL {
	u := *s.bucketURL
	u.Path = path.Join(u.Path, key)
	u.RawPath = strings.NewReplacer("+", "%20", "%2F", "/").Replace(url.QueryEscape(u.Path)) // SigV4 encoding
	u.RawQuery = strings.ReplaceAll(query.Encode(), "+", "%20")
	return &u
}

// sign authorizes req with AWS Signature Version 4, leaving the payload unsigned so bodies can stream.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
func (s *s3Storage) sign(req *http.Request, now time.Time) {
	date := now.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", date)
	if req.Header.Get("X-Amz-Content-Sha256") == "" {
		req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	}
	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		if name = strings.ToLower(name); strings.HasPrefix(name, "x-amz-") {
			headers[name] = strings.Join(values, ",")
		}
	}
	names := slices.Sorted(maps.Keys(headers))
	canonical := []string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery}
	for _, name := range names {
		canonical = append(canonical, name+":"+headers[name])
	}
	canonical = append(canonical, "", strings.Join(names, ";"), req.Header.Get("X-Amz-Content-Sha256"))
	scope := date[:8] + "/" + s.opts.Region + "/s3/aws4_request"
	signature := []byte("AWS4" + s.opts.SecretKey)
	for _, data := range []string{date[:8], s.opts.Region, "s3", "aws4_request", fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%x", date, scope, sha256.Sum256([]byte(strings.Join(canonical, "\n"))))} { // key derivation, the last round signs
		mac := hmac.New(sha256.New, signature)
		_, _ = mac.Write([]byte(data))
		signature = mac.Sum(nil)
	}
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%x", s.opts.AccessKey, scope, strings.Join(names, ";"), signature))
}

// do returns the response to a signed request when it succeeded, fs.ErrNotExist for a missing key.
func (s *s3Storage) do(ctx context.Context, method, key string, query url.Values, header http.Header, body io.Reader) (*http.Response, error) {
	var size int64
	if sized, ok := body.(interface{ Size() int64 }); ok {
		size = sized.Size()
	}
	if size == 0 {
		body = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, s.url(key, query).String(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	maps.Copy(req.Header, header)
	s.sign(req, time.Now())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}
	defer drain(resp)
	var failure struct{ Code, Message string }
	_ = xml.NewDecoder(resp.Body).Decode(&failure)
	if resp.StatusCode == http.StatusNotFound && (failure.Code == "NoSuchKey" || failure.Code == "" && method == http.MethodHead) {
		return nil, fs.ErrNotExist
	}
	return nil, fmt.Errorf("S3 %s %s: %s", method, key, cmp.Or(strings.TrimSpace(failure.Code+" "+failure.Message), resp.Status))
}

func (s *s3Storage) call(ctx context.Context, method, key string, query url.Values, body io.Reader, result any) (http.Header, error) {
	resp, err := s.do(ctx, method, key, query, nil, body)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if result == nil {
		return resp.Header, nil
	}
	return resp.Header, xml.NewDecoder(resp.Body).Decode(result)
}

// drain reads a response to its end before closing it, so its connection gets reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (s *s3Storage) Exist(id uint64) (bool, error) {
	_, err := s.call(context.Background(), http.MethodHead, s.blobKey(id), nil, nil, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *s3Storage) Commit(id uint64, size int64) (int64, error) {
	written, err := s.Storage.Commit(id, size)
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Storage.Remove(id) }()
	file, err := os.Open(s.filename(id))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if err := s.upload(context.Background(), s.blobKey(id), file, written); err != nil {
		return 0, fmt.Errorf("upload cache to S3: %w", err)
	}
	return written, nil
}

// upload always goes in parts, the only way S3 takes an object over 5 GiB.
func (s *s3Storage) upload(ctx context.Context, key string, file io.ReaderAt, size int64) error {
	var created struct {
		UploadID string `xml:"UploadId"`
	}
	if _, err := s.call(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, &created); err != nil {
		return err
	}
	var completion struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []s3Part `xml:"Part"`
	}
	for offset := int64(0); offset == 0 || offset < size; offset += s3PartSize {
		number, length := len(completion.Parts)+1, min(s3PartSize, size-offset)
		query := url.Values{"partNumber": {strconv.Itoa(number)}, "uploadId": {created.UploadID}}
		header, err := s.call(ctx, http.MethodPut, key, query, io.NewSectionReader(file, offset, length), nil)
		if err != nil {
			return err
		}
		completion.Parts = append(completion.Parts, s3Part{number, header.Get("ETag")})
	}
	body, err := xml.Marshal(completion)
	if err != nil {
		return err
	}
	var result struct{ Code, Message string } // a completion can fail after its 200 status
	if _, err := s.call(ctx, http.MethodPost, key, url.Values{"uploadId": {created.UploadID}}, bytes.NewReader(body), &result); err != nil {
		return err
	}
	if result.Code != "" {
		return fmt.Errorf("S3 complete upload %s: %s %s", key, result.Code, result.Message)
	}
	return nil
}

func (s *s3Storage) Serve(w http.ResponseWriter, r *http.Request, id uint64) {
	header := http.Header{}
	if byteRange := r.Header.Get("Range"); byteRange != "" {
		header.Set("Range", byteRange)
	}
	resp, err := s.do(r.Context(), http.MethodGet, s.blobKey(id), nil, header, nil)
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "read cache from S3", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, name := range []string{"Content-Length", "Content-Range"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Remove unpublishes an entry before deleting its blob, so no runner imports it without its bytes.
func (s *s3Storage) Remove(id uint64) error {
	for _, key := range []string{s.metadataKey(id), s.blobKey(id)} {
		if _, err := s.call(context.Background(), http.MethodDelete, key, nil, nil, nil); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s from S3: %w", key, err)
		}
	}
	return s.Storage.Remove(id)
}

func (s *s3Storage) publish(cache *Cache) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	if _, err := s.call(context.Background(), http.MethodPut, s.metadataKey(cache.ID), nil, bytes.NewReader(data), nil); err != nil {
		return fmt.Errorf("publish cache to S3: %w", err)
	}
	return nil
}

func (s *s3Storage) published(ctx context.Context) (map[uint64]bool, error) {
	ids := make(map[uint64]bool)
	query := url.Values{"list-type": {"2"}, "prefix": {s.metadataDir()}}
	for {
		var page struct {
			Contents              []struct{ Key string }
			IsTruncated           bool
			NextContinuationToken string
		}
		if _, err := s.call(ctx, http.MethodGet, "", query, nil, &page); err != nil {
			return nil, fmt.Errorf("list S3 cache metadata: %w", err)
		}
		for _, object := range page.Contents {
			if id, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(object.Key, s.metadataDir()), ".json"), 10, 64); err == nil {
				ids[id] = true
			}
		}
		if !page.IsTruncated {
			return ids, nil
		}
		query.Set("continuation-token", page.NextContinuationToken)
	}
}

func (s *s3Storage) fetch(ctx context.Context, id uint64) (*Cache, error) {
	resp, err := s.do(ctx, http.MethodGet, s.metadataKey(id), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	cache := &Cache{}
	if err := json.UnmarshalRead(resp.Body, cache); err != nil {
		return nil, fmt.Errorf("read S3 cache metadata %d: %w", id, err)
	}
	return cache, nil
}
