// Copyright 2024 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package run

import (
	"io"

	"gitea.com/gitea/runner/internal/pkg/report"

	log "github.com/sirupsen/logrus"
)

type JobLoggerWithReporter struct {
	Reporter *report.Reporter
}

// WithJobLogger forwards logs to the reporter without terminal output.
func (j JobLoggerWithReporter) WithJobLogger() *log.Logger {
	logger := log.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(log.TraceLevel)
	logger.AddHook(j.Reporter)

	return logger
}
