// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyProxy forwards TCP to Gitea and can drop every connection to simulate an outage.
type flakyProxy struct {
	ln      net.Listener
	target  string
	down    atomic.Bool
	refused atomic.Int64
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
}

func newFlakyProxy(t *testing.T, targetURL string) *flakyProxy {
	t.Helper()
	u, err := url.Parse(targetURL)
	if err != nil {
		t.Fatalf("parse %q: %v", targetURL, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &flakyProxy{ln: ln, target: u.Host, conns: map[net.Conn]struct{}{}}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.dropAll()
	})
	return p
}

func (p *flakyProxy) URL() string { return "http://" + p.ln.Addr().String() }

func (p *flakyProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			p.refused.Add(1)
			_ = c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
	}
}

func (p *flakyProxy) dropAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	clear(p.conns)
}

// cut starts the outage and returns once the runner has been refused at least refusals more times.
func (p *flakyProxy) cut(t *testing.T, refusals int64) {
	t.Helper()
	before := p.refused.Load()
	p.down.Store(true)
	p.dropAll()
	deadline := time.Now().Add(30 * time.Second)
	for p.refused.Load() < before+refusals {
		if time.Now().After(deadline) {
			t.Fatal("runner never retried during the outage")
		}
		time.Sleep(pollInterval)
	}
}

func (p *flakyProxy) restore() { p.down.Store(false) }

// testReconnect checks that a runner recovers from a lost connection to Gitea, idle or mid-job.
func testReconnect(t *testing.T) {
	t.Parallel()
	api, repo := newScenario(t)
	proxy := newFlakyProxy(t, fixture.baseURL)
	startRunner(t, repo, "e2e-reconnect", runnerOptions{host: true, address: proxy.URL()})

	proxy.cut(t, 2)
	pushWorkflow(t, api, repo, "reconnect_idle.yml")
	idle := waitForRun(t, api, repo)
	proxy.restore()
	requireSuccess(t, api, repo, idle.ID)

	pushWorkflow(t, api, repo, "reconnect.yml")
	slow := waitForRunAfter(t, api, repo, idle.ID)
	waitForRunningJobLog(t, api, repo, slow.ID, "e2e-before-outage")
	// the job logs a line per second, so a long outage spans several that can only arrive after it
	proxy.cut(t, 8)
	proxy.restore()
	requireSuccess(t, api, repo, slow.ID)
	logs := runLogs(t, api, repo, slow.ID)
	for i := 1; i <= 15; i++ {
		if tick := fmt.Sprintf("tick-%02d", i); commandRow(logs, tick) != tick {
			t.Errorf("log line %s written during or after the outage was lost:\n%s", tick, logs)
			break
		}
	}
}
