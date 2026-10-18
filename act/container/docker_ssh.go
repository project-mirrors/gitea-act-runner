// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/cli/cli/connhelper/ssh"
)

func sshArgs(daemonURL string) ([]string, error) {
	spec, err := ssh.ParseURL(daemonURL)
	if err != nil {
		return nil, err
	}
	remote := []string{"docker", "system", "dial-stdio"}
	if strings.Trim(spec.Path, "/") != "" {
		remote = []string{"docker", "--host=unix://" + spec.Path, "system", "dial-stdio"}
	}
	return spec.Command([]string{"-o", "ConnectTimeout=30", "-T"}, remote...)
}

func dialCommand(name string, args ...string) (net.Conn, error) {
	conn := &commandConn{cmd: exec.Command(name, args...)}
	conn.cmd.SysProcAttr = sshSysProcAttr() // a session of its own like docker's, so a ProxyCommand never reads the runner's terminal: https://github.com/docker/cli/issues/1707
	conn.cmd.Stderr = &conn.stderr
	conn.cmd.WaitDelay = time.Second // a ProxyCommand child keeping stderr open would otherwise block Wait after ssh exits
	conn.wait = sync.OnceValue(func() error {
		if err := conn.cmd.Wait(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
			return fmt.Errorf("%s: %w: %s", strings.Join(conn.cmd.Args, " "), err, strings.TrimSpace(conn.stderr.String()))
		}
		return nil
	})
	var err error
	if conn.stdin, err = conn.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if conn.stdout, err = conn.cmd.StdoutPipe(); err != nil {
		return nil, err
	}
	return conn, conn.cmd.Start()
}

type commandConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr cappedBuffer
	wait   func() error
}

func (c *commandConn) waitWithin(timeout time.Duration) error {
	timer := time.AfterFunc(timeout, func() { _ = c.cmd.Process.Kill() })
	defer timer.Stop()
	return c.wait()
}

func (c *commandConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	if errors.Is(err, io.EOF) {
		if waitErr := c.waitWithin(10 * time.Second); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

func (c *commandConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// CloseWrite lets the daemon see the end of a hijacked stream, like a TCP half-close.
func (c *commandConn) CloseWrite() error { return c.stdin.Close() }

func (c *commandConn) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process.Signal(syscall.SIGTERM) != nil { // SIGTERM lets ssh take its ProxyCommand down, Windows only kills
		_ = c.cmd.Process.Kill()
	}
	_ = c.waitWithin(3 * time.Second)
	return nil
}

func (c *commandConn) LocalAddr() net.Addr { return &net.UnixAddr{Name: "stdio", Net: "unix"} }

func (c *commandConn) RemoteAddr() net.Addr { return &net.UnixAddr{Name: "stdio", Net: "unix"} }

func (c *commandConn) SetDeadline(_deadline time.Time) error { return nil }

func (c *commandConn) SetReadDeadline(_deadline time.Time) error { return nil }

func (c *commandConn) SetWriteDeadline(_deadline time.Time) error { return nil }

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len() > 4096 {
		b.Reset()
	}
	return b.Buffer.Write(p)
}
