// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package checkout

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common/git"
	"gitea.com/gitea/runner/act/container"
	"gitea.com/gitea/runner/internal/pkg/action"

	"gitea.dev/actionslib/pkg/model"
	"github.com/kballard/go-shellquote"
)

var inputNames = []string{
	"repository", "ref", "token", "ssh-key", "ssh-known-hosts", "ssh-strict", "ssh-user", "persist-credentials", "path", "clean",
	"filter", "sparse-checkout", "sparse-checkout-cone-mode", "fetch-depth", "fetch-tags", "show-progress", "lfs", "submodules",
	"set-safe-directory",
}

const credentialsInclude = "includeIf.gitdir:**.path" // the form actions/checkout removes from a repository it checks out again

func Main(ctx context.Context, c *action.Context) (err error) {
	for name := range c.Inputs {
		if !slices.Contains(inputNames, name) {
			return fmt.Errorf("checkout: unsupported input %q", name)
		}
	}
	dst, err := workspaceDestination(c.Workspace, c.Inputs["path"])
	if err != nil {
		return err
	}
	depth, err := fetchDepth(c.Inputs["fetch-depth"])
	if err != nil {
		return err
	}

	ghc := c.Github
	repository := cmp.Or(c.Inputs["repository"], ghc.Repository)
	ref := c.Inputs["ref"]
	sshKey := strings.TrimSpace(c.Inputs["ssh-key"])
	isWorkflowRepo := strings.EqualFold(repository, ghc.Repository)
	local := isWorkflowRepo && slices.Contains([]string{"", shortBranch(ghc.Ref), ghc.Sha}, shortBranch(ref)) && !c.NoSkipCheckout && sshKey == ""
	if local && (!c.BindWorkdir || dst == c.Workspace) {
		if !c.BindWorkdir {
			if err := c.Container.CopyDir(dst, c.HostWorkdir+string(filepath.Separator)+".", c.UseGitIgnore, false)(ctx); err != nil {
				return err
			}
		}
		if output := c.Env["GITHUB_OUTPUT"]; output != "" {
			return c.Container.Copy(path.Dir(output), &container.FileEntry{Name: path.Base(output), Mode: 0o666, Body: fmt.Sprintf("ref=%s\ncommit=%s\n", ghc.Ref, ghc.Sha)})(ctx)
		}
		return nil
	}

	co := &checkout{
		url:       strings.TrimSuffix(ghc.ServerURL, "/") + "/" + repository,
		dst:       dst,
		ref:       ref,
		depth:     depth,
		fetchTags: boolInput(c.Inputs, "fetch-tags", false),
		clean:     boolInput(c.Inputs, "clean", true),
		lfs:       boolInput(c.Inputs, "lfs", false),
		progress:  boolInput(c.Inputs, "show-progress", true),
		cone:      boolInput(c.Inputs, "sparse-checkout-cone-mode", true),
		filter:    strings.TrimSpace(c.Inputs["filter"]),
		output:    c.Env["GITHUB_OUTPUT"],
	}
	for pattern := range strings.Lines(c.Inputs["sparse-checkout"]) {
		if pattern = strings.TrimSpace(pattern); pattern != "" {
			co.sparse = append(co.sparse, pattern)
		}
	}
	if len(co.sparse) > 0 {
		co.filter = cmp.Or(co.filter, "blob:none")
	}
	if submodules := strings.ToLower(strings.TrimSpace(c.Inputs["submodules"])); submodules == "true" || submodules == "recursive" {
		co.submodules = submodules
	}
	if sshKey != "" {
		if co.url, err = sshURL(ghc, repository, strings.TrimSpace(c.Inputs["ssh-user"])); err != nil {
			return err
		}
	}
	co.objectFormat = git.ObjectFormat(ref)
	switch {
	case co.objectFormat != "":
		co.sha, co.ref = ref, ""
	case ref == "" && isWorkflowRepo && strings.HasPrefix(ghc.Ref, "refs/"):
		co.sha, co.ref = ghc.Sha, ghc.Ref
	case ref == "" && isWorkflowRepo: // a merged pull request's ref is its merge commit
		co.sha = ghc.Sha
	case ref == "HEAD":
		co.ref = ""
	}
	if isWorkflowRepo {
		co.objectFormat = cmp.Or(co.objectFormat, git.ObjectFormat(ghc.Sha))
	}

	// named like actions/checkout's, which create-pull-request knows to hide, and gone with the job
	runnerTemp, _ := c.Container.GetRunnerContext(ctx)["temp"].(string)
	digest := sha256.Sum256([]byte(dst))
	credentials := fmt.Sprintf("git-credentials-%x.config", digest[:8])
	co.credentials = path.Join(runnerTemp, credentials)
	persist := boolInput(c.Inputs, "persist-credentials", true)
	config, remoteConfig := serverConfig(ghc.ServerURL, cmp.Or(c.Inputs["token"], ghc.Token), c.InsecureSkipTLS, co.submodules != "" && sshKey == "")
	if boolInput(c.Inputs, "set-safe-directory", true) {
		remoteConfig = append(remoteConfig, "safe.directory=*") // a bound workdir belongs to the host user
	}
	env := gitEnv(c, remoteConfig)
	env["GIT_LFS_SKIP_SMUDGE"] = strconv.FormatBool(!co.lfs || len(co.sparse) == 0) // lfs pull fetches the objects of a full checkout in one go
	if sshKey != "" {
		key := fmt.Sprintf("git-ssh-key-%x", digest[:8])
		files := []*container.FileEntry{{Name: key, Mode: 0o600, Body: sshKey + "\n"}}
		ssh := []string{"ssh", "-i", path.Join(runnerTemp, key)}
		if boolInput(c.Inputs, "ssh-strict", true) {
			ssh = append(ssh, "-o", "StrictHostKeyChecking=yes", "-o", "CheckHostIP=no")
		}
		if knownHosts := strings.TrimSpace(c.Inputs["ssh-known-hosts"]); knownHosts != "" {
			name := fmt.Sprintf("git-ssh-known-hosts-%x", digest[:8])
			files = append(files, &container.FileEntry{Name: name, Mode: 0o600, Body: knownHosts + "\n"})
			ssh = append(ssh, "-o", "UserKnownHostsFile=~/.ssh/known_hosts "+strconv.Quote(path.Join(runnerTemp, name)))
		}
		if !persist {
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				err = errors.Join(err, c.Container.Copy(runnerTemp, &container.FileEntry{Name: key, Mode: 0o600})(cleanupCtx))
			}()
		}
		if err := c.Container.Copy(runnerTemp, files...)(ctx); err != nil {
			return err
		}
		env["GIT_SSH_COMMAND"] = shellquote.Join(ssh...)
		config += "[core]\n\tsshCommand = " + strconv.Quote(env["GIT_SSH_COMMAND"]) + "\n"
	}
	if !persist {
		config = ""
	}

	if co.objectFormat == "" || co.sha == "" && !strings.HasPrefix(co.ref, "refs/") {
		advertised, err := lsRemote(ctx, c, env, co.url, co.ref)
		if err != nil {
			return fmt.Errorf("checkout: %s@%s: %w", repository, ref, err)
		}
		if co.resolve(advertised); co.ref == "" && co.sha == "" {
			return fmt.Errorf("checkout: %s has no ref %q", repository, ref)
		}
	}
	co.outputRef = co.ref
	if ref != co.sha {
		co.outputRef = cmp.Or(ref, co.ref) // as given, like actions/checkout
	}
	if err := c.Container.Copy(runnerTemp, &container.FileEntry{Name: credentials, Mode: 0o600, Body: config})(ctx); err != nil {
		return err
	}

	clone := func(commands [][]string) error {
		if err := run(ctx, c, env, commands); err != nil {
			return fmt.Errorf("checkout: clone %s@%s: %w", repository, cmp.Or(co.ref, co.sha), err)
		}
		return nil
	}
	commands, fetched := co.commands()
	if co.sha != "" && strings.HasPrefix(co.ref, "refs/tags/") {
		if err := clone(commands[:fetched]); err != nil {
			return err
		}
		tagged, err := output(ctx, c, env, "git", "-C", dst, "rev-parse", "--verify", "--quiet", co.ref+"^{commit}")
		if err != nil {
			return fmt.Errorf("checkout: %s: %w", co.ref, err)
		}
		if !strings.EqualFold(strings.TrimSpace(tagged), co.sha) {
			return fmt.Errorf("checkout: %s does not point to the expected commit %s, it may have been moved after the workflow was triggered", co.ref, co.sha)
		}
		commands = commands[fetched:]
	}
	return clone(commands)
}

func gitEnv(c *action.Context, config []string) map[string]string {
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	maps.Copy(env, c.Env)
	maps.DeleteFunc(env, func(name, _ string) bool { return slices.Contains(git.LocalEnvVars, name) })
	maps.Copy(env, git.ConfigEnv(append([]string{"protocol.version=2", "init.defaultBranch=main", "advice.detachedHead=false", "maintenance.auto=false"}, config...)...))
	return env
}

func run(ctx context.Context, c *action.Context, env map[string]string, commands [][]string) error {
	if _, host := c.Container.(*container.HostEnvironment); !host || runtime.GOOS != "windows" {
		script := []string{"set -e", `command -v git >/dev/null || { echo "builtin:checkout needs git on PATH" >&2; exit 127; }`}
		for _, command := range commands {
			script = append(script, shellquote.Join(command...))
		}
		return c.Container.Exec([]string{"sh", "-c", strings.Join(script, "\n")}, env, "", "")(ctx)
	}
	for _, command := range commands {
		if err := c.Container.Exec(command, env, "", "")(ctx); err != nil {
			return err
		}
	}
	return nil
}

// output runs where the job runs, so the job's network and git config apply as for the fetch.
func output(ctx context.Context, c *action.Context, env map[string]string, command ...string) (string, error) {
	var out strings.Builder
	stdout, stderr := c.Container.ReplaceLogWriter(&out, &out)
	defer c.Container.ReplaceLogWriter(stdout, stderr)
	if err := c.Container.Exec(command, env, "", "")(ctx); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

func lsRemote(ctx context.Context, c *action.Context, env map[string]string, url, ref string) (string, error) {
	patterns := []string{ref}
	switch {
	case ref == "":
		patterns = []string{"HEAD"}
	case !strings.HasPrefix(ref, "refs/"):
		patterns = []string{"refs/heads/" + ref, "refs/tags/" + ref}
	}
	return output(ctx, c, env, append([]string{"git", "ls-remote", "--symref", url}, patterns...)...)
}

// serverConfig scopes the token to the server, so no other host like a repository's LFS endpoint gets it.
func serverConfig(serverURL, token string, insecureSkipTLS, rewriteSSH bool) (file string, env []string) {
	scope := strings.TrimSuffix(serverURL, "/") + "/"
	file = fmt.Sprintf("[http %q]\n", scope)
	for _, entry := range git.RemoteConfig(token, insecureSkipTLS) {
		setting := strings.TrimPrefix(entry, "http.")
		key, value, _ := strings.Cut(setting, "=")
		file += "\t" + key + " = " + value + "\n"
		env = append(env, "http."+scope+"."+setting)
	}
	if server, err := url.Parse(serverURL); rewriteSSH && err == nil && server.Hostname() != "" { // submodules on the server fetch over HTTP with the token
		file += fmt.Sprintf("[url %q]\n\tinsteadOf = git@%s:\n", scope, server.Hostname())
		env = append(env, "url."+scope+".insteadOf=git@"+server.Hostname()+":")
	}
	return file, env
}

func sshURL(ghc *model.GithubContext, repository, user string) (string, error) {
	endpoint := &url.URL{Scheme: "ssh"}
	if advertised, _ := model.NestedMapLookup(ghc.Event, "repository", "ssh_url").(string); advertised != "" {
		if colon := strings.LastIndex(advertised, ":"); !strings.HasPrefix(advertised, "ssh://") && colon >= 0 {
			advertised = "ssh://" + advertised[:colon] + "/" + advertised[colon+1:] // scp-like git@host:owner/repo.git
		}
		var err error
		if endpoint, err = url.Parse(advertised); err != nil {
			return "", fmt.Errorf("checkout: parse the event's ssh_url: %w", err)
		}
	} else if server, err := url.Parse(ghc.ServerURL); err == nil {
		endpoint.Host = server.Hostname()
	}
	endpoint.User = url.User(cmp.Or(user, endpoint.User.Username(), "git"))
	endpoint.Path = "/" + repository + ".git"
	return endpoint.String(), nil
}

type checkout struct {
	url, dst, objectFormat, credentials, output, outputRef string
	ref, sha                                               string // ref is fully qualified or empty, sha pins it when known
	filter, submodules                                     string
	sparse                                                 []string
	depth                                                  int
	fetchTags, clean, lfs, progress, cone                  bool
}

const fullDepth = "2147483647" // git's INFINITE_DEPTH, also deepens a shallow dst

// commands checks out co.dst, the first `fetched` of them fetch so a fetched tag can be verified before the rest.
func (co *checkout) commands() (commands [][]string, fetched int) {
	repo := func(args ...string) []string { return append([]string{"git", "-C", co.dst}, args...) }
	commands = [][]string{
		{"git", "init", "--quiet", "--object-format=" + co.objectFormat, co.dst},
		repo("config", "remote.origin.url", co.url), // `remote add` fails on a repeated checkout
		repo("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"),
		repo("config", "--replace-all", "--fixed-value", credentialsInclude, co.credentials, co.credentials),
	}
	if co.lfs {
		commands = append(commands, repo("lfs", "install", "--local"))
	}

	depth := strconv.Itoa(co.depth)
	var refSpecs []string
	if co.depth == 0 {
		depth = fullDepth
		refSpecs = append(refSpecs, "+refs/heads/*:refs/remotes/origin/*")
	}
	fetch := []string{"fetch", "--force", "--no-tags", "--no-recurse-submodules", "--depth", depth}
	if co.progress {
		fetch = append(fetch, "--progress")
	} else {
		fetch = append(fetch, "--no-progress")
	}
	if co.filter != "" {
		fetch = append(fetch, "--filter="+co.filter)
	}
	if co.fetchTags || co.depth == 0 {
		refSpecs = append(refSpecs, "+refs/tags/*:refs/tags/*")
	}
	tag := strings.HasPrefix(co.ref, "refs/tags/") // fetched itself, which keeps an annotated tag's object
	pinned := co.sha != "" && co.ref != "" && !tag
	if co.ref != "" && !pinned {
		refSpecs = append(refSpecs, "+"+co.ref+":"+trackingRef(co.ref))
	}
	if co.sha != "" && !tag {
		refSpecs = append(refSpecs, co.sha)
	}
	commands = append(commands, repo(append(append(fetch, "origin"), refSpecs...)...))
	if pinned {
		commands = append(commands, repo("update-ref", trackingRef(co.ref), co.sha))
	}
	fetched = len(commands)

	if len(co.sparse) > 0 {
		mode := "--cone"
		if !co.cone {
			mode = "--no-cone"
		}
		commands = append(commands, repo("sparse-checkout", "init", mode), repo(append([]string{"sparse-checkout", "set", "--"}, co.sparse...)...))
	}
	if branch, ok := strings.CutPrefix(co.ref, "refs/heads/"); ok {
		commands = append(commands, repo("checkout", "--force", "--ignore-other-worktrees", "-B", branch, "--track", trackingRef(co.ref)))
	} else {
		commands = append(commands, repo("checkout", "--force", cmp.Or(co.sha, trackingRef(co.ref))))
	}
	if len(co.sparse) == 0 { // after the checkout, so a sparse dst of another repository needs nothing of it
		commands = append(commands, repo("sparse-checkout", "disable"), repo("config", "--local", "--unset-all", "extensions.worktreeConfig"))
	}
	if co.lfs && len(co.sparse) == 0 {
		commands = append(commands, repo("lfs", "pull", "origin"))
	}
	if co.clean {
		commands = append(commands, repo("clean", "-ffdx"))
	}
	if co.submodules != "" {
		commands = append(commands, co.submoduleCommands(repo)...)
	}
	if co.output != "" {
		commands = append(commands, repo("log", "-1", "--format=commit=%H%nref="+strings.ReplaceAll(co.outputRef, "%", "%%"), "--output="+co.output))
	}
	return commands, fetched
}

func (co *checkout) submoduleCommands(repo func(args ...string) []string) [][]string {
	var recursive []string
	if co.submodules == "recursive" {
		recursive = []string{"--recursive"}
	}
	foreach := func(commands ...[]string) []string {
		joined := make([]string, len(commands))
		for i, command := range commands {
			joined[i] = shellquote.Join(command...)
		}
		return repo(append(append([]string{"submodule", "foreach"}, recursive...), strings.Join(joined, " && "))...)
	}
	commands := [][]string{repo(append([]string{"submodule", "sync"}, recursive...)...)}
	if co.clean {
		commands = append(commands, foreach([]string{"git", "clean", "-ffdx"}))
	}
	update := append([]string{"submodule", "update", "--init", "--force"}, recursive...)
	if co.depth > 0 {
		update = append(update, "--depth="+strconv.Itoa(co.depth))
	}
	include := []string{"git", "config", "--replace-all", "--fixed-value", credentialsInclude, co.credentials, co.credentials}
	if co.lfs {
		return append(commands, repo(update...), foreach([]string{"git", "lfs", "install", "--local"}, []string{"git", "lfs", "pull", "origin"}, include))
	}
	return append(commands, repo(update...), foreach(include))
}

func (co *checkout) resolve(advertised string) {
	ref := co.ref
	if !strings.HasPrefix(ref, "refs/") {
		co.ref = ""
	}
	for line := range strings.Lines(advertised) {
		value, name, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if target, ok := strings.CutPrefix(value, "ref: "); ok {
			if name == "HEAD" {
				co.ref = target
			}
			continue
		}
		co.objectFormat = cmp.Or(co.objectFormat, git.ObjectFormat(value))
		switch name {
		case "HEAD":
			if co.ref == "" {
				co.sha = value
			}
		case "refs/heads/" + ref:
			co.ref = name
		case "refs/tags/" + ref:
			co.ref = cmp.Or(co.ref, name)
		}
	}
	co.objectFormat = cmp.Or(co.objectFormat, "sha1")
}

func trackingRef(ref string) string {
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return "refs/remotes/origin/" + branch
	}
	if pull, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		return "refs/remotes/pull/" + pull
	}
	return ref
}

func boolInput(inputs map[string]string, name string, fallback bool) bool {
	value := strings.TrimSpace(inputs[name])
	return strings.EqualFold(value, "true") || value == "" && fallback
}

func workspaceDestination(workspace, checkoutPath string) (string, error) {
	cleanPath := path.Clean(strings.TrimSpace(checkoutPath))
	if path.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return "", fmt.Errorf("checkout: path %q must be relative to the workspace", checkoutPath)
	}
	return path.Join(workspace, cleanPath), nil
}

func fetchDepth(input string) (int, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 1, nil
	}
	depth, err := strconv.Atoi(input)
	if err != nil || depth < 0 {
		return 0, fmt.Errorf("checkout: fetch-depth %q must be a non-negative integer", input)
	}
	return depth, nil
}

func shortBranch(ref string) string { // full tag refs stay exact
	return strings.TrimPrefix(ref, "refs/heads/")
}
