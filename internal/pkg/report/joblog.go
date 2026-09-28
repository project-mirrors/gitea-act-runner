// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.com/gitea/runner/internal/pkg/config"

	log "github.com/sirupsen/logrus"
)

const (
	jobLogNameLayout = "20060102-150405"
	jobLogTimestamp  = "2006-01-02T15:04:05.000Z"
)

// jobLog is this task's copy of the rows sent to Gitea. A nil *jobLog is a no-op, and every
// caller holds Reporter.stateMu, so it needs no lock.
type jobLog struct {
	file    *os.File
	size    int64
	max     int64
	stopped bool // the cap was reached or a write failed, only the trailer still follows
	closed  bool
}

// openJobLog returns nil when the logs are off or cannot be created: a copy must never fail a job.
func openJobLog(cfg config.LogJob, taskID int64, started time.Time) *jobLog {
	if cfg.Dir == "" {
		return nil
	}

	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil { // repository output, readable by this user only
		log.Warnf("cannot create job log directory %s: %v", cfg.Dir, err)
		return nil
	}
	pruneJobLogs(cfg.Dir, cfg.Retention, started)

	name := fmt.Sprintf("%s-task-%d.log", started.UTC().Format(jobLogNameLayout), taskID)
	file, err := os.OpenFile(filepath.Join(cfg.Dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		log.Warnf("cannot create job log: %v", err)
		return nil
	}
	log.Infof("writing the log of task %d to %s", taskID, file.Name())
	return &jobLog{file: file, max: int64(cfg.MaxSize)}
}

func (j *jobLog) write(t time.Time, content string) {
	if j == nil || j.stopped || j.closed {
		return
	}
	line := t.UTC().Format(jobLogTimestamp) + " " + content
	if j.max > 0 && j.size+int64(len(line))+1 > j.max {
		j.stopped = true
		j.line(runnerLine(fmt.Sprintf("truncated: log.job.max_size of %d bytes reached", j.max)))
		return
	}
	j.line(line)
}

func (j *jobLog) close(trailer string) {
	if j == nil || j.closed {
		return
	}
	j.closed = true
	j.line(runnerLine(trailer)) // past the cap on purpose: no trailer means the runner died mid-job
	if err := j.file.Close(); err != nil {
		log.Warnf("cannot close %s: %v", j.file.Name(), err)
	}
}

// line writes unbuffered, so a killed runner keeps what it had written. Only the runner's own
// lines can carry a newline, a row reaching Gitea cannot (see DEVELOPMENT.md).
func (j *jobLog) line(content string) {
	n, err := j.file.WriteString(strings.ReplaceAll(content, "\n", `\n`) + "\n")
	j.size += int64(n)
	if err != nil {
		j.stopped = true // reported once, a failing write is a full disk and retrying floods the log
		log.Warnf("cannot write %s: %v", j.file.Name(), err)
	}
}

func runnerLine(content string) string {
	return time.Now().UTC().Format(jobLogTimestamp) + " [runner] " + content
}

// pruneJobLogs removes the logs older than retention. The age comes from the name, not the
// mtime, which a reader or a backup tool can move.
func pruneJobLogs(root string, retention time.Duration, now time.Time) {
	if retention <= 0 {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		log.Warnf("cannot list job log directory %s: %v", root, err)
		return
	}

	cutoff := now.Add(-retention)
	for _, entry := range entries {
		stamp, _, isTaskLog := strings.Cut(entry.Name(), "-task-")
		if !isTaskLog || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		if started, err := time.Parse(jobLogNameLayout, stamp); err != nil || !started.Before(cutoff) {
			continue
		}
		name := filepath.Join(root, entry.Name())
		if err := os.Remove(name); err != nil {
			log.Warnf("cannot remove expired job log %s: %v", name, err)
		}
	}
}
