// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package report

import (
	"strings"
	"sync"
	"sync/atomic"

	"gitea.com/gitea/runner/act/runner"

	log "github.com/sirupsen/logrus"
)

// A job's plan runs before its job logger exists and logs through the process-wide one, which
// several tasks share, so that one holds the union of every live task's starting secrets.
var (
	globalMu       sync.Mutex
	globalMasks    = map[*Reporter][]string{}
	globalReplacer atomic.Pointer[strings.Replacer] // nil while nothing is registered
)

func registerGlobalMasks(r *Reporter) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalMasks[r] = r.oldnew
	rebuildGlobalReplacer()
}

func deregisterGlobalMasks(r *Reporter) {
	globalMu.Lock()
	defer globalMu.Unlock()
	delete(globalMasks, r)
	rebuildGlobalReplacer()
}

func rebuildGlobalReplacer() { // caller holds globalMu
	var oldnew []string
	for _, masks := range globalMasks {
		oldnew = append(oldnew, masks...)
	}
	if len(oldnew) == 0 {
		globalReplacer.Store(nil)
		return
	}
	globalReplacer.Store(runner.NewSecretReplacer(oldnew))
}

// MaskingFormatter wraps f so a registered value cannot reach the process-wide log, fields included.
func MaskingFormatter(f log.Formatter) log.Formatter {
	return &maskingFormatter{inner: f}
}

type maskingFormatter struct{ inner log.Formatter }

func (m *maskingFormatter) Format(entry *log.Entry) ([]byte, error) {
	line, err := m.inner.Format(entry)
	if err != nil {
		return nil, err
	}
	replacer := globalReplacer.Load()
	if replacer == nil { // nothing to hide, so an idle daemon pays no copy
		return line, nil
	}
	return []byte(replacer.Replace(string(line))), nil
}
