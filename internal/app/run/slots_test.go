// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"testing"

	"gitea.com/gitea/runner/internal/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolCacheSlotReusesLowestFree(t *testing.T) {
	var slots slotAllocator
	require.Equal(t, 0, slots.acquire())
	require.Equal(t, 1, slots.acquire())
	require.Equal(t, 2, slots.acquire())
	slots.release(1)
	assert.Equal(t, 1, slots.acquire())
	assert.Equal(t, 3, slots.acquire())

	assert.Equal(t, "3", toolCacheSlot(config.ToolCacheModeShared, 3))
	assert.Empty(t, toolCacheSlot(config.ToolCacheModeShared, 0))
	assert.Empty(t, toolCacheSlot(config.ToolCacheModeNone, 3))
}
