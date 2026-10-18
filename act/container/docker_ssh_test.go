// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"io"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSHArgs(t *testing.T) {
	args, err := sshArgs("ssh://me@host:2222/run/user/1000/my docker.sock")
	require.NoError(t, err)
	assert.Equal(t, []string{"-l", "me", "-p", "2222", "-o", "ConnectTimeout=30", "-T", "--", "host", "docker '--host=unix:///run/user/1000/my docker.sock' system dial-stdio"}, args)

	args, err = sshArgs("ssh://[::1]")
	require.NoError(t, err)
	assert.Equal(t, []string{"-o", "ConnectTimeout=30", "-T", "--", "::1", "docker system dial-stdio"}, args)

	for _, invalid := range []string{"ssh://me:secret@host", "ssh://host?x=1", "ssh://host#x", "ssh://"} {
		_, err := sshArgs(invalid)
		assert.Error(t, err, invalid)
	}
}

func TestDialCommandStreamsStdioAndReportsExit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}
	conn, err := dialCommand("sh", "-c", "cat")
	require.NoError(t, err)
	_, err = conn.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, conn.(interface{ CloseWrite() error }).CloseWrite())
	echoed, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(echoed))
	require.NoError(t, conn.Close())

	conn, err = dialCommand("sh", "-c", "exec 3<&0; cat <&3 >/dev/null & echo 'no route to host' >&2; exit 255")
	require.NoError(t, err)
	_, err = io.ReadAll(conn)
	require.ErrorContains(t, err, "no route to host")
	require.NoError(t, conn.Close())
}
