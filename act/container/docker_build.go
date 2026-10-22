// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2020 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gitea.com/gitea/runner/act/common"

	"github.com/moby/moby/client"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

// NewDockerBuildExecutor function to create a run executor for the container
func NewDockerBuildExecutor(input NewDockerBuildExecutorInput) common.Executor {
	return func(ctx context.Context) error {
		logger := common.Logger(ctx)
		if input.Platform != "" {
			logger.Infof("docker build -t %s --platform %s %s", input.ImageTag, input.Platform, input.ContextDir)
		} else {
			logger.Infof("docker build -t %s %s", input.ImageTag, input.ContextDir)
		}
		if common.Dryrun(ctx) {
			return nil
		}

		cli, err := GetDockerClient(ctx)
		if err != nil {
			return err
		}
		defer cli.Close()

		logger.Debugf("Building image from '%v'", input.ContextDir)

		options := client.ImageBuildOptions{
			Tags:        []string{input.ImageTag},
			Remove:      true,
			AuthConfigs: LoadDockerAuthConfigs(ctx),
			Dockerfile:  input.Dockerfile,
			BuildArgs:   input.BuildArgs,
		}
		platform, err := parsePlatform(input.Platform)
		if err != nil {
			return err
		}
		if platform != nil {
			options.Platforms = []specs.Platform{*platform}
		}
		var buildContext io.ReadCloser
		if input.BuildContext != nil {
			buildContext = io.NopCloser(input.BuildContext)
		} else {
			buildContext, err = createBuildContext(ctx, input.ContextDir, input.Dockerfile)
		}
		if err != nil {
			return err
		}

		defer buildContext.Close()

		logger.Debugf("Creating image from context dir '%s' with tag '%s' and platform '%s'", input.ContextDir, input.ImageTag, input.Platform)
		resp, err := cli.ImageBuild(ctx, buildContext, options)

		return logDockerResponse(logger, resp.Body, err != nil)
	}
}

func createBuildContext(ctx context.Context, contextDir, relDockerfile string) (io.ReadCloser, error) {
	common.Logger(ctx).Debugf("Creating archive for build context dir '%s' with relative dockerfile '%s'", contextDir, relDockerfile)

	// And canonicalize dockerfile name to a platform-independent one
	relDockerfile = filepath.ToSlash(relDockerfile)

	f, err := os.Open(filepath.Join(contextDir, ".dockerignore"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	defer f.Close()

	var excludes []string
	if err == nil {
		excludes, err = ignorefile.ReadAll(f)
		if err != nil {
			return nil, err
		}
	}

	// If .dockerignore mentions .dockerignore or the Dockerfile
	// then make sure we send both files over to the daemon
	// because Dockerfile is, obviously, needed no matter what, and
	// .dockerignore is needed to know if either one needs to be
	// removed. The daemon will remove them for us, if needed, after it
	// parses the Dockerfile. Ignore errors here, as they will have been
	// caught by validateContextDirectory above.
	includes := []string{"."}
	keepThem1, _ := patternmatcher.Matches(".dockerignore", excludes)
	keepThem2, _ := patternmatcher.Matches(relDockerfile, excludes)
	if keepThem1 || keepThem2 {
		includes = append(includes, ".dockerignore", relDockerfile)
	}

	pm, err := patternmatcher.New(excludes)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(writeBuildContext(ctx, writer, contextDir, includes, pm)) }()
	return reader, nil
}

func writeBuildContext(ctx context.Context, w io.Writer, contextDir string, includes []string, pm *patternmatcher.PatternMatcher) error {
	tw := tar.NewWriter(w)
	seen := map[string]bool{}
	hardlinks := map[uint64]string{}
	buffer := make([]byte, 32*1024)
	for _, include := range includes {
		include = filepath.FromSlash(include)
		var parents []parentMatch
		root := strings.TrimSuffix(contextDir, string(filepath.Separator)) + string(filepath.Separator) + include // unlike filepath.Join, follows a symlinked context dir as docker build does
		err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
			if err != nil {
				return leaveOut(ctx, file, err)
			}
			rel, err := filepath.Rel(contextDir, file)
			if err != nil || rel == "." {
				return nil
			}
			if rel != include {
				for len(parents) > 0 && !strings.HasPrefix(rel, parents[len(parents)-1].dir+string(filepath.Separator)) {
					parents = parents[:len(parents)-1]
				}
				var parentInfo patternmatcher.MatchInfo
				if len(parents) > 0 {
					parentInfo = parents[len(parents)-1].info
				}
				skip, info, err := pm.MatchesUsingParentResults(rel, parentInfo)
				if err != nil {
					return err
				}
				if entry.IsDir() {
					parents = append(parents, parentMatch{rel, info})
				}
				if skip {
					if entry.IsDir() && !hasExceptionWithin(pm, rel) {
						return filepath.SkipDir
					}
					return nil
				}
			}
			if seen[rel] {
				return nil
			}
			seen[rel] = true
			return addBuildContextEntry(ctx, tw, file, rel, entry, hardlinks, buffer)
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

type parentMatch struct {
	dir  string
	info patternmatcher.MatchInfo
}

func hasExceptionWithin(pm *patternmatcher.PatternMatcher, dir string) bool {
	dirPrefix := dir + string(filepath.Separator)
	for _, pattern := range pm.Patterns() {
		if pattern.Exclusion() && strings.HasPrefix(pattern.String()+string(filepath.Separator), dirPrefix) {
			return true
		}
	}
	return false
}

func leaveOut(ctx context.Context, file string, err error) error {
	common.Logger(ctx).Warnf("Leaving %s out of the build context: %v", file, err)
	return nil
}

func addBuildContextEntry(ctx context.Context, tw *tar.Writer, file, rel string, entry fs.DirEntry, hardlinks map[uint64]string, buffer []byte) error {
	info, err := entry.Info()
	if err != nil {
		return leaveOut(ctx, file, err)
	}
	if info.Mode()&fs.ModeSocket != 0 {
		return nil // tar has no socket type
	}
	var link string
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(file); err != nil {
			return leaveOut(ctx, file, err)
		}
	}
	header, err := tar.FileInfoHeader(noNameLookups{info}, link)
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(rel)
	inode, linked := hardlinkInode(info)
	if target, ok := hardlinks[inode]; linked && ok {
		header.Typeflag, header.Linkname, header.Size = tar.TypeLink, target, 0
	}
	var content *os.File
	if header.Typeflag == tar.TypeReg && header.Size > 0 {
		if content, err = os.Open(file); err != nil {
			return leaveOut(ctx, file, err)
		}
		defer content.Close()
	}
	if linked && header.Typeflag != tar.TypeLink {
		hardlinks[inode] = header.Name
	}
	if info.IsDir() {
		header.Name += "/"
	}
	header.Format = tar.FormatPAX
	header.ModTime = header.ModTime.Truncate(time.Second)
	header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
	if runtime.GOOS == "windows" {
		header.Mode = header.Mode&0o755 | 0o111 // Windows has no exec bit, so mark everything executable like docker build
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if content != nil {
		_, err = io.CopyBuffer(tw, struct{ io.Reader }{content}, buffer) // hides os.File's WriteTo, which would allocate a buffer per file
	}
	return err
}

type noNameLookups struct{ fs.FileInfo } // leaves Uname and Gname empty like docker build, so packing never waits on NSS

func (noNameLookups) Uname() (string, error) { return "", nil }

func (noNameLookups) Gname() (string, error) { return "", nil }
