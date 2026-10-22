// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunnerCloneEnvsConcurrentMutation(t *testing.T) {
	r := &Runner{
		envs: map[string]string{
			"ACTIONS_CACHE_URL":   "http://cache.example",
			"ACTIONS_RUNTIME_URL": "http://runner.example",
		},
	}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			envs := r.cloneEnvs()
			assert.Equal(t, r.envs, envs)
			envs["ACTIONS_RUNTIME_TOKEN"] = "task-token"
			envs["ACTIONS_ID_TOKEN_REQUEST_URL"] = "http://oidc.example"
			envs["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = "oidc-token"
		}()
	}
	wg.Wait()

	assert.Equal(t, map[string]string{
		"ACTIONS_CACHE_URL":   "http://cache.example",
		"ACTIONS_RUNTIME_URL": "http://runner.example",
	}, r.envs)
}
