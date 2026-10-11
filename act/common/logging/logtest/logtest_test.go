// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package logtest_test

import (
	"testing"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLogger(t *testing.T) {
	logger, hook := logtest.NewLogger()
	logger.With("job", "j1").Debug("below the default level")
	logger.With("job", "j1").Infof("hello %s", "world")

	require.Len(t, hook.AllRecords(), 1)
	assert.Equal(t, "hello world", hook.LastRecord().Message)
	v, _ := hook.LastRecord().Attr("job")
	assert.Equal(t, "j1", v)
	assert.True(t, logtest.NewWithLevel(logging.LevelTrace).Enabled(t.Context(), logging.LevelTrace))
}
