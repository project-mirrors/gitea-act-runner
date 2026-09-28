// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package report

import (
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalMasks(t *testing.T) {
	formatter := MaskingFormatter(&log.TextFormatter{DisableTimestamp: true})
	format := func(job string) string {
		line, err := formatter.Format(log.WithField("job", job))
		require.NoError(t, err)
		return string(line)
	}
	register := func(oldnew ...string) *Reporter {
		r := &Reporter{oldnew: oldnew}
		registerGlobalMasks(r)
		t.Cleanup(func() { deregisterGlobalMasks(r) })
		return r
	}

	assert.Contains(t, format("build s3cr3t"), "s3cr3t")

	first := register("s3cr3t", "***")
	second := register("other", "***", "otherlonger", "***")

	line := format("build s3cr3t and other")
	assert.NotContains(t, line, "s3cr3t") // masked though it rode a field, not the message
	assert.NotContains(t, line, "other")
	assert.NotContains(t, format("otherlonger"), "longer") // longest first, so not "***longer"

	deregisterGlobalMasks(first)
	line = format("build s3cr3t and other")
	assert.Contains(t, line, "s3cr3t")
	assert.NotContains(t, line, "other") // the task still running keeps its own

	deregisterGlobalMasks(second)
	assert.Nil(t, globalReplacer.Load())

	// A workflow could otherwise mask "error" here and rewrite every other task's log.
	third := register("s3cr3t", "***")
	third.addMask("runtime-secret")
	assert.Contains(t, format("saw runtime-secret"), "runtime-secret")
	assert.NotContains(t, third.mask("saw runtime-secret"), "runtime-secret")
}
