// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
)

var actionFetches sync.Map

const treeSizes = ".sizes" // beside the tree rather than in it, which is copied into job containers as it is

type treeFile struct {
	Name []byte // not a string, which JSON would turn into UTF-8, as git allows any bytes but NUL
	Size int64
}

type actionFetch struct {
	done     chan struct{}
	dir, sha string
	err      error
	retry    bool // the leader was cancelled or panicked, so followers fetch themselves
}

var archiveRetryDelay = func(retryAfter string) time.Duration { // as actions/runner backs off
	if seconds, err := strconv.Atoi(retryAfter); err == nil {
		return min(max(time.Duration(seconds)*time.Second, 10*time.Second), 10*time.Minute)
	}
	return time.Duration(10+rand.IntN(21)) * time.Second
}

func FetchAction(ctx context.Context, input NewGitCloneExecutorInput) (string, string, error) { // input.Dir is one clone URL and ref's, holding a directory per commit
	if !reflect.ValueOf(input).Comparable() {
		return fetchAction(ctx, input)
	}
	for ctx.Err() == nil {
		call := &actionFetch{done: make(chan struct{}), retry: true}
		previous, loaded := actionFetches.LoadOrStore(input, call)
		if !loaded {
			func() {
				defer func() { actionFetches.Delete(input); close(call.done) }()
				call.dir, call.sha, call.err = fetchAction(ctx, input)
				call.retry = ctx.Err() != nil
			}()
			if call.retry {
				continue
			}
			return call.dir, call.sha, call.err
		}
		call, _ = previous.(*actionFetch)
		select {
		case <-ctx.Done():
		case <-call.done:
			if !call.retry && ctx.Err() == nil {
				return call.dir, call.sha, call.err
			}
		}
	}
	return "", "", ctx.Err()
}

func fetchAction(ctx context.Context, input NewGitCloneExecutorInput) (string, string, error) {
	unlock, err := AcquireCloneLock(ctx, input.Dir)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	if err := os.MkdirAll(input.Dir, 0o755); err != nil {
		return "", "", err
	}

	tree := func(sha string) string { return filepath.Join(input.Dir, sha) }
	record := filepath.Join(input.Dir, "ref")
	data, _ := os.ReadFile(record)
	sha, etag, _ := strings.Cut(string(data), "\n")
	switch {
	case !isFullObjectID(sha):
		sha, etag = "", ""
	case input.OfflineMode && exists(tree(sha)) && !exists(tree(sha)+treeSizes):
		return tree(sha), sha, nil // from an older runner, with no sizes to check, so offline mode uses it as before
	case !treeIntact(input.Dir, sha):
		sha, etag = "", ""
	case input.OfflineMode || isFullObjectID(input.Ref):
		return tree(sha), sha, nil
	}

	newSha, newEtag, err := downloadArchive(ctx, input, sha, etag)
	if err != nil && ctx.Err() == nil {
		common.Logger(ctx).Debugf("Cloning %s@%s: %v", input.URL, input.Ref, err)
		newEtag = ""
		newSha, err = exportClone(ctx, input)
	}
	if err != nil {
		return "", "", err
	}
	if newSha != sha || newEtag != etag {
		if err := os.WriteFile(record, []byte(newSha+"\n"+newEtag), 0o600); err != nil {
			return "", "", err
		}
	}
	return tree(newSha), newSha, nil
}

func downloadArchive(ctx context.Context, input NewGitCloneExecutorInput, sha, etag string) (string, string, error) {
	rewritten, err := gitOutput(ctx, "", "ls-remote", "--get-url", input.URL)
	if err != nil {
		return "", "", err
	}
	source, err := url.Parse(rewritten)
	if err != nil || source.Scheme != "http" && source.Scheme != "https" || hexRefRegex.MatchString(input.Ref) && !isFullObjectID(input.Ref) { // only git tells a short commit from a branch
		return "", "", errors.New("no archive")
	}
	archive := source.JoinPath("archive", input.Ref+".tar.gz")
	if source.Host == "github.com" {
		source.Host = "codeload.github.com"
		archive = source.JoinPath("tar.gz", input.Ref)
	}
	client := &http.Client{Transport: input.Transport, Timeout: 5 * time.Minute, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if request.URL.Host != via[0].URL.Host || request.URL.Scheme != via[0].URL.Scheme { // the token stays on its origin
			request.Header.Del("Authorization")
		}
		return nil
	}}
	for attempt := 1; ; attempt++ {
		newSha, newEtag, retryAfter, err := downloadArchiveOnce(ctx, client, archive.String(), input.Token, sha, etag, input.Dir)
		if retryAfter == 0 || attempt == 3 || ctx.Err() != nil {
			return newSha, newEtag, err
		}
		common.Logger(ctx).Warnf("Failed to download action '%s'. Error: %v. Back off %s before retry.", archive.Redacted(), err, retryAfter)
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(retryAfter):
		}
	}
}

func downloadArchiveOnce(ctx context.Context, client *http.Client, archive, token, sha, etag, dir string) (string, string, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archive, nil)
	if err != nil {
		return "", "", 0, err
	}
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	if token != "" {
		request.SetBasicAuth("token", token)
	}
	response, err := client.Do(request)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return "", "", 0, err
		}
		return "", "", archiveRetryDelay(""), err
	}
	defer response.Body.Close()
	switch status := response.StatusCode; {
	case status == http.StatusNotModified && sha != "":
		return sha, etag, 0, nil
	case status == http.StatusOK:
		newEtag := response.Header.Get("ETag")
		gz, err := gzip.NewReader(response.Body)
		if err == nil {
			sha, err = extractTree(ctx, gz, dir, !strings.HasSuffix(newEtag, `-false"`)) // Gitea's mark of an unprefixed archive
		}
		var netErr net.Error
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) {
			return "", "", archiveRetryDelay(""), err
		}
		return sha, newEtag, 0, err
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return "", "", archiveRetryDelay(response.Header.Get("Retry-After")), errors.New(response.Status)
	default:
		return "", "", 0, errors.New(response.Status)
	}
}

func exportClone(ctx context.Context, input NewGitCloneExecutorInput) (string, error) {
	clone := input
	clone.Dir = filepath.Join(input.Dir, "git")
	sha, _, err := resolveClone(ctx, clone)
	if err != nil || treeIntact(input.Dir, sha) {
		return sha, err
	}
	archive, err := runGit(ctx, clone.Dir, nil, "archive", "--format=tar", sha)
	if err != nil {
		return "", err
	}
	_, err = extractTree(ctx, bytes.NewReader(archive), input.Dir, false)
	return sha, err
}

func extractTree(ctx context.Context, archive io.Reader, dir string, strip bool) (string, error) { // into dir/<commit of the pax header>, strip dropping the directory wrapping the files
	temp, err := os.MkdirTemp(dir, "tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	root, err := os.OpenRoot(temp)
	if err != nil {
		return "", err
	}
	defer root.Close()

	sha := ""
	sizes := map[string]int64{}
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			if sha = header.PAXRecords["comment"]; isFullObjectID(sha) && treeIntact(dir, sha) {
				return sha, nil
			}
			continue
		}
		name := header.Name
		if strip {
			_, name, _ = strings.Cut(name, "/")
		}
		if name = strings.TrimSuffix(name, "/"); name == "" {
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			sizes[name], err = writeTreeFile(root, name, header.Mode, reader)
		case tar.TypeSymlink:
			if err = root.Symlink(header.Linkname, name); err != nil && runtime.GOOS == "windows" {
				sizes[name], err = writeTreeFile(root, name, 0, strings.NewReader(header.Linkname))
			}
		}
		if err != nil {
			return "", err
		}
	}
	if !isFullObjectID(sha) {
		return "", errors.New("the archive names no commit")
	}
	if err := root.Close(); err != nil {
		return "", err
	}
	return sha, installTree(ctx, dir, sha, temp, sizes)
}

var lockTree = AcquireCloneLock // replaced in tests to see installTree wait for a job holding the tree

// installTree moves temp into place as dir/sha, replacing a tree that failed treeIntact.
func installTree(ctx context.Context, dir, sha, temp string, sizes map[string]int64) error {
	files := make([]treeFile, 0, len(sizes))
	for name, size := range sizes {
		files = append(files, treeFile{[]byte(name), size})
	}
	data, err := json.Marshal(files)
	if err != nil {
		return err
	}
	// Written before the swap: it describes the commit, not a copy of it, so it holds for whichever tree is left.
	if err := os.WriteFile(filepath.Join(dir, sha+treeSizes), data, 0o600); err != nil { // a torn record fails treeIntact, so it needs no rename
		return err
	}
	tree := filepath.Join(dir, sha)
	if !exists(tree) {
		return os.Rename(temp, tree)
	}
	stale, err := os.MkdirTemp(dir, "tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stale)          // after the unlock, as no job reads the old tree once it is moved
	unlock, err := lockTree(ctx, tree) // jobs copying the old tree hold it
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Rename(tree, filepath.Join(stale, sha)); err != nil {
		return err
	}
	return os.Rename(temp, tree)
}

// treeIntact reports whether dir/sha still holds each file extracted into it at the size it was written.
// Only sizes are compared, not content: a stat per file is cheap enough for every job, hashing is not.
func treeIntact(dir, sha string) bool {
	data, err := os.ReadFile(filepath.Join(dir, sha+treeSizes))
	if err != nil || !exists(filepath.Join(dir, sha)) {
		return false
	}
	var files []treeFile
	if json.Unmarshal(data, &files) != nil {
		return false
	}
	for _, file := range files {
		info, err := os.Lstat(filepath.Join(dir, sha, filepath.FromSlash(string(file.Name))))
		if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			return false
		}
	}
	return true
}

func writeTreeFile(root *os.Root, name string, mode int64, content io.Reader) (int64, error) {
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(file, content)
	return n, errors.Join(err, file.Close())
}

func exists(name string) bool {
	_, err := os.Stat(name)
	return err == nil
}
