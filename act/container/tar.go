// Copyright 2026 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"context"
	"io"
	"path/filepath"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/filecollector"

	"github.com/go-git/go-billy/v5/helper/polyfill"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

func writeFilesTar(ctx context.Context, w io.Writer, uid, gid int, files ...*FileEntry) error {
	tw := tar.NewWriter(w)
	for _, file := range files {
		common.Logger(ctx).Debugf("Writing entry to tarball %s len:%d", file.Name, len(file.Body))
		if err := tw.WriteHeader(&tar.Header{
			Name: file.Name,
			Mode: file.Mode,
			Size: int64(len(file.Body)),
			Uid:  uid,
			Gid:  gid,
		}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(file.Body)); err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeDirTar prefixes entries with dstPath for extraction at root.
func writeDirTar(ctx context.Context, w io.Writer, dstPath, srcPath string, useGitIgnore, skipGitDir bool, uid, gid int) error {
	logger := common.Logger(ctx)
	tw := tar.NewWriter(w)
	srcPrefix := strings.TrimSuffix(filepath.Dir(srcPath), string(filepath.Separator)) + string(filepath.Separator)
	logger.Debugf("Stripping prefix:%s src:%s", srcPrefix, srcPath)

	var ignorer gitignore.Matcher
	if useGitIgnore {
		ps, err := gitignore.ReadPatterns(polyfill.New(osfs.New(srcPath)), nil)
		if err != nil {
			logger.Debugf("Error loading .gitignore: %v", err)
		}

		ignorer = gitignore.NewMatcher(ps)
	}

	if err := filepath.Walk(srcPath, (&filecollector.FileCollector{
		Ignorer:    ignorer,
		SrcPath:    srcPath,
		SrcPrefix:  srcPrefix,
		SkipGitDir: skipGitDir,
		Handler:    &filecollector.TarCollector{TarWriter: tw, UID: uid, GID: gid, DstDir: dstPath[1:]},
	}).CollectFiles(ctx, []string{})); err != nil {
		return err
	}
	return tw.Close()
}
