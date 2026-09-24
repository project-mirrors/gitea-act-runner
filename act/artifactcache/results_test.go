// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"cmp"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrontResultsService(t *testing.T) {
	var gotHost, gotPath, gotProto string
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath, gotProto = r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Proto")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer gitea.Close()
	selfSigned := httptest.NewTLSServer(gitea.Config.Handler)
	defer selfSigned.Close()

	handler, err := StartHandler(Options{Dir: t.TempDir(), OutboundIP: "127.0.0.1"})
	require.NoError(t, err)
	defer handler.Close()
	const token, rpc = "forward-token", artifactServicePath + "CreateArtifact"

	noRedirect := func(token string) *http.Client {
		return &http.Client{
			Transport:     &bearerTransport{token: token},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	post := func(path string) (int, map[string]string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, handler.ExternalURL()+path, nil)
		require.NoError(t, err)
		resp, err := noRedirect(token).Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body := map[string]string{}
		_ = json.UnmarshalRead(resp.Body, &body)
		return resp.StatusCode, body
	}

	status, body := post(rpc)
	assert.Equal(t, http.StatusUnauthorized, status, "an unregistered token is told so, not left to parse a 404 page")
	assert.Equal(t, twirpUnauthenticated, body["code"])
	assert.Contains(t, body["msg"], "bearer")
	assert.NotEmpty(t, body["msg"])

	defer handler.RegisterJob(token, JobCredential{Repo: "owner/repo", Results: selfSigned.URL, InsecureTLS: true})()

	status, _ = post(rpc)
	require.Equal(t, http.StatusOK, status, "an http job reaching an https instance must proxy")
	assert.Equal(t, strings.TrimPrefix(selfSigned.URL, "https://"), gotHost, "Gitea must see the host it mints its URLs from")
	assert.Empty(t, gotProto, "a forwarded scheme would make an https Gitea mint http URLs")
	assert.Equal(t, rpc, gotPath)

	gotPath = ""
	status, _ = post("/twirp/github.actions.results.api.v1.OtherService/Do")
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = post("/api/v1/repos/owner/repo")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Empty(t, gotPath, "only the artifact service is forwarded")

	tests := []struct {
		name           string
		cred           JobCredential
		forwardedProto string
		reqQuery       string
		want           int
		location       string
		wantMsg        string
	}{
		{name: "an instance the job reaches itself", cred: JobCredential{Results: gitea.URL}, want: http.StatusTemporaryRedirect},
		{name: "a sub-path instance keeps its prefix", cred: JobCredential{Results: gitea.URL + "/sub"}, want: http.StatusTemporaryRedirect},
		{name: "an instance with no certificate to distrust", cred: JobCredential{Results: gitea.URL, InsecureTLS: true}, want: http.StatusTemporaryRedirect},
		{
			name: "both queries survive", cred: JobCredential{Results: gitea.URL + "?tenant=one"}, reqQuery: "?run=7",
			want: http.StatusTemporaryRedirect, location: gitea.URL + rpc + "?tenant=one&run=7",
		},
		{
			name: "a trusted https instance is reached by the job itself",
			cred: JobCredential{Results: selfSigned.URL}, forwardedProto: "https", want: http.StatusTemporaryRedirect,
		},
		{
			name: "the job does not share this server's disregard for the certificate",
			cred: JobCredential{Results: selfSigned.URL, InsecureTLS: true}, forwardedProto: "https", want: http.StatusOK,
		},
		{name: "an http job is not redirected to an https instance", cred: JobCredential{Results: selfSigned.URL}, want: http.StatusInternalServerError},
		{
			name: "a forwarded https scheme onto an http instance is proxied",
			cred: JobCredential{Results: gitea.URL}, forwardedProto: "https", want: http.StatusOK,
		},
		{
			name: "a scheme the job could not use is refused before it is sent one",
			cred: JobCredential{Results: "ftp://gitea.example"}, want: http.StatusInternalServerError, wantMsg: "unusable",
		},
		{
			name: "an address with no host is refused before it is sent one",
			cred: JobCredential{Results: "http://:3000"}, want: http.StatusInternalServerError, wantMsg: "unusable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer handler.RegisterJob(tt.name, tt.cred)()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, handler.ExternalURL()+rpc+tt.reqQuery, nil)
			require.NoError(t, err)
			if tt.forwardedProto != "" {
				req.Header.Set("X-Forwarded-Proto", tt.forwardedProto)
			}
			resp, err := noRedirect(tt.name).Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			require.Equal(t, tt.want, resp.StatusCode)
			switch tt.want {
			case http.StatusTemporaryRedirect:
				assert.Equal(t, cmp.Or(tt.location, tt.cred.Results+rpc), resp.Header.Get("Location"))
			case http.StatusInternalServerError:
				body := map[string]string{}
				require.NoError(t, json.UnmarshalRead(resp.Body, &body))
				assert.Equal(t, twirpInternal, body["code"])
				assert.Contains(t, body["msg"], cmp.Or(tt.wantMsg, tt.cred.Results))
			}
		})
	}
}
