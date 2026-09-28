// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// The results service is one origin serving every github.actions.results.api.v1 service, and
// Gitea implements only the artifact half of it. Forwarding that half from here makes this origin
// the whole service, so ACTIONS_RESULTS_URL can point at it truthfully, which is what the clients
// this runner cannot patch need, docker buildx among them.
const artifactServicePath = "/twirp/github.actions.results.api.v1.ArtifactService/"

// forwardOrNotFound is the router's fallback: the artifact service of the instance the job
// registered with, and the 404 the router would have written otherwise.
func (h *Handler) forwardOrNotFound(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, artifactServicePath) {
		http.NotFound(w, r)
		return
	}
	cred, ok := h.lookupCredential(bearerToken(r))
	if !ok {
		h.twirpError(w, r, twirpUnauthenticated, errors.New("unknown bearer token"))
		return
	}
	if cred.Results == "" {
		h.twirpError(w, r, twirpInternal, errors.New("no instance is registered for this job"))
		return
	}
	target, err := url.Parse(strings.TrimSuffix(cred.Results, "/"))
	if err == nil && (target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https")) {
		err = errors.New("not an absolute http or https URL the job could use")
	}
	if err != nil {
		h.logger.Errorf("artifact service forward to %q: %v", cred.Results, err)
		h.twirpError(w, r, twirpInternal, fmt.Errorf("artifact service address %q is unusable: %w", cred.Results, err))
		return
	}
	if !h.mustProxy(r, cred, target) {
		redirect := target.JoinPath(r.URL.EscapedPath())
		redirect.RawQuery = mergeQuery(target.RawQuery, r.URL.RawQuery)
		h.logger.Debugf("%s %s: redirecting to %s", r.Method, r.URL.Path, target)
		http.Redirect(w, r, redirect.String(), http.StatusTemporaryRedirect)
		return
	}
	h.logger.Debugf("%s %s: forwarding to %s", r.Method, r.URL.Path, target)

	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Gitea builds the URLs it hands back from this Host, and their scheme from the
			// connection unless a forwarded header overrides it, so artifact bodies go to Gitea
			// directly and never through here.
			r.Out.Host = target.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.logger.Warnf("artifact service forward to %s: %v", target, err)
			h.twirpError(w, r, twirpInternal, fmt.Errorf("cache server cannot reach the artifact service at %s: %w", target, err))
		},
	}
	if cred.InsecureTLS {
		proxy.Transport = insecureTransport
	}
	proxy.ServeHTTP(w, r)
}

// A protocol switch loses the bearer or the agent, and an untrusted instance needs skipped verification.
func (h *Handler) mustProxy(r *http.Request, cred JobCredential, target *url.URL) bool {
	if cred.InsecureTLS && target.Scheme == "https" {
		return true
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}
	return scheme != target.Scheme
}

func mergeQuery(target, request string) string {
	if target == "" || request == "" {
		return target + request
	}
	return target + "&" + request
}

// insecureTransport is shared, because a transport per request would pool no connections.
var insecureTransport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // the runner reaches its instance on the operator's say-so
