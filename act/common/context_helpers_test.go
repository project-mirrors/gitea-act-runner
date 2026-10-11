// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package common

import (
	"errors"
	"testing"

	"gitea.com/gitea/runner/act/common/logging"
	"gitea.com/gitea/runner/act/common/logging/logtest"
)

func TestDryrunContext(t *testing.T) {
	ctx := t.Context()
	if Dryrun(ctx) {
		t.Fatal("plain context should not be dryrun")
	}
	if !Dryrun(WithDryrun(ctx, true)) {
		t.Fatal("WithDryrun(true) should set dryrun")
	}
	if Dryrun(WithDryrun(ctx, false)) {
		t.Fatal("WithDryrun(false) should clear dryrun")
	}
}

func TestJobErrorContainer(t *testing.T) {
	ctx := t.Context()
	err := errors.New("job failed")

	SetJobError(ctx, err)
	if got := JobError(ctx); got != nil {
		t.Fatalf("JobError without container = %v, want nil", got)
	}

	ctx = WithJobErrorContainer(ctx)
	SetJobError(ctx, err)
	if got := JobError(ctx); !errors.Is(got, err) {
		t.Fatalf("JobError = %v, want %v", got, err)
	}
}

func TestLoggerAndHandlerContext(t *testing.T) {
	ctx := t.Context()
	if Logger(ctx) != logging.Default() {
		t.Fatal("plain context should use the default logger")
	}
	if LoggerHandler(ctx) != nil {
		t.Fatal("plain context should not have a logger handler")
	}

	logger := logging.New(logtest.New())
	ctx = WithLogger(ctx, logger)
	if Logger(ctx) != logger {
		t.Fatal("WithLogger should set logger")
	}

	handler := logtest.New()
	ctx = WithLoggerHandler(ctx, handler)
	if LoggerHandler(ctx) != handler {
		t.Fatal("WithLoggerHandler should set handler")
	}
}

func TestStepIDsContext(t *testing.T) {
	ctx := t.Context()
	if got := StepIDs(ctx); len(got) != 0 {
		t.Fatalf("plain context step IDs = %v, want none", got)
	}

	ids := []string{"a"}
	ctx = WithStepIDs(ctx, ids)
	ids[0] = "changed"
	if got := StepIDs(ctx); len(got) != 1 || got[0] != "a" {
		t.Fatalf("StepIDs = %v, want [a]", got)
	}
}
