// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVolumeReplacesInvalidUTF8LikeTheDaemonSees(t *testing.T) {
	parsed, err := ParseVolume("/data/\xff\xfe:/mnt:ro")
	require.NoError(t, err)
	assert.Equal(t, VolumeSpec{Type: mount.TypeBind, Source: "/data/��", Target: "/mnt", ReadOnly: true}, parsed)
}
