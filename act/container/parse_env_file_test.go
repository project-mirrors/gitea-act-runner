// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/unicode"
)

func newTestHostEnv(t *testing.T) (*HostEnvironment, string) {
	t.Helper()
	e := &HostEnvironment{Path: t.TempDir()}
	return e, filepath.Join(e.Path, "envfile")
}

func TestParseEnvFile(t *testing.T) {
	largeValue := strings.Repeat("x", 2*1024*1024)
	encodeUTF16 := func(endianness unicode.Endianness) string {
		content, err := unicode.UTF16(endianness, unicode.UseBOM).NewEncoder().Bytes([]byte("FOO=bar\r\nMULTI<<EOF\r\nline1\r\nEOF\r\n"))
		require.NoError(t, err)
		return string(content)
	}
	for _, test := range []struct {
		name, content string
		want          map[string]string
	}{
		{"single line", "FOO=bar\nBAZ=qux\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"blank lines", "\nFOO=bar\n\n   \nBAZ=qux\n\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"multiline", "FOO<<EOF\nline1\nline2\nEOF\n", map[string]string{"FOO": "line1\nline2"}},
		{"multiline blank lines", "FOO<<EOF\nline1\n\nline2\nEOF\n", map[string]string{"FOO": "line1\n\nline2"}},
		{"multiline leading blank line", "FOO<<EOF\n\nline2\nEOF\n", map[string]string{"FOO": "\nline2"}},
		{"large value within limit", "FOO<<EOF\n" + largeValue + "\nEOF\n", map[string]string{"FOO": largeValue}},
		{"UTF8 BOM", "\xef\xbb\xbfFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"UTF16 little endian", encodeUTF16(unicode.LittleEndian), map[string]string{"FOO": "bar", "MULTI": "line1"}},
		{"UTF16 big endian", encodeUTF16(unicode.BigEndian), map[string]string{"FOO": "bar", "MULTI": "line1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, envPath := newTestHostEnv(t)
			require.NoError(t, os.WriteFile(envPath, []byte(test.content), 0o600))
			env := map[string]string{}
			require.NoError(t, parseEnvFile(e, envPath, &env)(t.Context()))
			assert.Equal(t, test.want, env)
		})
	}
}

func TestParseEnvFileLineExceedsBufferReportsScannerError(t *testing.T) {
	e, envPath := newTestHostEnv(t)
	require.NoError(t, os.WriteFile(envPath, []byte("FOO<<EOF\n"+strings.Repeat("x", 17*1024*1024)+"\nEOF\n"), 0o600))

	env := map[string]string{}
	err := parseEnvFile(e, envPath, &env)(t.Context())
	require.ErrorIs(t, err, bufio.ErrTooLong)
	assert.Contains(t, err.Error(), "reading env file")
}

func TestParseEnvFileMissingDelimiter(t *testing.T) {
	e, envPath := newTestHostEnv(t)
	require.NoError(t, os.WriteFile(envPath, []byte("FOO<<EOF\nline1\nline2\n"), 0o600))

	env := map[string]string{}
	err := parseEnvFile(e, envPath, &env)(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delimiter")
}
