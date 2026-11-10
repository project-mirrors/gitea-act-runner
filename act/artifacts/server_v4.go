// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifacts

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type serverV4 struct { // the subset of Gitea's v4 artifact service that the toolkit uses, unauthenticated like the v3 routes beside it
	dir     string
	mu      sync.Mutex
	lastID  int64
	created map[string]bool
}

const (
	zipMimeType   = "application/zip"
	maxTwirpBytes = 1 << 20
)

func registerV4(router *http.ServeMux, baseDir string) {
	server := &serverV4{dir: filepath.Join(baseDir, "v4"), lastID: time.Now().UnixMilli(), created: map[string]bool{}} // IDs stay unique across invocations sharing baseDir
	router.HandleFunc("POST "+artifactServicePath+"/CreateArtifact", twirp(server.createArtifact))
	router.HandleFunc("POST "+artifactServicePath+"/FinalizeArtifact", twirp(server.finalizeArtifact))
	router.HandleFunc("POST "+artifactServicePath+"/ListArtifacts", twirp(server.listArtifacts))
	router.HandleFunc("POST "+artifactServicePath+"/GetSignedArtifactURL", twirp(server.getSignedArtifactURL))
	router.HandleFunc("POST "+artifactServicePath+"/DeleteArtifact", twirp(server.deleteArtifact))
	router.HandleFunc("PUT "+artifactServicePath+"/UploadArtifact", server.uploadArtifact)
	router.HandleFunc("GET "+artifactServicePath+"/DownloadArtifact", server.downloadArtifact)
}

type twirpError struct {
	status int
	code   string
	msg    string
}

func (e *twirpError) Error() string { return e.msg }

func notFound(name string) error {
	return &twirpError{http.StatusNotFound, "not_found", fmt.Sprintf("artifact %q not found", name)}
}

func badRequest(format string, args ...any) error {
	return &twirpError{http.StatusBadRequest, "invalid_argument", fmt.Sprintf(format, args...)}
}

func twirp[Req, Resp any](handle func(*http.Request, Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Req
		var resp Resp
		err := json.UnmarshalRead(io.LimitReader(r.Body, maxTwirpBytes), &req, json.MatchCaseInsensitiveNames(true)) // also matches the toolkit's proto field names
		if err != nil {
			err = badRequest("malformed request: %v", err)
		} else {
			resp, err = handle(r, req)
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			terr := &twirpError{http.StatusInternalServerError, "internal", err.Error()}
			errors.As(err, &terr)
			w.WriteHeader(terr.status)
			_ = json.MarshalWrite(w, map[string]string{"code": terr.code, "msg": terr.msg})
			return
		}
		_ = json.MarshalWrite(w, resp)
	}
}

func fileName(name string) string { // safe on any filesystem for any client-chosen name, case-insensitive ones included
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

func (s *serverV4) runDir(runID string) string {
	return filepath.Join(s.dir, fileName(runID))
}

func (s *serverV4) artifactDir(runID, name string) string {
	return filepath.Join(s.runDir(runID), fileName(name))
}

func (s *serverV4) urlDir(r *http.Request) string {
	return s.artifactDir(r.URL.Query().Get("run"), r.URL.Query().Get("name"))
}

func readMeta(dir string) (*monolithArtifact, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta monolithArtifact
	return &meta, json.Unmarshal(data, &meta)
}

func writeMeta(dir string, meta *monolithArtifact) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

func blobURL(r *http.Request, endpoint, runID, name string) string {
	return "http://" + r.Host + artifactServicePath + "/" + endpoint + "?" + url.Values{"run": {runID}, "name": {name}}.Encode()
}

func (s *serverV4) createArtifact(r *http.Request, req createArtifactRequest) (createArtifactResponse, error) { // an exec invocation re-runs its workflows, so it replaces earlier invocations' artifacts but conflicts with its own
	dir := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.created[dir] {
		return createArtifactResponse{}, &twirpError{http.StatusConflict, "already_exists", "an artifact with this name already exists on the workflow run"}
	}
	if err := os.RemoveAll(dir); err != nil {
		return createArtifactResponse{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		return createArtifactResponse{}, err
	}
	s.lastID++
	if err := writeMeta(dir, &monolithArtifact{
		WorkflowRunBackendID: req.WorkflowRunBackendID,
		DatabaseID:           s.lastID,
		Name:                 req.Name,
		CreatedAt:            time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		return createArtifactResponse{}, err
	}
	s.created[dir] = true
	return createArtifactResponse{Ok: true, SignedUploadURL: blobURL(r, "UploadArtifact", req.WorkflowRunBackendID, req.Name)}, nil
}

type blockList struct {
	Blocks []string `xml:",any"` // Latest, Committed and Uncommitted all name a staged block
}

func (s *serverV4) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	dir := s.urlDir(r)
	if meta, err := readMeta(dir); err != nil || meta.Digest != "" {
		http.Error(w, "no artifact awaits an upload", http.StatusNotFound)
		return
	}
	contentType := r.Header.Get("x-ms-blob-content-type")
	var err error
	switch comp := r.URL.Query().Get("comp"); comp {
	case "block":
		err = writeFile(filepath.Join(dir, "blocks", fileName(r.URL.Query().Get("blockid"))), r.Body)
	case "blocklist":
		err = commitBlob(dir, contentType, func(out io.Writer) error { return joinBlocks(out, dir, r.Body) })
	case "": // Put Blob, which docker/build-push-action uses
		err = commitBlob(dir, contentType, func(out io.Writer) error {
			_, err := io.Copy(out, r.Body)
			return err
		})
	default:
		http.Error(w, "unsupported comp "+comp, http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func writeFile(name string, body io.Reader) error {
	file, err := os.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, body)
	return errors.Join(err, file.Close())
}

func commitBlob(dir, contentType string, write func(io.Writer) error) error { // replaces the blob whole and keeps its content type, as Azure does
	out, err := os.CreateTemp(dir, "blob")
	if err != nil {
		return err
	}
	if err = errors.Join(write(out), out.Close()); err == nil {
		err = os.Rename(out.Name(), filepath.Join(dir, "blob"))
	}
	if err != nil {
		return errors.Join(err, os.Remove(out.Name()))
	}
	return os.WriteFile(filepath.Join(dir, "content-type"), []byte(contentType), 0o644)
}

func joinBlocks(out io.Writer, dir string, body io.Reader) error { // keeps the blocks for finalizeArtifact, so that a retried commit can join them again
	var list blockList
	if err := xml.NewDecoder(io.LimitReader(body, maxTwirpBytes)).Decode(&list); err != nil {
		return err
	}
	for _, id := range list.Blocks {
		if err := appendBlock(out, filepath.Join(dir, "blocks", fileName(id))); err != nil {
			return err
		}
	}
	return nil
}

func appendBlock(out io.Writer, name string) error {
	in, err := os.Open(name)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(out, in)
	return err
}

func (s *serverV4) finalizeArtifact(_ *http.Request, req finalizeArtifactRequest) (finalizeArtifactResponse, error) {
	dir := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	meta, err := readMeta(dir)
	if err != nil {
		return finalizeArtifactResponse{}, notFound(req.Name)
	} else if meta.Digest != "" {
		return finalizeArtifactResponse{Ok: true, ArtifactID: meta.DatabaseID}, nil
	}
	blob, err := os.Open(filepath.Join(dir, "blob"))
	if err != nil {
		return finalizeArtifactResponse{}, badRequest("artifact %q has no upload", req.Name)
	}
	defer blob.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, blob)
	if err != nil {
		return finalizeArtifactResponse{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if size != req.Size {
		return finalizeArtifactResponse{}, badRequest("size %d does not match the %d bytes uploaded", req.Size, size)
	}
	if req.Hash != "" && req.Hash != digest {
		return finalizeArtifactResponse{}, badRequest("hash %s does not match the uploaded %s", req.Hash, digest)
	}
	s.mu.Lock() // only after hashing, so finalizing one large artifact does not stall the others
	defer s.mu.Unlock()
	if current, err := readMeta(dir); err != nil || current.DatabaseID != meta.DatabaseID {
		return finalizeArtifactResponse{}, notFound(req.Name)
	}
	meta.Size, meta.Digest = size, digest
	if err := writeMeta(dir, meta); err != nil {
		return finalizeArtifactResponse{}, err
	}
	return finalizeArtifactResponse{Ok: true, ArtifactID: meta.DatabaseID}, os.RemoveAll(filepath.Join(dir, "blocks"))
}

func (s *serverV4) listArtifacts(_ *http.Request, req listArtifactsRequest) (listArtifactsResponse, error) {
	run := s.runDir(req.WorkflowRunBackendID)
	entries, err := os.ReadDir(run)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return listArtifactsResponse{}, err
	}
	var resp listArtifactsResponse
	for _, entry := range entries {
		meta, err := readMeta(filepath.Join(run, entry.Name()))
		if err != nil || meta.Digest == "" ||
			req.NameFilter != "" && req.NameFilter != meta.Name || req.IDFilter != 0 && req.IDFilter != meta.DatabaseID {
			continue
		}
		resp.Artifacts = append(resp.Artifacts, *meta)
	}
	slices.SortFunc(resp.Artifacts, func(left, right monolithArtifact) int { return cmp.Compare(left.DatabaseID, right.DatabaseID) })
	return resp, nil
}

func (s *serverV4) getSignedArtifactURL(r *http.Request, req getSignedArtifactURLRequest) (getSignedArtifactURLResponse, error) {
	if meta, err := readMeta(s.artifactDir(req.WorkflowRunBackendID, req.Name)); err != nil || meta.Digest == "" {
		return getSignedArtifactURLResponse{}, notFound(req.Name)
	}
	return getSignedArtifactURLResponse{SignedURL: blobURL(r, "DownloadArtifact", req.WorkflowRunBackendID, req.Name)}, nil
}

func (s *serverV4) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	dir := s.urlDir(r)
	meta, err := readMeta(dir)
	if err != nil || meta.Digest == "" {
		http.Error(w, "artifact not found", http.StatusNotFound)
		return
	}
	blob, err := os.Open(filepath.Join(dir, "blob"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer blob.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "content-type"))
	contentType, filename := string(data), meta.Name
	if !strings.Contains(contentType, "/") {
		contentType = zipMimeType
	}
	if contentType == zipMimeType && !strings.HasSuffix(filename, ".zip") {
		filename += ".zip"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+strings.ReplaceAll(url.QueryEscape(filename), "+", "%20")) // RFC 5987 attr-chars only, which the toolkit and mime.ParseMediaType both accept
	http.ServeContent(w, r, "", meta.CreatedAt, blob)
}

func (s *serverV4) deleteArtifact(_ *http.Request, req deleteArtifactRequest) (deleteArtifactResponse, error) {
	dir := s.artifactDir(req.WorkflowRunBackendID, req.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMeta(dir)
	if err != nil {
		return deleteArtifactResponse{}, notFound(req.Name)
	}
	delete(s.created, dir)
	return deleteArtifactResponse{Ok: true, ArtifactID: meta.DatabaseID}, os.RemoveAll(dir)
}
