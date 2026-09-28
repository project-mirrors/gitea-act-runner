// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !WITHOUT_DOCKER && (linux || darwin || netbsd)

package container

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func copyDockerSocketPermissions(daemonSocket, socket string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("docker socket ownership is unavailable")
	}
	groupErr := os.Chown(socket, int(stat.Uid), int(stat.Gid))
	if groupErr != nil && (!errors.Is(groupErr, fs.ErrPermission) || int(stat.Uid) != os.Geteuid()) {
		return groupErr
	}
	if err := os.Chown(filepath.Dir(socket), int(stat.Uid), -1); err != nil {
		return err
	}
	mode := uint16(info.Mode().Perm())
	if err := os.Chmod(socket, fs.FileMode(mode)); err != nil || groupErr == nil {
		return err
	}
	if size, err := unix.Getxattr(daemonSocket, "system.posix_acl_access", nil); err == nil && size > 0 {
		return groupErr
	}
	owner, group, other := mode>>6, mode>>3&7, mode&7
	acl := binary.LittleEndian.AppendUint32(nil, 2)
	for _, entry := range []struct {
		tag, perm uint16
		id        uint32
	}{{1, owner, math.MaxUint32}, {4, other, math.MaxUint32}, {8, group, stat.Gid}, {16, group | other, math.MaxUint32}, {32, other, math.MaxUint32}} {
		acl = binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint16(acl, entry.tag), entry.perm), entry.id)
	}
	return unix.Setxattr(socket, "system.posix_acl_access", acl, 0) // names the group a rootless runner cannot chown to, its own group keeps what others had
}
