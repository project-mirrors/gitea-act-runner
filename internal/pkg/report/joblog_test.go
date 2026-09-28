// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package report

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/internal/pkg/client/mocks"
	"gitea.com/gitea/runner/internal/pkg/config"

	connect_go "connectrpc.com/connect"
	runnerv1 "gitea.dev/actionslib/runner/v1"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

var testStart = time.Date(2026, 8, 14, 9, 12, 3, 0, time.UTC)

func readJobLog(t *testing.T, joblog *jobLog) string {
	t.Helper()
	content, err := os.ReadFile(joblog.file.Name())
	require.NoError(t, err)
	return string(content)
}

func TestJobLog_MirrorsUploadedRows(t *testing.T) {
	client := mocks.NewClient(t)
	client.On("UpdateLog", mock.Anything, mock.Anything).Return(func(_ context.Context, req *connect_go.Request[runnerv1.UpdateLogRequest]) (*connect_go.Response[runnerv1.UpdateLogResponse], error) {
		return connect_go.NewResponse(&runnerv1.UpdateLogResponse{AckIndex: req.Msg.Index + int64(len(req.Msg.Rows))}), nil
	})
	client.On("UpdateTask", mock.Anything, mock.Anything).Return(connect_go.NewResponse(&runnerv1.UpdateTaskResponse{}), nil)

	cfg, err := config.LoadDefault("")
	require.NoError(t, err)
	cfg.Log.Job.Dir = t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	task := &runnerv1.Task{Id: 41, Context: &structpb.Struct{}, Secrets: map[string]string{"TOKEN": "s3cret-value"}}
	reporter := NewReporter(ctx, cancel, client, task, cfg)
	require.NotNil(t, reporter.jobLog)
	reporter.RunDaemon()
	reporter.ResetSteps(1)

	fire := func(message string) {
		require.NoError(t, reporter.Fire(&log.Entry{
			Message: message,
			Level:   log.InfoLevel,
			Data:    log.Fields{"stage": "Main", "stepNumber": 0, "raw_output": true},
		}))
	}
	fire("the token is s3cret-value")
	fire("::add-mask::dyn4mic-value")
	fire("and dyn4mic-value too")
	fire("::debug::suppressed unless ACTIONS_STEP_DEBUG")
	require.NoError(t, reporter.Close(""))

	job := readJobLog(t, reporter.jobLog)
	assert.Contains(t, job, "the token is ***")
	assert.Contains(t, job, "and *** too")
	assert.NotContains(t, job, "s3cret-value")
	assert.NotContains(t, job, "dyn4mic-value")
	assert.NotContains(t, job, "add-mask", "the row carrying the secret never reaches the log")
	assert.NotContains(t, job, "suppressed unless", "the file holds only what was sent")
	assert.NotRegexp(t, `(?m)^\S+Z ?$`, job, "the empty row Gitea needs is not job output")
	assert.Contains(t, job, "[runner] task 41 finished: failure")
}

func TestJobLog_MaxSize(t *testing.T) {
	joblog := openJobLog(config.LogJob{Dir: t.TempDir(), MaxSize: 200}, 1, testStart)
	require.NotNil(t, joblog)

	for range 10 {
		joblog.write(testStart, strings.Repeat("x", 40))
	}
	joblog.close("task 1 finished: success")
	joblog.write(testStart, "after the close") // a container goroutine can outlive the step

	job := readJobLog(t, joblog)
	assert.Equal(t, 1, strings.Count(job, "log.job.max_size"), "the cap is reported once")
	assert.NotContains(t, job, "after the close")
	assert.Contains(t, job, "[runner] task 1 finished: success", "the trailer is written past the cap")
}

func TestPruneJobLogs(t *testing.T) {
	root := t.TempDir()
	expired := filepath.Join(root, "20200101-000000-task-1.log")
	fresh := filepath.Join(root, testStart.Format(jobLogNameLayout)+"-task-2.log")
	unrelated := filepath.Join(root, "20200101-000000-task-3.txt")
	for _, name := range []string{expired, fresh, unrelated} {
		require.NoError(t, os.WriteFile(name, []byte("log"), 0o600))
	}

	pruneJobLogs(root, 0, testStart)
	assert.FileExists(t, expired, "retention 0 keeps every log")

	pruneJobLogs(root, 24*time.Hour, testStart)
	assert.NoFileExists(t, expired)
	assert.FileExists(t, fresh)
	assert.FileExists(t, unrelated)
}
