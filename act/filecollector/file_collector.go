// Copyright 2024 The Gitea Authors. All rights reserved.
// Copyright 2024 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package filecollector

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/git"

	"golang.org/x/text/unicode/norm"
)

type Handler interface {
	WriteFile(path string, fi fs.FileInfo, linkName string, f io.Reader) error
}

type TarCollector struct {
	TarWriter *tar.Writer
	UID       int
	GID       int
	DstDir    string
}

func (tc TarCollector) WriteFile(fpath string, fi fs.FileInfo, linkName string, f io.Reader) error {
	// create a new dir/file header
	header, err := tar.FileInfoHeader(fi, linkName)
	if err != nil {
		return err
	}

	// update the name to correctly reflect the desired destination when untaring
	header.Name = path.Join(tc.DstDir, fpath)
	header.Mode = int64(fi.Mode())
	header.ModTime = fi.ModTime()
	header.Uid = tc.UID
	header.Gid = tc.GID

	// write the header
	if err := tc.TarWriter.WriteHeader(header); err != nil {
		return err
	}

	// this is a symlink no reader provided
	if f == nil {
		return nil
	}

	// copy file data into tar writer
	if _, err := io.Copy(tc.TarWriter, f); err != nil {
		return err
	}
	return nil
}

type CopyCollector struct {
	DstDir string
}

func (cc *CopyCollector) WriteFile(fpath string, fi fs.FileInfo, linkName string, f io.Reader) error {
	fdestpath := filepath.Join(cc.DstDir, fpath)
	if err := os.MkdirAll(filepath.Dir(fdestpath), 0o777); err != nil {
		return err
	}
	// Remove any existing destination so we can overwrite read-only files
	// (e.g. git pack files at mode 0444 trip EACCES on macOS and "Access is
	// denied" on Windows when reopened with O_WRONLY) and so os.Symlink does
	// not fail with EEXIST. os.Remove clears the Windows read-only attribute
	// internally; on Unix unlink only needs write permission on the parent.
	_ = os.Remove(fdestpath)
	if f == nil {
		return os.Symlink(linkName, fdestpath)
	}
	df, err := os.OpenFile(fdestpath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := io.Copy(df, f); err != nil {
		return err
	}
	return nil
}

type FileCollector struct {
	SrcPath      string
	SrcPrefix    string
	SkipGitDir   bool
	UseGitIgnore bool
	Handler      Handler
}

// foldPath compares git's spelling of a path with the filesystem's, which may differ in case or Unicode normalization.
func foldPath(name string) string {
	return strings.ToLower(norm.NFC.String(name))
}

func (fc *FileCollector) gitFiles(ctx context.Context) map[string]bool {
	if !fc.UseGitIgnore {
		return nil
	}
	files, err := git.ListFiles(ctx, fc.SrcPath)
	if err != nil {
		common.Logger(ctx).Debugf("Copying %s without applying .gitignore: %v", fc.SrcPath, err)
		return nil
	}
	keep := map[string]bool{".": true}
	for _, file := range files {
		for file = foldPath(file); !keep[file]; file = path.Dir(file) {
			keep[file] = true
		}
	}
	return keep
}

func (fc *FileCollector) CollectFiles(ctx context.Context) filepath.WalkFunc {
	keep := fc.gitFiles(ctx)
	return func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errors.New("copy cancelled")
		}
		if fc.SkipGitDir && fi.Name() == ".git" {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if keep != nil {
			rel, _ := filepath.Rel(fc.SrcPath, file)
			if rel = filepath.ToSlash(rel); !keep[foldPath(rel)] && !strings.Contains("/"+rel+"/", "/.git/") {
				if fi.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		path := filepath.ToSlash(strings.TrimPrefix(file, fc.SrcPrefix))

		if fi.Mode()&os.ModeSymlink == os.ModeSymlink {
			linkName, err := os.Readlink(file)
			if err != nil {
				return fmt.Errorf("unable to readlink '%s': %w", file, err)
			}
			return fc.Handler.WriteFile(path, fi, linkName, nil)
		} else if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		defer context.AfterFunc(ctx, func() { _ = f.Close() })()

		return fc.Handler.WriteFile(path, fi, "", f)
	}
}
