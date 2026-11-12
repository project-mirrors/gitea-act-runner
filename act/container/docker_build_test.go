// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd || freebsd))

package container

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDockerBuildLabelsImageWithItsTagAndReportsDaemonError(t *testing.T) {
	var labels string
	daemon := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("API-Version", "1.47")
		if strings.HasSuffix(request.URL.Path, "/build") {
			labels = request.URL.Query().Get("labels")
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `{"message":"build rejected"}`)
		}
	}))
	t.Cleanup(daemon.Close)
	t.Setenv("DOCKER_HOST", daemon.URL)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_AUTH_CONFIG", "")

	require.ErrorContains(t, NewDockerBuildExecutor(NewDockerBuildExecutorInput{
		ImageTag: "action:test", Dockerfile: "Dockerfile", BuildContext: strings.NewReader(""), Stdout: io.Discard,
	})(t.Context()), "build rejected")
	require.JSONEq(t, `{"com.gitea.runner.image":"action:test"}`, labels)
}

func TestCreateBuildContextFollowsDockerignoreAndSymlinkedRootButKeepsDockerfile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "build"), 0o755))
	for name, content := range map[string]string{
		".dockerignore": "*.log\nbuild\n!build/keep.txt\nDockerfile", "Dockerfile": "FROM scratch", "a.txt": "a",
		"a.log": "log", "build/drop.txt": "drop", "build/keep.txt": "keep",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0o755))

	contexts := []string{dir}
	if link := filepath.Join(t.TempDir(), "link"); os.Symlink(dir, link) == nil {
		contexts = append(contexts, link, link+string(filepath.Separator))
	}

	for _, contextDir := range contexts {
		buildContext, err := createBuildContext(t.Context(), contextDir, "Dockerfile")
		require.NoError(t, err)
		var names []string
		for reader := tar.NewReader(buildContext); ; {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			names = append(names, header.Name)
		}
		require.NoError(t, buildContext.Close())
		require.Equal(t, []string{".dockerignore", "a.txt", "build/keep.txt", "empty/", "Dockerfile"}, names, contextDir)
	}
}

func TestFilterBuildContextAppliesDockerignoreAndMaterializesHardlinksToExcludedFiles(t *testing.T) {
	const dockerignore = "*.log\nbuild\n!build/keep.txt\nActionfile\n.dockerignore\n"
	for _, prefix := range []string{"", "./"} {
		var archive bytes.Buffer
		writer := tar.NewWriter(&archive)
		for _, entry := range []struct {
			name, body, link string
			typeflag         byte
		}{
			{"Actionfile", "FROM scratch", "", tar.TypeReg},
			{"output.log", "log", "", tar.TypeReg},
			{"build/", "", "", tar.TypeDir},
			{"build/drop.txt", "drop", "", tar.TypeReg},
			{"build/keep.txt", "keep", "", tar.TypeReg},
			{"entrypoint", "", prefix + "output.log", tar.TypeLink},
			{"other-entrypoint", "", prefix + "output.log", tar.TypeLink},
			{".dockerignore", dockerignore, "", tar.TypeReg},
		} {
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: prefix + entry.name, Linkname: entry.link, Typeflag: entry.typeflag, Size: int64(len(entry.body)), Mode: 0o644}))
			_, err := io.WriteString(writer, entry.body)
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())

		unfiltered, err := filterBuildContext(bytes.NewReader(archive.Bytes()), nil, "Actionfile")
		require.NoError(t, err)
		passedThrough, err := io.ReadAll(unfiltered)
		require.NoError(t, err)
		require.Equal(t, archive.Bytes(), passedThrough)

		filtered, err := filterBuildContext(bytes.NewReader(archive.Bytes()), []byte(dockerignore), "Actionfile")
		require.NoError(t, err)
		var entries []string
		for reader := tar.NewReader(filtered); ; {
			header, err := reader.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			body, err := io.ReadAll(reader)
			require.NoError(t, err)
			entries = append(entries, fmt.Sprintf("%s %c %s %s", strings.TrimPrefix(header.Name, prefix), header.Typeflag, strings.TrimPrefix(header.Linkname, prefix), body))
		}
		require.Equal(t, []string{
			"Actionfile 0  FROM scratch",
			"build/keep.txt 0  keep",
			"entrypoint 0  log",
			"other-entrypoint 1 entrypoint ",
			".dockerignore 0  " + dockerignore,
		}, entries, prefix)
	}
}
