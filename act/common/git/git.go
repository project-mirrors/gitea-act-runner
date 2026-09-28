// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/internal/pkg/lock"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/mattn/go-isatty"
	log "github.com/sirupsen/logrus"
)

var (
	codeCommitHTTPRegex = regexp.MustCompile(`^https?://git-codecommit\.(.+)\.amazonaws.com/v1/repos/(.+)$`)
	codeCommitSSHRegex  = regexp.MustCompile(`ssh://git-codecommit\.(.+)\.amazonaws.com/v1/repos/(.+)$`)
	githubHTTPRegex     = regexp.MustCompile(`^https?://.*github.com.*/(.+)/(.+?)(?:.git)?$`)
	githubSSHRegex      = regexp.MustCompile(`github.com[:/](.+)/(.+?)(?:.git)?$`)

	cloneLocks lock.Keyed[string] // key: clone target directory

	ErrShortRef = errors.New("short SHA references are not supported")
)

// AcquireCloneLock returns an unlock function after locking the per-directory mutex for dir.
// Only concurrent operations targeting the same directory are serialized; clones into different directories run in parallel.
// Callers reading files inside dir (e.g. tarring a checked-out action into a job container) must hold this lock too,
// otherwise a concurrent NewGitCloneExecutor on the same dir can mutate the worktree mid-read.
func AcquireCloneLock(dir string) func() {
	return cloneLocks.Lock(dir)
}

type Error struct {
	err    error
	commit string
}

func (e *Error) Error() string {
	return e.err.Error()
}

func (e *Error) Unwrap() error {
	return e.err
}

func (e *Error) Commit() string {
	return e.commit
}

// goGitMu serializes go-git repository access across the process. go-git is not safe for
// concurrent use of the same repository (even read access decodes packfiles into shared
// state), so parallel jobs inspecting the shared workdir repo race without this. The guarded
// operations are fast local reads; gitea runs one job per process, so the lock is effectively
// uncontended in production.
var goGitMu sync.Mutex

// FindGitRevision get the current git revision
func FindGitRevision(ctx context.Context, file string) (shortSha, sha string, err error) {
	goGitMu.Lock()
	defer goGitMu.Unlock()
	return findGitRevision(ctx, file)
}

func findGitRevision(ctx context.Context, file string) (shortSha, sha string, err error) {
	logger := common.Logger(ctx)

	gitDir, err := git.PlainOpenWithOptions(
		file,
		&git.PlainOpenOptions{
			DetectDotGit:          true,
			EnableDotGitCommonDir: true,
		},
	)
	if err != nil {
		logger.WithError(err).Error("path", file, "not located inside a git repository")
		return "", "", err
	}

	head, err := gitDir.Reference(plumbing.HEAD, true)
	if err != nil {
		return "", "", err
	}

	if head.Hash().IsZero() {
		return "", "", errors.New("HEAD sha1 could not be resolved")
	}

	hash := head.Hash().String()

	logger.Debugf("Found revision: %s", hash)
	return hash[:7], strings.TrimSpace(hash), nil
}

// FindGitRef get the current git ref
func FindGitRef(ctx context.Context, file string) (string, error) {
	goGitMu.Lock()
	defer goGitMu.Unlock()

	logger := common.Logger(ctx)

	logger.Debugf("Loading revision from git directory")
	_, ref, err := findGitRevision(ctx, file)
	if err != nil {
		return "", err
	}

	logger.Debugf("HEAD points to '%s'", ref)

	// Prefer the git library to iterate over the references and find a matching tag or branch.
	refTag := ""
	refBranch := ""
	repo, err := git.PlainOpenWithOptions(
		file,
		&git.PlainOpenOptions{
			DetectDotGit:          true,
			EnableDotGitCommonDir: true,
		},
	)
	if err != nil {
		return "", err
	}

	iter, err := repo.References()
	if err != nil {
		return "", err
	}

	// find the reference that matches the revision's has
	err = iter.ForEach(func(r *plumbing.Reference) error {
		/* tags and branches will have the same hash
		 * when a user checks out a tag, it is not mentioned explicitly
		 * in the go-git package, we must identify the revision
		 * then check if any tag matches that revision,
		 * if so then we checked out a tag
		 * else we look for branches and if matches,
		 * it means we checked out a branch
		 *
		 * If a branches matches first we must continue and check all tags (all references)
		 * in case we match with a tag later in the interation
		 */
		if r.Hash().String() == ref {
			if r.Name().IsTag() {
				refTag = r.Name().String()
			}
			if r.Name().IsBranch() {
				refBranch = r.Name().String()
			}
		}

		// we found what we where looking for
		if refTag != "" && refBranch != "" {
			return storer.ErrStop
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	// order matters here see above comment.
	if refTag != "" {
		return refTag, nil
	}
	if refBranch != "" {
		return refBranch, nil
	}

	return "", fmt.Errorf("failed to identify reference (tag/branch) for the checked-out revision '%s'", ref)
}

// FindGithubRepo get the repo
func FindGithubRepo(ctx context.Context, file, githubInstance string) (string, error) {
	goGitMu.Lock()
	defer goGitMu.Unlock()

	url, err := findGitRemoteURL(ctx, file, "origin")
	if err != nil {
		return "", err
	}
	_, slug := findGitSlug(url, githubInstance)
	return slug, nil
}

func findGitRemoteURL(_ context.Context, file, remoteName string) (string, error) {
	repo, err := git.PlainOpenWithOptions(
		file,
		&git.PlainOpenOptions{
			DetectDotGit:          true,
			EnableDotGitCommonDir: true,
		},
	)
	if err != nil {
		return "", err
	}

	remote, err := repo.Remote(remoteName)
	if err != nil {
		return "", err
	}

	if len(remote.Config().URLs) < 1 {
		return "", fmt.Errorf("remote '%s' exists but has no URL", remoteName)
	}

	return remote.Config().URLs[0], nil
}

func findGitSlug(url, githubInstance string) (string, string) {
	if matches := codeCommitHTTPRegex.FindStringSubmatch(url); matches != nil {
		return "CodeCommit", matches[2]
	} else if matches := codeCommitSSHRegex.FindStringSubmatch(url); matches != nil {
		return "CodeCommit", matches[2]
	} else if matches := githubHTTPRegex.FindStringSubmatch(url); matches != nil {
		return "GitHub", fmt.Sprintf("%s/%s", matches[1], matches[2])
	} else if matches := githubSSHRegex.FindStringSubmatch(url); matches != nil {
		return "GitHub", fmt.Sprintf("%s/%s", matches[1], matches[2])
	} else if githubInstance != "github.com" {
		gheHTTPRegex := regexp.MustCompile(fmt.Sprintf(`^https?://%s/(.+)/(.+?)(?:.git)?$`, githubInstance))
		gheSSHRegex := regexp.MustCompile(githubInstance + "[:/](.+)/(.+?)(?:.git)?$")
		if matches := gheHTTPRegex.FindStringSubmatch(url); matches != nil {
			return "GitHubEnterprise", fmt.Sprintf("%s/%s", matches[1], matches[2])
		} else if matches := gheSSHRegex.FindStringSubmatch(url); matches != nil {
			return "GitHubEnterprise", fmt.Sprintf("%s/%s", matches[1], matches[2])
		}
	}
	return "", url
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
	InsecureSkipTLS bool
}

// CloneIfRequired returns the repository and a boolean indicating whether an existing local clone was reused.
func CloneIfRequired(ctx context.Context, refName plumbing.ReferenceName, input NewGitCloneExecutorInput, logger log.FieldLogger) (*git.Repository, bool, error) {
	r, err := git.PlainOpen(input.Dir)
	if err == nil {
		// Verify the cached clone still points to the resolved URL before reusing it.
		remote, err := r.Remote("origin")
		if err == nil && len(remote.Config().URLs) > 0 && remote.Config().URLs[0] == input.URL {
			// Reuse existing clone
			return r, true, nil
		}

		switch {
		case err != nil:
			logger.Debugf("Removing cached clone at %s because origin cannot be read: %v", input.Dir, err)
		case len(remote.Config().URLs) == 0:
			logger.Debugf("Removing cached clone at %s because origin has no URL", input.Dir)
		default:
			logger.Debugf("Removing cached clone at %s because origin URL changed from %s to %s", input.Dir, remote.Config().URLs[0], input.URL)
		}
		if err := os.RemoveAll(input.Dir); err != nil {
			return nil, false, fmt.Errorf("remove cached clone %s: %w", input.Dir, err)
		}
	}

	var progressWriter io.Writer
	if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
		if entry, ok := logger.(*log.Entry); ok {
			progressWriter = entry.WriterLevel(log.DebugLevel)
		} else if lgr, ok := logger.(*log.Logger); ok {
			progressWriter = lgr.WriterLevel(log.DebugLevel)
		} else {
			log.Errorf("Unable to get writer from logger (type=%T)", logger)
			progressWriter = os.Stdout
		}
	}

	cloneOptions := git.CloneOptions{
		URL:      input.URL,
		Progress: progressWriter,

		InsecureSkipTLS: input.InsecureSkipTLS, // For Gitea
	}
	if input.Token != "" {
		cloneOptions.Auth = &http.BasicAuth{
			Username: "token",
			Password: input.Token,
		}
	}

	r, err = cloneAtDepth(ctx, input, cloneOptions, logger)
	if err != nil {
		logger.Errorf("Unable to clone %v %s: %v", input.URL, refName, err)
		return nil, false, err
	}

	if err = os.Chmod(input.Dir, 0o755); err != nil {
		return nil, false, err
	}

	return r, false, nil
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

		defer AcquireCloneLock(input.Dir)()

		refName := plumbing.ReferenceName("refs/heads/" + input.Ref)
		r, reused, err := CloneIfRequired(ctx, refName, input, logger)
		if err != nil {
			return err
		}

		isOfflineMode := input.OfflineMode

		fetchOptions := git.FetchOptions{
			RefSpecs:        []config.RefSpec{"refs/*:refs/*", "HEAD:refs/heads/HEAD", "refs/heads/*:refs/remotes/origin/*"},
			Force:           true,
			InsecureSkipTLS: input.InsecureSkipTLS,
		}
		if input.Token != "" {
			fetchOptions.Auth = &http.BasicAuth{
				Username: "token",
				Password: input.Token,
			}
		}

		// Action clones only ever need the tip commit, so keep a shallow cache cheap on update at depth 1 regardless of its original depth
		// Turning action_shallow_clone off does not convert an existing shallow cache; evict it for a full clone.
		if isShallow(r) {
			fetchOptions.Depth = 1
			if spec, ok := shallowFetchRefSpec(r, input.Ref); ok {
				fetchOptions.RefSpecs = []config.RefSpec{spec}
			}
		}

		// A just-cloned ref is as current as a fetch would make it, and a commit hash never moves.
		// TODO: revalidate a mutable ref with a conditional archive request instead, once every
		// supported Gitea sends an ETag for them: https://github.com/go-gitea/gitea/pull/39289
		_, present := refRevision(r, input.Ref)
		refresh := !isOfflineMode && (!present || (reused && !plumbing.IsHash(input.Ref)))

		if refresh {
			err = r.FetchContext(ctx, &fetchOptions)
			if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
				return err
			}
		}

		var hash *plumbing.Hash
		rev := plumbing.Revision(input.Ref)
		if hash, err = r.ResolveRevision(rev); err != nil {
			// ResolveRevision returns a nil hash on error, and a branch ref legitimately fails
			// here (no local refs/heads/<ref>); the duck-typing below resolves it.
			logger.Errorf("Unable to resolve %s: %v", input.Ref, err)
		} else if hash.String() != input.Ref && strings.HasPrefix(hash.String(), input.Ref) {
			return &Error{
				err:    ErrShortRef,
				commit: hash.String(),
			}
		}

		rev, _ = refRevision(r, input.Ref)

		if hash, err = r.ResolveRevision(rev); err != nil {
			logger.Errorf("Unable to resolve %s: %v", input.Ref, err)
			return err
		}

		var w *git.Worktree
		if w, err = r.Worktree(); err != nil {
			return err
		}

		reusedMsg := ""
		if isOfflineMode && reused {
			reusedMsg = " (reused in offline mode)"
		}

		logger.Debugf("Cloned %s to %s%s", input.URL, input.Dir, reusedMsg)

		if err = w.Checkout(&git.CheckoutOptions{
			Hash:  *hash,
			Force: true,
		}); err != nil {
			logger.Errorf("Unable to checkout %s: %v", *hash, err)
			return err
		}

		logger.Debugf("Checked out %s", input.Ref)
		return nil
	}
}

// cloneAtDepth falls back to a full clone when the shallow attempt fails.
func cloneAtDepth(ctx context.Context, input NewGitCloneExecutorInput, opts git.CloneOptions, logger log.FieldLogger) (*git.Repository, error) {
	if input.Depth > 0 {
		if plumbing.IsHash(input.Ref) {
			r, err := fetchPinnedSHA(ctx, input, opts)
			if err == nil {
				return r, nil
			}
			logger.Debugf("Shallow fetch of %s at %s failed: %v", input.URL, input.Ref, err)
			if err := removePartialClone(input.Dir); err != nil {
				return nil, err
			}
		} else {
			refNames := []plumbing.ReferenceName{plumbing.NewBranchReferenceName(input.Ref), plumbing.NewTagReferenceName(input.Ref)}
			if input.Ref == plumbing.HEAD.String() || strings.HasPrefix(input.Ref, "refs/") {
				refNames = []plumbing.ReferenceName{plumbing.ReferenceName(input.Ref)} // HEAD is the remote's default branch
			}
			for _, refName := range refNames {
				shallowOpts := opts
				shallowOpts.Depth = input.Depth
				shallowOpts.SingleBranch = true
				shallowOpts.ReferenceName = refName
				shallowOpts.Tags = git.NoTags

				r, err := git.PlainCloneContext(ctx, input.Dir, false, &shallowOpts)
				if err == nil {
					return r, nil
				}
				logger.Debugf("Shallow clone of %s as %s failed: %v", input.URL, refName, err)
				if err := removePartialClone(input.Dir); err != nil {
					return nil, err
				}
			}
		}
		logger.Debugf("Falling back to a full clone of %s for ref %q", input.URL, input.Ref)
	}

	return git.PlainCloneContext(ctx, input.Dir, false, &opts)
}

func removePartialClone(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove partial clone %s: %w", dir, err)
	}
	return nil
}

func fetchPinnedSHA(ctx context.Context, input NewGitCloneExecutorInput, opts git.CloneOptions) (*git.Repository, error) {
	r, err := git.PlainInit(input.Dir, false)
	if err != nil {
		return nil, err
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{input.URL}}); err != nil {
		return nil, err
	}
	if err := r.FetchContext(ctx, &git.FetchOptions{
		RefSpecs:        []config.RefSpec{pinnedRefSpec(input.Ref)},
		Depth:           input.Depth,
		Tags:            git.NoTags,
		Auth:            opts.Auth,
		Progress:        opts.Progress,
		InsecureSkipTLS: opts.InsecureSkipTLS,
	}); err != nil {
		return nil, err
	}
	return r, nil
}

// pinnedRefSpec keeps a hash-shaped name out of refs/heads and refs/tags.
func pinnedRefSpec(sha string) config.RefSpec {
	return config.RefSpec(fmt.Sprintf("+%s:refs/pinned/%s", sha, sha))
}

// refRevision picks the revision to check out and reports whether it resolves locally.
func refRevision(r *git.Repository, ref string) (plumbing.Revision, bool) {
	if plumbing.IsHash(ref) {
		// git ignores a ref named as 40 hex digits, so a full hash always denotes the commit itself.
		_, err := r.CommitObject(plumbing.NewHash(ref))
		return plumbing.Revision(ref), err == nil
	}
	if _, err := r.Tag(ref); err == nil {
		return plumbing.Revision(path.Join("refs", "tags", ref)), true
	}
	remoteRef := plumbing.ReferenceName(path.Join("refs", "remotes", "origin", ref))
	if _, err := r.Reference(remoteRef, false); err == nil {
		return plumbing.Revision(remoteRef), true
	}
	rev := plumbing.Revision(ref)
	_, err := r.ResolveRevision(rev)
	return rev, err == nil
}

// isShallow reports whether the local repository was cloned with a limited depth.
func isShallow(r *git.Repository) bool {
	shallows, err := r.Storer.Shallow()
	return err == nil && len(shallows) > 0
}

// shallowFetchRefSpec limits a shallow update to the requested ref, falling back to the broad refspec when it is not present locally.
func shallowFetchRefSpec(r *git.Repository, ref string) (config.RefSpec, bool) {
	tagRef := plumbing.NewTagReferenceName(ref)
	if _, err := r.Reference(tagRef, false); err == nil {
		return config.RefSpec(fmt.Sprintf("+%s:%s", tagRef, tagRef)), true
	}
	remoteRef := plumbing.NewRemoteReferenceName("origin", ref)
	if _, err := r.Reference(remoteRef, false); err == nil {
		branchRef := plumbing.NewBranchReferenceName(ref)
		return config.RefSpec(fmt.Sprintf("+%s:%s", branchRef, remoteRef)), true
	}
	if plumbing.IsHash(ref) {
		// The broad refspec carries only advertised tips, never a pinned commit.
		return pinnedRefSpec(ref), true
	}
	return "", false
}
