// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"slices"
	"strconv"
	"sync"

	"gitea.com/gitea/runner/internal/pkg/config"
)

// slotAllocator hands out the lowest free slot, so slot numbers stay below capacity.
type slotAllocator struct {
	mu   sync.Mutex
	used []bool
}

func (a *slotAllocator) acquire() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	slot := slices.Index(a.used, false)
	if slot < 0 {
		slot = len(a.used)
		a.used = append(a.used, false)
	}
	a.used[slot] = true
	return slot
}

func (a *slotAllocator) release(slot int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[slot] = false
}

// toolCacheSlot leaves slot 0 unnamed, so it keeps the cache a runner had before slots existed.
func toolCacheSlot(mode string, slot int) string {
	if mode != config.ToolCacheModeShared || slot == 0 {
		return ""
	}
	return strconv.Itoa(slot)
}
