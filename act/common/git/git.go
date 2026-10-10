// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/internal/pkg/lock"

	log "github.com/sirupsen/logrus"
)

var (
	codeCommitHTTPRegex = regexp.MustCompile(`^https?://git-codecommit\.(.+)\.amazonaws.com/v1/repos/(.+)$`)
	codeCommitSSHRegex  = regexp.MustCompile(`ssh://git-codecommit\.(.+)\.amazonaws.com/v1/repos/(.+)$`)
	hexRefRegex         = regexp.MustCompile(`^[0-9a-fA-F]+$`)

	LocalEnvVars = []string{ // `git rev-parse --local-env-vars`, inherited ones point git at another repository
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_OBJECT_DIRECTORY",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE", "GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS",
		"GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
	}

	cloneLocks lock.Keyed[string] // key: clone target directory

	ErrShortRef = errors.New("short SHA references are not supported")
)

// AcquireCloneLock serializes access to dir across runner processes, or within this one where no lock file can be created. Readers of dir must hold it too.
func AcquireCloneLock(ctx context.Context, dir string) (func(), error) {
	dir = filepath.Clean(dir)
	if runtime.GOOS == "plan9" {
		return cloneLocks.Lock(dir), nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	for {
		release, err := lock.TryLock(dir)
		if err == nil {
			return func() { _ = release() }, nil
		}
		if !errors.Is(err, lock.ErrLocked) {
			return cloneLocks.Lock(dir), nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type Error struct {
	err    error
	commit string
}

func (e *Error) Error() string  { return e.err.Error() }
func (e *Error) Unwrap() error  { return e.err }
func (e *Error) Commit() string { return e.commit }

// FindGitRevision get the current git revision
func FindGitRevision(ctx context.Context, file string) (shortSHA, sha string, err error) {
	logger := common.Logger(ctx)
	sha, err = gitOutput(ctx, file, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		logger.WithError(err).Error("path", file, "not located inside a git repository")
		return "", "", err
	}
	if len(sha) < 7 {
		return "", "", errors.New("HEAD could not be resolved")
	}
	logger.Debugf("Found revision: %s", sha)
	return sha[:7], sha, nil
}

// FindGitRef get the current git ref
func FindGitRef(ctx context.Context, file string) (string, error) {
	logger := common.Logger(ctx)
	logger.Debugf("Loading revision from git directory")
	_, revision, err := FindGitRevision(ctx, file)
	if err != nil {
		return "", err
	}
	logger.Debugf("HEAD points to '%s'", revision)

	refs, err := gitOutput(ctx, file, "for-each-ref", "--points-at=HEAD", "--format=%(refname)", "refs/tags", "refs/heads")
	if err != nil {
		return "", err
	}
	var tag, branch string
	for ref := range strings.FieldsSeq(refs) {
		switch {
		case strings.HasPrefix(ref, "refs/tags/"):
			tag = ref
		case strings.HasPrefix(ref, "refs/heads/"):
			branch = ref
		}
	}
	if tag != "" {
		return tag, nil
	}
	if branch != "" {
		return branch, nil
	}
	return "", fmt.Errorf("failed to identify reference (tag/branch) for the checked-out revision '%s'", revision)
}

// FindGithubRepo get the repo
func FindGithubRepo(ctx context.Context, file string) (string, error) {
	url, err := findGitRemoteURL(ctx, file)
	if err != nil {
		return "", err
	}
	return findGitSlug(url)
}

func findGitRemoteURL(ctx context.Context, file string) (string, error) {
	urls, err := gitOutput(ctx, file, "config", "--get-all", "remote.origin.url") // `remote get-url` applies insteadOf
	if err != nil {
		return "", err
	}
	if urls == "" {
		return "", errors.New("remote 'origin' exists but has no URL")
	}
	url, _, _ := strings.Cut(urls, "\n")
	return url, nil
}

func findGitSlug(remoteURL string) (string, error) {
	if matches := codeCommitHTTPRegex.FindStringSubmatch(remoteURL); matches != nil {
		return matches[2], nil
	} else if matches := codeCommitSSHRegex.FindStringSubmatch(remoteURL); matches != nil {
		return matches[2], nil
	}
	if host, repoPath, ok := strings.Cut(remoteURL, ":"); ok && filepath.VolumeName(remoteURL) == "" && !strings.Contains(host, "/") && !strings.HasPrefix(repoPath, "//") {
		remoteURL = "ssh://" + host + "/" + repoPath // scp-like [user@]host:path
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return "", errors.New("cannot parse the origin remote URL")
	}
	owner, repo := path.Split(strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git"))
	owner = path.Base(owner)
	if parsed.Host == "" || repo == "" || owner == "." || owner == "/" {
		return "", errors.New("cannot determine owner and repository from the origin remote URL")
	}
	return owner + "/" + repo, nil
}

// NewGitCloneExecutorInput the input for the NewGitCloneExecutor
type NewGitCloneExecutorInput struct {
	URL         string
	Ref         string
	Dir         string
	Token       string
	OfflineMode bool

	// Depth limits the clone/fetch to the given number of commits from the tip of the requested ref.
	// 0 for full clone.
	Depth int

	// Quiet drops the informational clone line to debug level, for callers that log their own
	// download summary (the setup section's action report).
	Quiet bool

	// For Gitea
	InsecureSkipTLS               bool
	ClientCertFile, ClientKeyFile string // PEM, absolute as git runs in other directories
}

// CloneIfRequired reports whether an existing local clone was reused.
func CloneIfRequired(ctx context.Context, input NewGitCloneExecutorInput, logger log.FieldLogger) (bool, error) {
	var origin string
	_, err := os.Stat(filepath.Join(input.Dir, ".git")) // without it git would discover a repository around Dir
	if err == nil {
		origin, err = findGitRemoteURL(ctx, input.Dir)
	}
	if err == nil && origin == input.URL {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil { // a cancelled origin lookup must not discard a valid clone
		return false, ctxErr
	}
	if err == nil {
		logger.Debugf("Removing cached clone at %s because origin URL changed from %s to %s", input.Dir, origin, input.URL)
	} else if _, statErr := os.Stat(input.Dir); statErr == nil {
		logger.Debugf("Removing cached clone at %s because origin cannot be read: %v", input.Dir, err)
	}
	if err := os.RemoveAll(input.Dir); err != nil {
		return false, fmt.Errorf("remove cached clone %s: %w", input.Dir, err)
	}
	if err := clone(ctx, input); err != nil {
		return false, err
	}
	if err := os.Chmod(input.Dir, 0o755); err != nil {
		return false, err
	}
	return false, nil
}

// NewGitCloneExecutor creates an executor to clone git repos
func NewGitCloneExecutor(input NewGitCloneExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		if input.Quiet {
			logger.Debugf("git clone '%s' # ref=%s", input.URL, input.Ref)
		} else {
			logger.Infof("git clone '%s' # ref=%s", input.URL, input.Ref)
		}
		logger.Debugf("  cloning %s to %s", input.URL, input.Dir)
		unlock, err := AcquireCloneLock(ctx, input.Dir)
		if err != nil {
			return err
		}
		defer unlock()

		reused, err := CloneIfRequired(ctx, input, logger)
		if err != nil {
			return err
		}
		resolved, err := resolveRef(ctx, input.Dir, input.Ref)
		if errors.Is(err, ErrShortRef) || (err != nil && input.OfflineMode) {
			return err
		}
		if !input.OfflineMode && (err != nil || (reused && !isFullHash(input.Ref, resolved))) {
			if err := fetch(ctx, input); err != nil {
				return err
			}
			resolved, err = resolveRef(ctx, input.Dir, input.Ref)
		}
		if err != nil {
			logger.Errorf("Unable to resolve %s: %v", input.Ref, err)
			return err
		}
		if _, err := runGit(ctx, input.Dir, nil, "checkout", "--force", "--detach", resolved); err != nil {
			return fmt.Errorf("checkout %s: %w", resolved, err)
		}
		reusedMsg := ""
		if input.OfflineMode && reused {
			reusedMsg = " (reused in offline mode)"
		}
		logger.Debugf("Cloned %s to %s%s", input.URL, input.Dir, reusedMsg)
		logger.Debugf("Checked out %s", input.Ref)
		return nil
	}
}

func clone(ctx context.Context, input NewGitCloneExecutorInput) error {
	if input.Depth > 0 && isFullObjectID(input.Ref) {
		if err := shallowPinnedClone(ctx, input); err == nil {
			return nil
		} else if err := removePartialClone(input.Dir); err != nil {
			return err
		}
	}
	args := []string{"clone", "--no-checkout"}
	if input.Depth > 0 {
		args = append(args, "--no-local", "--depth", strconv.Itoa(input.Depth), "--no-tags")
		if input.Ref != "HEAD" && !strings.HasPrefix(input.Ref, "refs/") {
			args = append(args, "--branch", input.Ref)
		}
	}
	args = append(args, input.URL, input.Dir)
	if _, err := runGit(ctx, "", &input, args...); err == nil {
		return nil
	} else if input.Depth == 0 {
		return err
	}
	if err := removePartialClone(input.Dir); err != nil {
		return err
	}
	_, err := runGit(ctx, "", &input, "clone", "--no-checkout", input.URL, input.Dir)
	return err
}

func shallowPinnedClone(ctx context.Context, input NewGitCloneExecutorInput) error {
	if _, err := runGit(ctx, "", &input, "init", "--object-format="+ObjectFormat(input.Ref), input.Dir); err != nil {
		return err
	}
	if _, err := runGit(ctx, input.Dir, &input, "remote", "add", "origin", input.URL); err != nil {
		return err
	}
	_, err := runGit(ctx, input.Dir, &input, "fetch", "--depth", strconv.Itoa(input.Depth), "--no-tags", "origin", "+"+input.Ref+":refs/pinned/"+input.Ref)
	return err
}

func fetch(ctx context.Context, input NewGitCloneExecutorInput) error {
	args := []string{"fetch", "--force", "--prune"}
	if _, err := os.Stat(filepath.Join(input.Dir, ".git", "shallow")); err == nil {
		args = append(args, "--depth", "1", "--no-tags")
	}
	_, err := runGit(ctx, input.Dir, &input, append(args, "origin", "+"+input.Ref+":"+cachedRef(input.Ref))...) // git picks the remote ref, a tag before a branch
	return err
}

func cachedRef(ref string) string {
	return fmt.Sprintf("refs/runner/%x", sha256.Sum256([]byte(ref)))
}

func resolveRef(ctx context.Context, dir, ref string) (string, error) {
	if !isFullObjectID(ref) {
		for _, name := range []string{cachedRef(ref), "refs/tags/" + ref, "refs/remotes/origin/" + ref} {
			if sha, err := gitOutput(ctx, dir, "rev-parse", "--verify", name+"^{commit}"); err == nil {
				return sha, nil
			}
		}
	}
	sha, err := gitOutput(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	if err == nil && hexRefRegex.MatchString(ref) && len(sha) != len(ref) {
		return "", &Error{err: ErrShortRef, commit: sha}
	}
	return sha, err
}

func isFullObjectID(ref string) bool {
	return ObjectFormat(ref) != ""
}

func isFullHash(ref, resolved string) bool {
	return isFullObjectID(ref) && len(ref) == len(resolved)
}

// ObjectFormat returns the object format of a full commit hash, empty for anything else.
func ObjectFormat(sha string) string {
	switch {
	case !hexRefRegex.MatchString(sha):
		return ""
	case len(sha) == 40:
		return "sha1"
	case len(sha) == 64:
		return "sha256"
	}
	return ""
}

func removePartialClone(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove partial clone %s: %w", dir, err)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	output, err := runGit(ctx, dir, nil, args...)
	return strings.TrimSpace(string(output)), err
}

func RemoteConfig(token string, insecureSkipTLS bool) []string {
	var config []string
	if insecureSkipTLS {
		config = append(config, "http.sslVerify=false")
	}
	if token != "" {
		config = append(config, "http.extraHeader=", "http.extraHeader=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("token:"+token)))
	}
	return config
}

// ConfigEnv applies `-c` style entries through the environment, keeping secrets out of argv.
func ConfigEnv(config ...string) map[string]string {
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(config))}
	for i, entry := range config {
		key, value, _ := strings.Cut(entry, "=")
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = key
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = value
	}
	return env
}

func runGit(ctx context.Context, dir string, input *NewGitCloneExecutorInput, args ...string) ([]byte, error) {
	config := []string{"core.autocrlf=false", "core.eol=lf"} // keep action files LF
	if input != nil {
		config = append(config, RemoteConfig(input.Token, input.InsecureSkipTLS)...)
		if input.ClientCertFile != "" {
			config = append(config, "http.sslCert="+input.ClientCertFile, "http.sslKey="+input.ClientKeyFile)
		}
	}
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(LocalEnvVars, name)
	})
	for key, value := range ConfigEnv(config...) {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) } // lets git remove its lock files
	}
	cmd.WaitDelay = time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}
