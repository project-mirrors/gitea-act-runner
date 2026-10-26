// Copyright 2022 The Gitea Authors. All rights reserved.
// Copyright 2022 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"gitea.com/gitea/runner/act/common"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// parseEnvFile reads a GITHUB_ENV style file, splitting lines like GitHub's runner on Windows or elsewhere.
func parseEnvFile(e Container, srcPath string, env *map[string]string, windows bool) common.Executor {
	localEnv := *env
	return func(ctx context.Context) error {
		envTar, err := e.GetContainerArchive(ctx, srcPath)
		if err != nil {
			return nil
		}
		defer envTar.Close()
		reader := tar.NewReader(envTar)
		_, err = reader.Next()
		if err != nil && err != io.EOF {
			return err
		}
		// Decode by BOM: Windows PowerShell 5.1 redirection writes UTF-16, and some
		// tools emit a UTF-8 BOM. Without a BOM the file is read as UTF-8, as before.
		decoded := transform.NewReader(reader, unicode.BOMOverride(unicode.UTF8.NewDecoder()))

		s := bufio.NewScanner(decoded)
		// Default 64 KiB max token size is too small for realistic env-file lines; allow up to 16 MiB.
		s.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		scanned := 0
		s.Split(func(data []byte, atEOF bool) (int, []byte, error) {
			if index := bytes.IndexByte(data[scanned:], '\n'); index >= 0 {
				advance := scanned + index + 1
				scanned = 0
				return advance, data[:advance], nil
			}
			scanned = len(data)
			if atEOF && len(data) > 0 {
				scanned = 0
				return len(data), data, nil
			}
			return 0, nil, nil
		})
		for s.Scan() {
			line := trimEnvLineEnding(s.Text(), windows)
			// GitHub's runner ignores blank lines
			if strings.TrimSpace(line) == "" {
				continue
			}
			singleLineEnv := strings.Index(line, "=")
			multiLineEnv := strings.Index(line, "<<")
			switch {
			case singleLineEnv != -1 && (multiLineEnv == -1 || singleLineEnv < multiLineEnv):
				localEnv[line[:singleLineEnv]] = line[singleLineEnv+1:]
			case multiLineEnv != -1:
				var multiLineEnvContent []string
				multiLineEnvDelimiter := line[multiLineEnv+2:]
				if multiLineEnv == 0 || multiLineEnvDelimiter == "" {
					return fmt.Errorf("invalid format '%v', name and delimiter must not be empty", line)
				}
				delimiterFound := false
				for s.Scan() {
					content := s.Text()
					if trimEnvLineEnding(content, windows) == multiLineEnvDelimiter {
						delimiterFound = true
						break
					}
					multiLineEnvContent = append(multiLineEnvContent, content)
				}
				if err := s.Err(); err != nil {
					return fmt.Errorf("reading env file: %w", err)
				}
				if !delimiterFound {
					return fmt.Errorf("invalid format delimiter '%v' not found before end of file", multiLineEnvDelimiter)
				}
				localEnv[line[:multiLineEnv]] = trimEnvLineEnding(strings.Join(multiLineEnvContent, ""), windows)
			default:
				return fmt.Errorf("invalid format '%v', expected a line with '=' or '<<'", line)
			}
		}
		if err := s.Err(); err != nil {
			return fmt.Errorf("reading env file: %w", err)
		}
		return nil
	}
}

func trimEnvLineEnding(line string, windows bool) string {
	if windows && strings.HasSuffix(line, "\r\n") {
		return line[:len(line)-2]
	}
	return strings.TrimSuffix(line, "\n")
}
