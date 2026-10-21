// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"gitea.com/gitea/runner/act/common"

	"gitea.dev/actionslib/pkg/model"
)

// Actions bundle the @actions toolkit into their own JavaScript, and two of its lines keep it
// from working against Gitea. Both are edited out of the bundle the runner downloaded.
//
// isGhes() takes any host that is not github.com, *.ghe.com or *.localhost for GitHub
// Enterprise. @actions/cache then forces the v1 API, and @actions/artifact refuses outright,
// which is why the stock upload-artifact aborts here. The edit empties the last of the three
// hostname tests, so `endsWith('.LOCALHOST')` becomes `endsWith(”)`, which every hostname
// satisfies: one string literal, no call sites to resolve, and the same answer the toolkit's own
// proposed ACTIONS_VENDOR switch would give. Gitea already makes this edit by hand in its fork
// of upload-artifact.
//
// getCacheServiceURL() then resolves the cache service from ACTIONS_RESULTS_URL alone, where v1
// reads ACTIONS_CACHE_URL first. Both reads there are given the same preference, so a patched cache
// client reaches the cache server by its own address rather than through the results origin.
//
// Either of these landing upstream makes this file deletable:
//
//	https://github.com/actions/toolkit/pull/2123   — an ACTIONS_VENDOR switch, naming Gitea
//	https://github.com/actions/toolkit/issues/2439 — treat ACTIONS_RESULTS_URL as the signal
const (
	CacheServiceV2Env = "ACTIONS_CACHE_SERVICE_V2"
	cacheURLEnv       = "ACTIONS_CACHE_URL"
	resultsURLEnv     = "ACTIONS_RESULTS_URL"

	// localhostHost is the suffix isGhes accepts, and gheHost the one it tests just before.
	localhostHost = ".LOCALHOST"
	gheHost       = ".GHE.COM"

	// artifactRefusal is the only thing the gate guards in @actions/artifact, which is what makes
	// such a bundle safe to open. A bundle carrying neither toolkit uses isGhes for something this
	// runner has not looked at, and is left alone.
	artifactRefusal = "GHESNotSupportedError"

	maxBundleSize = 64 << 20
)

var (
	// localhostTest matches the third hostname test of isGhes, in any quoting. The match is case
	// sensitive on purpose, and that is load-bearing: isGhes uppercases the hostname before
	// testing it, while undici, bundled into all of these actions, tests a lowercase ".localhost"
	// in isURLPotentiallyTrustworthy. Opening that one would tell its HTTP client that every URL
	// is trustworthy. Uppercase, the literal occurs nowhere but this test, across 118 bundles
	// covering every major version of sixteen actions. Anchoring on the .GHE.COM test, 5 to 35
	// bytes before it, keeps any other library's uppercase test out.
	localhostTest = regexp.MustCompile(`endsWith\s*\(\s*` + quoted(regexp.QuoteMeta(gheHost)) + `\s*\)(?s:.{0,128}?)` +
		`endsWith\s*\(\s*` + quoted(regexp.QuoteMeta(localhostHost)) + `\s*\)`)

	// serviceURLBranches matches both branches of getCacheServiceURL at once: the v1 branch reads
	// the cache URL and falls back to the results URL, the v2 branch just below reads the results
	// URL alone. That `||` pairing is the only place the two variables are read together, so
	// matching them as one expression is what keeps the edit inside this function rather than
	// anywhere they happen to sit near each other. The branches are 21 bytes apart minified and
	// 63 not, across every bundle measured.
	serviceURLBranches = regexp.MustCompile(`(` + envRead(cacheURLEnv) + `\s*\|\|\s*)(` +
		envRead(resultsURLEnv) + `)((?s).{0,256}?)(` + envRead(resultsURLEnv) + `)`)

	// cacheURLFirst gives both reads the preference the v1 branch already had.
	cacheURLFirst = []byte(`${1}(process.env.` + cacheURLEnv + `||${2})${3}(process.env.` + cacheURLEnv + `||${4})`)
)

func envRead(name string) string {
	return `process\s*\.\s*env\s*(?:\.\s*` + name + `\b|\[\s*` + quoted(name) + `\s*\])`
}

// quoted matches a string literal in any of the three quote characters. RE2 has no
// backreferences, so the pairs are spelled out.
func quoted(pattern string) string {
	return "(?:'" + pattern + "'|\"" + pattern + "\"|`" + pattern + "`)"
}

// actionScriptPaths returns a node action's entrypoints and the chunks bundled beside them. Only
// remote actions get here: a local one lives in the user's checkout, which the runner does not
// rewrite. Scripts may leave dir for the rest of the checkout, as setup-gradle's ../dist does.
func actionScriptPaths(checkout, dir string, action *model.Action) []string {
	if action == nil || !action.Runs.Using.IsNode() {
		return nil
	}
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return nil
	}
	var paths []string
	for _, script := range []string{action.Runs.Pre, action.Runs.Main, action.Runs.Post} {
		if script == "" {
			continue
		}
		path, err := filepath.EvalSymlinks(filepath.Join(dir, script))
		if err != nil {
			continue
		}
		// `runs` is the action's own yaml, and a key resolving outside its checkout is not ours.
		bundleDir := filepath.Dir(path)
		if rel, err := filepath.Rel(root, bundleDir); err != nil || !filepath.IsLocal(rel) {
			continue
		}
		paths = append(paths, path)
		entries, _ := os.ReadDir(bundleDir)
		for _, entry := range entries {
			if entry.Type().IsRegular() && slices.Contains([]string{".js", ".cjs", ".mjs"}, filepath.Ext(entry.Name())) {
				paths = append(paths, filepath.Join(bundleDir, entry.Name()))
			}
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// patchActions returns a restore: the tree is shared, so a job with patching off gets no edits.
func patchActions(ctx context.Context, scripts []string) func() {
	restore := map[string][]byte{}
	for _, script := range scripts {
		original, err := patchBundle(script)
		if original != nil {
			restore[script] = original // also when the write failed part way through
		}
		if err != nil {
			common.Logger(ctx).Warnf("actions toolkit: %s left unpatched: %v", script, err)
		}
	}
	return func() {
		for script, original := range restore {
			if err := os.WriteFile(script, original, 0o644); err != nil { //nolint:gosec // as the checkout wrote it
				common.Logger(ctx).Warnf("actions toolkit: %s left patched in the shared copy: %v", script, err)
			}
		}
	}
}

// patchBundle returns the bytes it replaced, or nil when it left the bundle alone.
func patchBundle(script string) ([]byte, error) {
	info, err := os.Stat(script)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBundleSize {
		return nil, nil
	}
	data, err := os.ReadFile(script)
	if err != nil {
		return nil, err
	}
	patched, ok := patchedBundle(data)
	if !ok {
		return nil, nil
	}
	return data, os.WriteFile(script, patched, info.Mode().Perm())
}

// patchedBundle opens the GHES gate, and where the cache toolkit is present, points the cache
// service at the cache server. A bundle this runner cannot account for comes back untouched.
func patchedBundle(data []byte) ([]byte, bool) {
	// Literals before regex: most bundles carry neither toolkit and stop here. The artifact gate
	// guards a refusal with no URL to move, so it opens alone; the cache gate opens only with its
	// service URL, since a bundle whose getter this cannot find is better left on v1.
	artifact := bytes.Contains(data, []byte(artifactRefusal))
	cache := bytes.Contains(data, []byte(CacheServiceV2Env)) && serviceURLBranches.Match(data)
	if !artifact && !cache {
		return data, false
	}
	if !localhostTest.Match(data) {
		return data, false
	}

	opened := localhostTest.ReplaceAllFunc(data, func(test []byte) []byte {
		// Drop the hostname from the test rather than rewriting the call, so the bundle's own
		// quoting survives and the result stays valid even inside a string literal.
		before, after, _ := bytes.CutLast(test, []byte(localhostHost))
		return slices.Concat(before, after)
	})
	if cache {
		opened = serviceURLBranches.ReplaceAll(opened, cacheURLFirst)
	}
	return opened, true
}
