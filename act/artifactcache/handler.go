// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2023 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package artifactcache

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/internal/pkg/disk"

	"github.com/julienschmidt/httprouter"
	"github.com/sirupsen/logrus"
	"github.com/timshannon/bolthold"
	"go.etcd.io/bbolt"
)

const (
	apiPath      = "/_apis/artifactcache"
	internalPath = "/_internal"

	// artifactURLTTL bounds how long a signed artifactLocation URL stays valid.
	// Short enough that a leaked URL is near-worthless; long enough to let the
	// @actions/cache client download a big blob that was returned from /cache.
	artifactURLTTL = 10 * time.Minute
)

type credKey struct{}

// JobCredential ties a per-job bearer token (ACTIONS_RUNTIME_TOKEN) to the
// repository that owns it. Every cache entry is stamped with Repo on
// reserve/commit and checked on read/write so one repo can never observe or
// poison another repo's cache, even from inside a container that reaches the
// cache server over the docker bridge network.
type JobCredential struct {
	Repo string `json:"repo"`

	// Results is the instance whose artifact service this server forwards for the job, and
	// InsecureTLS how the runner reaches it; see results.go.
	Results     string `json:"-"`
	InsecureTLS bool   `json:"-"`

	// PublicURL is this server as a reverse proxy makes the job reach it, not the listen address.
	PublicURL string `json:"public_url"`
}

// credEntry holds a registered job's credential along with an active
// registration count. RegisterJob is reference-counted so that if two tasks
// briefly share an ACTIONS_RUNTIME_TOKEN — e.g. a runner that retries a task
// after a crash before the old registration is revoked — the first task's
// revoker does not cut the second task's auth out from under it.
type credEntry struct {
	cred JobCredential
	refs int
}

type Handler struct {
	dir      string
	storage  *Storage
	router   *httprouter.Router
	listener net.Listener
	port     int
	server   *http.Server
	logger   logrus.FieldLogger

	gcing atomic.Bool
	gcAt  time.Time

	outboundIP string

	// internalSecret guards /_internal/{register,revoke}. When set, a remote
	// runner can use these endpoints to pre-register per-job
	// ACTIONS_RUNTIME_TOKENs against this server, enabling the same
	// per-job auth and repo scoping as the embedded handler over the
	// network. Empty disables the control-plane entirely.
	internalSecret string

	// secret signs short-lived artifact download URLs. The @actions/cache
	// toolkit does not send Authorization on the download request, so blob
	// GETs authenticate via a per-URL HMAC signature with expiry rather than
	// via the bearer token used for management endpoints.
	secret []byte

	credMu sync.RWMutex
	creds  map[string]*credEntry

	policy Policy

	// freeDisk is a field so tests can drive evictForFreeSpace without a full volume.
	freeDisk func(string) (uint64, error)
}

// Options configures a cache server started by StartHandler; the zero value is usable.
type Options struct {
	Dir        string
	OutboundIP string
	Port       uint16
	Upstream   string

	// InternalSecret, when non-empty, enables a control-plane API at
	// /_internal/{register,revoke} that lets a remote runner pre-register the
	// per-job ACTIONS_RUNTIME_TOKENs it expects this server to honor. The
	// embedded in-process handler leaves it empty and registers tokens via the
	// in-process RegisterJob method directly.
	InternalSecret string

	Policy Policy
	Logger logrus.FieldLogger
}

// StartHandler starts an embedded cache or forwards cache requests to Upstream.
func StartHandler(opts Options) (*Handler, error) {
	dir, logger := opts.Dir, opts.Logger
	h := &Handler{
		creds:          make(map[string]*credEntry),
		internalSecret: opts.InternalSecret,
		policy:         opts.Policy.withDefaults(),
		freeDisk:       disk.FreeBytes,
	}

	if logger == nil {
		discard := logrus.New()
		discard.Out = io.Discard
		logger = discard
	}
	logger = logger.WithField("module", "artifactcache")
	h.logger = logger

	if opts.OutboundIP != "" {
		h.outboundIP = opts.OutboundIP
	} else if ip := common.GetOutboundIP(); ip == nil {
		return nil, errors.New("unable to determine outbound IP address")
	} else {
		h.outboundIP = ip.String()
	}

	if opts.Upstream != "" {
		handler, err := h.cacheForwarder(opts.Upstream)
		if err != nil {
			return nil, err
		}
		if err := h.serve(opts.Port, handler); err != nil {
			return nil, err
		}
		return h, nil
	}

	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".cache", "actcache")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	h.dir = dir

	storage, err := NewStorage(filepath.Join(dir, "cache"))
	if err != nil {
		return nil, err
	}
	h.storage = storage

	secret, err := loadOrCreateSecret(dir)
	if err != nil {
		return nil, err
	}
	h.secret = secret

	router := httprouter.New()
	router.GET(apiPath+"/cache", h.bearerAuth(h.find))
	router.POST(apiPath+"/caches", h.bearerAuth(h.reserve))
	router.PATCH(apiPath+"/caches/:id", h.bearerAuth(h.upload))
	router.POST(apiPath+"/caches/:id", h.bearerAuth(h.commit))
	router.POST(apiPath+"/clean", h.bearerAuth(h.clean))
	// Artifact GET is signed via query-string HMAC because @actions/cache
	// does not attach Authorization when downloading archiveLocation.
	router.GET(apiPath+"/artifacts/:id", h.signedAuth("", h.get))
	// Control-plane: a remote runner registers/revokes per-job tokens so the
	// cache API can authenticate them. Always wired so the routes exist; the
	// handlers themselves 401 when internalSecret is unset.
	router.POST(internalPath+"/register", h.internalAuth(h.internalRegister))
	router.POST(internalPath+"/revoke", h.internalAuth(h.internalRevoke))
	h.registerV2Routes(router)
	router.NotFound = http.HandlerFunc(h.forwardOrNotFound)

	h.router = router

	h.gcCache()

	if err := h.serve(opts.Port, router); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Handler) serve(port uint16, handler http.Handler) error {
	// Listen on all interfaces. Binding to outboundIP only would give no real
	// security benefit (it is the LAN/internet-facing address either way) and
	// can break Docker Desktop variants where the host's outbound IP is not
	// routable from inside the container network. Authentication is enforced
	// by the bearer middleware and per-repo scoping, not by reachability.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		listener.Close()
		return fmt.Errorf("cache server listens on %T, want a TCP address", listener.Addr())
	}
	h.port = addr.Port
	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           handler,
	}
	go func() {
		if err := server.Serve(listener); err != nil && errors.Is(err, net.ErrClosed) {
			h.logger.Errorf("http serve: %v", err)
		}
	}()
	h.listener = listener
	h.server = server

	return nil
}

func (h *Handler) cacheForwarder(upstream string) (http.Handler, error) {
	target, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse cache upstream: %w", err)
	}
	transport, _ := http.DefaultTransport.(*http.Transport)
	transport = transport.Clone()
	transport.MaxIdleConnsPerHost = 64 // parallel block uploads would otherwise redial past the default 2
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.URL.RawQuery = r.In.URL.RawQuery // Rewrite drops unparsable query params, and signed URLs must pass verbatim.
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.logger.Warnf("cache forward %s %s: %v", r.Method, r.URL.Path, err)
			h.twirpError(w, r, twirpUnavailable, errors.New("cache upstream is unavailable"))
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, apiPath+"/") || strings.HasPrefix(r.URL.Path, cacheServiceV2Path+"/") {
			proxy.ServeHTTP(w, r)
		} else {
			h.forwardOrNotFound(w, r)
		}
	}), nil
}

func (h *Handler) ExternalURL() string {
	// TODO: make the external url configurable if necessary
	return "http://" + net.JoinHostPort(h.outboundIP, strconv.Itoa(h.port))
}

func (h *Handler) baseURL(cred JobCredential) string {
	if base := strings.TrimRight(cred.PublicURL, "/"); base != "" {
		return base
	}
	return h.ExternalURL()
}

// RegisterJob makes token a valid bearer credential for cache requests from
// the given repository and returns a function that removes it. The runner
// calls this at job start and defers the returned func so that the credential
// is only accepted while the job is running.
//
// Registrations are reference-counted: if a token is already registered, the
// credential it was registered with is kept and the refcount is incremented.
// The entry is removed only when every revoker returned by RegisterJob has
// been called.
// This keeps a stray re-registration from silently revoking a live job.
func (h *Handler) RegisterJob(token string, cred JobCredential) func() {
	if h == nil || token == "" {
		return func() {}
	}
	h.credMu.Lock()
	if existing, ok := h.creds[token]; ok {
		existing.refs++
	} else {
		h.creds[token] = &credEntry{
			cred: cred,
			refs: 1,
		}
	}
	h.credMu.Unlock()
	return func() {
		h.credMu.Lock()
		if entry, ok := h.creds[token]; ok {
			entry.refs--
			if entry.refs <= 0 {
				delete(h.creds, token)
			}
		}
		h.credMu.Unlock()
	}
}

// RevokeJob explicitly revokes one registration of token, mirroring one call
// of the closure returned by RegisterJob. Used by the control-plane endpoint
// so a remote runner can revoke without holding the closure.
func (h *Handler) RevokeJob(token string) {
	if h == nil || token == "" {
		return
	}
	h.credMu.Lock()
	if entry, ok := h.creds[token]; ok {
		entry.refs--
		if entry.refs <= 0 {
			delete(h.creds, token)
		}
	}
	h.credMu.Unlock()
}

func (h *Handler) lookupCredential(token string) (JobCredential, bool) {
	h.credMu.RLock()
	entry, ok := h.creds[token]
	h.credMu.RUnlock()
	if !ok {
		return JobCredential{}, false
	}
	return entry.cred, true
}

// loadOrCreateSecret returns the 32-byte HMAC signing key for artifact URLs,
// persisted in dir/.secret so signed URLs handed out before a restart stay
// valid across the restart and so the standalone cache-server can be pointed
// at by config.Cache.ExternalServer without the URL rotating.
func loadOrCreateSecret(dir string) ([]byte, error) {
	path := filepath.Join(dir, ".secret")
	if data, err := os.ReadFile(path); err == nil {
		if secret, err := hex.DecodeString(strings.TrimSpace(string(data))); err == nil && len(secret) >= 32 {
			return secret, nil
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read cache secret: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate cache secret: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
		return nil, fmt.Errorf("write cache secret: %w", err)
	}
	return secret, nil
}

func (h *Handler) Close() error {
	if h == nil {
		return nil
	}
	var retErr error
	if h.server != nil {
		err := h.server.Close()
		if err != nil {
			retErr = err
		}
		h.server = nil
	}
	if h.listener != nil {
		err := h.listener.Close()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
		if err != nil {
			retErr = err
		}
		h.listener = nil
	}
	return retErr
}

func (h *Handler) openDB() (*bolthold.Store, error) {
	return bolthold.Open(filepath.Join(h.dir, "bolt.db"), 0o644, &bolthold.Options{
		Encoder: func(value any) ([]byte, error) { return json.Marshal(value) },
		Decoder: func(data []byte, value any) error { return json.Unmarshal(data, value) },
		Options: &bbolt.Options{
			Timeout:      5 * time.Second,
			NoGrowSync:   bbolt.DefaultOptions.NoGrowSync,
			FreelistType: bbolt.DefaultOptions.FreelistType,
		},
	})
}

// GET /_apis/artifactcache/cache
func (h *Handler) find(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	cred := credFromContext(r.Context())
	keys := strings.Split(r.URL.Query().Get("keys"), ",")
	version := r.URL.Query().Get("version")

	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()

	cache, err := h.lookupCache(db, cred.Repo, keys, version)
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	if cache == nil {
		h.responseJSON(w, r, 204)
		return
	}
	h.responseJSON(w, r, 200, map[string]any{
		"result":          "hit",
		"archiveLocation": h.signedArtifactURL(cred, cache.ID, time.Now().Add(artifactURLTTL)),
		"cacheKey":        cache.Key,
	})
}

// lookupCache returns the entry to restore for these keys, or (nil, nil) when there is none:
// either nothing matched, or the match had lost its blob to a prune, in which case the dangling
// entry is dropped on the way out.
func (h *Handler) lookupCache(db *bolthold.Store, repo string, keys []string, version string) (*Cache, error) {
	cache, err := findCache(db, repo, keys, version)
	if err != nil || cache == nil {
		return nil, err
	}
	ok, err := h.storage.Exist(cache.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		_ = db.Delete(cache.ID, cache)
		return nil, nil //nolint:nilnil // absence is not an error here
	}
	// Handing out a download URL counts as access, or eviction could drop the entry between
	// this call and the GET that follows it.
	h.touch(db, cache)
	return cache, nil
}

// POST /_apis/artifactcache/caches
func (h *Handler) reserve(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	cred := credFromContext(r.Context())
	api := &Request{}
	if err := json.UnmarshalRead(r.Body, api); err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache := &Cache{Repo: cred.Repo, Key: api.Key, Version: api.Version, Size: api.Size}
	if cache.Size == 0 {
		cache.Size = -1
	}
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()

	now := time.Now().Unix()
	cache.CreatedAt = now
	cache.UsedAt = now
	if err := insertCache(db, cache); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	h.responseJSON(w, r, 200, map[string]any{
		"cacheId": cache.ID,
	})
}

// PATCH /_apis/artifactcache/caches/:id
func (h *Handler) upload(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	cred := credFromContext(r.Context())
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache := &Cache{}
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()
	if err := db.Get(id, cache); err != nil {
		if errors.Is(err, bolthold.ErrNotFound) {
			h.responseJSON(w, r, 400, fmt.Errorf("cache %d: not reserved", id))
			return
		}
		h.responseJSON(w, r, 500, err)
		return
	}

	if cache.Repo != cred.Repo {
		h.responseJSON(w, r, 403, fmt.Errorf("cache %d: forbidden", id))
		return
	}

	if cache.Complete {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %v %q: already complete", cache.ID, cache.Key))
		return
	}
	db.Close()
	start, _, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	if err := h.storage.Write(cache.ID, start, r.Body); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	_ = h.touchCache(uint64(id), false)
	h.responseJSON(w, r, 200)
}

// POST /_apis/artifactcache/caches/:id
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	cred := credFromContext(r.Context())
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}

	cache := &Cache{}
	db, err := h.openDB()
	if err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}
	defer db.Close()
	if err := db.Get(id, cache); err != nil {
		if errors.Is(err, bolthold.ErrNotFound) {
			h.responseJSON(w, r, 400, fmt.Errorf("cache %d: not reserved", id))
			return
		}
		h.responseJSON(w, r, 500, err)
		return
	}

	if cache.Repo != cred.Repo {
		h.responseJSON(w, r, 403, fmt.Errorf("cache %d: forbidden", id))
		return
	}

	if cache.Complete {
		h.responseJSON(w, r, 400, fmt.Errorf("cache %v %q: already complete", cache.ID, cache.Key))
		return
	}

	db.Close()

	if err := h.commitCache(cache); err != nil {
		h.responseJSON(w, r, 500, err)
		return
	}

	h.responseJSON(w, r, 200)
}

// commitCache assembles the uploaded parts and marks the entry complete. The caller must
// have closed its store first: Commit concatenates the whole archive and would otherwise
// hold bolt's exclusive file lock for the duration.
func (h *Handler) commitCache(cache *Cache) error {
	written, err := h.storage.Commit(cache.ID, cache.Size)
	if err != nil {
		return err
	}
	// write real size back to cache, it may be different from the current value when the request doesn't specify it.
	cache.Size = written
	cache.Complete = true
	cache.UsedAt = time.Now().Unix() // a just-written entry counts as accessed, so it cannot be its own eviction victim

	db, err := h.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Update(cache.ID, cache); err != nil {
		return err
	}
	// A commit is the only thing that grows the store, so the only thing that can push the
	// volume under the floor.
	h.evictRepo(db, cache.Repo)
	h.evictTotal(db)
	h.evictForFreeSpace(db)
	return nil
}

// GET /_apis/artifactcache/artifacts/:id
// Authenticated via signed URL (see signedAuth), not bearer, because the
// @actions/cache toolkit downloads archiveLocation without Authorization.
// Repository scoping is already enforced at find() time; the signature binds
// the URL to the specific cache ID and an expiry.
func (h *Handler) get(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
	if err != nil {
		h.responseJSON(w, r, 400, err)
		return
	}
	_ = h.touchCache(uint64(id), false)
	h.storage.Serve(w, r, uint64(id))
}

// POST /_apis/artifactcache/clean
func (h *Handler) clean(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	// TODO: don't support force deleting cache entries
	// see: https://docs.github.com/en/actions/using-workflows/caching-dependencies-to-speed-up-workflows#force-deleting-cache-entries

	h.responseJSON(w, r, 200)
}

// bearerAuth resolves ACTIONS_RUNTIME_TOKEN against the set of currently
// registered jobs. A match attaches the job's JobCredential to the request
// context; a miss returns 401 before the handler body runs.
func (h *Handler) bearerAuth(handler httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		h.logger.Debugf("%s %s", r.Method, r.URL.Path)
		token := bearerToken(r)
		if token == "" {
			h.unauthorized(w, r, errors.New("missing bearer token"))
			return
		}
		cred, ok := h.lookupCredential(token)
		if !ok {
			h.unauthorized(w, r, errors.New("unknown bearer token"))
			return
		}
		ctx := context.WithValue(r.Context(), credKey{}, cred)
		handler(w, r.WithContext(ctx), params)
		go h.gcCache()
	}
}

func (h *Handler) unauthorized(w http.ResponseWriter, r *http.Request, err error) {
	if strings.HasPrefix(r.URL.Path, cacheServiceV2Path) {
		h.twirpError(w, r, twirpUnauthenticated, err)
		return
	}
	h.responseJSON(w, r, http.StatusUnauthorized, err)
}

// signedAuth authenticates a signed URL. purpose separates the flavours of URL the
// handler hands out, so one cannot be replayed as another; see computeSignature.
func (h *Handler) signedAuth(purpose string, handler httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		h.logger.Debugf("%s %s", r.Method, r.URL.Path)
		id, err := strconv.ParseInt(params.ByName("id"), 10, 64)
		if err != nil {
			h.responseJSON(w, r, 400, err)
			return
		}
		expStr := r.URL.Query().Get("exp")
		sig := r.URL.Query().Get("sig")
		if expStr == "" || sig == "" {
			h.responseJSON(w, r, http.StatusUnauthorized, errors.New("missing signature"))
			return
		}
		exp, err := strconv.ParseInt(expStr, 10, 64)
		if err != nil {
			h.responseJSON(w, r, http.StatusUnauthorized, errors.New("invalid expiry"))
			return
		}
		if time.Now().Unix() > exp {
			h.responseJSON(w, r, http.StatusUnauthorized, errors.New("signature expired"))
			return
		}
		expected := h.computeSignature(purpose, id, exp)
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			h.responseJSON(w, r, http.StatusUnauthorized, errors.New("bad signature"))
			return
		}
		handler(w, r, params)
		go h.gcCache()
	}
}

// internalAuth gates the control-plane endpoints. The bearer must
// constant-time-equal the configured internalSecret. If the secret is empty,
// the control-plane is disabled and every request gets 404 — which matches
// the upstream nektos/act behavior of "the route does not exist".
func (h *Handler) internalAuth(handler httprouter.Handle) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		if h.internalSecret == "" {
			http.NotFound(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" || !hmac.Equal([]byte(token), []byte(h.internalSecret)) {
			h.responseJSON(w, r, http.StatusUnauthorized, errors.New("internal: bad secret"))
			return
		}
		handler(w, r, params)
	}
}

type internalRegisterBody struct {
	Token string `json:"token"`
	JobCredential
}

type internalRevokeBody struct {
	Token string `json:"token"`
}

// POST /_internal/register
// ResultsURL is what a job registered with cred should be given as ACTIONS_RESULTS_URL, or "" when
// the credential names no instance to forward the artifact half to.
func (h *Handler) ResultsURL(cred JobCredential) string {
	if h == nil || cred.Results == "" {
		return ""
	}
	return h.baseURL(cred)
}

func (h *Handler) internalRegister(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var body internalRegisterBody
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		h.responseJSON(w, r, http.StatusBadRequest, err)
		return
	}
	if body.Token == "" {
		h.responseJSON(w, r, http.StatusBadRequest, errors.New("token is required"))
		return
	}
	h.RegisterJob(body.Token, body.JobCredential)
	h.responseJSON(w, r, http.StatusOK)
}

// POST /_internal/revoke
func (h *Handler) internalRevoke(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var body internalRevokeBody
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		h.responseJSON(w, r, http.StatusBadRequest, err)
		return
	}
	if body.Token == "" {
		h.responseJSON(w, r, http.StatusBadRequest, errors.New("token is required"))
		return
	}
	h.RevokeJob(body.Token)
	h.responseJSON(w, r, http.StatusOK)
}

// hashedToken fingerprints a job's bearer, so a reservation can tell its own retry from another job.
func hashedToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

func credFromContext(ctx context.Context) JobCredential {
	if cred, ok := ctx.Value(credKey{}).(JobCredential); ok {
		return cred
	}
	return JobCredential{}
}

// computeSignature signs a URL for one cache entry and expiry. purpose is mixed into the
// message so a URL handed out for writing an entry cannot be replayed to read one, and the
// other way round. Downloads use the empty purpose, the message v1 has always signed.
func (h *Handler) computeSignature(purpose string, cacheID, exp int64) string {
	mac := hmac.New(sha256.New, h.secret)
	fmt.Fprintf(mac, "%s%d:%d", purpose, cacheID, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// signedURL builds a URL under path that signedAuth accepts for the same purpose.
func (h *Handler) signedURL(cred JobCredential, path, purpose string, cacheID uint64, exp time.Time) string {
	expUnix := exp.Unix()
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(expUnix, 10))
	q.Set("sig", h.computeSignature(purpose, int64(cacheID), expUnix))
	return fmt.Sprintf("%s%s/%d?%s", h.baseURL(cred), path, cacheID, q.Encode())
}

func (h *Handler) signedArtifactURL(cred JobCredential, cacheID uint64, exp time.Time) string {
	return h.signedURL(cred, apiPath+"/artifacts", "", cacheID, exp)
}

// if not found, return (nil, nil) instead of an error.
func findCache(db *bolthold.Store, repo string, keys []string, version string) (*Cache, error) {
	cache := &Cache{}
	for _, prefix := range keys {
		// if a key in the list matches exactly, don't return partial matches
		exact, err := findExactCache(db, repo, prefix, version, true)
		if err != nil {
			return nil, err
		}
		if exact != nil {
			return exact, nil
		}
		if err := db.FindOne(cache,
			bolthold.Where("Repo").Eq(repo).Index("Repo").
				And("Key").MatchFunc(func(key string) (bool, error) { return strings.HasPrefix(key, prefix), nil }).
				And("Version").Eq(version).
				And("Complete").Eq(true).
				SortBy("CreatedAt").Reverse()); err != nil {
			if errors.Is(err, bolthold.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("find cache: %w", err)
		}
		return cache, nil
	}
	return nil, nil //nolint:nilnil // pre-existing issue from nektos/act
}

// findExactCache returns the entry for exactly this key and version, or (nil, nil) if there is
// none. Unlike findCache it never falls back to a prefix (restore-key) match, which is what both
// its callers need: a new key that is only a prefix of an existing key is not the same entry.
//
// A completed entry is the one to restore, sorted by when it was written. An incomplete one is a
// reservation being uploaded to, sorted by when it was last written to, because the upload route
// touches UsedAt on every part.
func findExactCache(db *bolthold.Store, repo, key, version string, complete bool) (*Cache, error) {
	sortBy := "UsedAt"
	if complete {
		sortBy = "CreatedAt"
	}
	cache := &Cache{}
	err := db.FindOne(cache,
		bolthold.Where("Key").Eq(key).Index("Key").
			And("Repo").Eq(repo).
			And("Version").Eq(version).
			And("Complete").Eq(complete).
			SortBy(sortBy).Reverse())
	if errors.Is(err, bolthold.ErrNotFound) {
		return nil, nil //nolint:nilnil // absence is not an error here
	}
	if err != nil {
		return nil, fmt.Errorf("find cache: %w", err)
	}
	return cache, nil
}

func insertCache(db *bolthold.Store, cache *Cache) error {
	return db.Bolt().Update(func(tx *bbolt.Tx) error {
		if err := db.TxInsert(tx, bolthold.NextSequence(), cache); err != nil {
			return fmt.Errorf("insert cache: %w", err)
		}
		// write back id to db
		if err := db.TxUpdate(tx, cache.ID, cache); err != nil {
			return fmt.Errorf("write back id to db: %w", err)
		}
		return nil
	})
}

// touchCache stamps UsedAt so gcCache does not reap an entry mid-upload. With requireIncomplete
// it also refuses an entry that is already complete, which is what the v2 blob route needs: its
// upload URL outlives the finalize call, and overwriting a finished entry would leave the blob
// other jobs restore no longer matching its recorded size. An entry missing from the store is
// accepted, since the signature proves the id was handed out.
func (h *Handler) touchCache(id uint64, requireIncomplete bool) error {
	db, err := h.openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	cache := &Cache{}
	if err := db.Get(id, cache); err != nil {
		if errors.Is(err, bolthold.ErrNotFound) {
			return nil
		}
		return err
	}
	if requireIncomplete && cache.Complete {
		return fmt.Errorf("cache %d: already complete", id)
	}
	if !touchNeeded(cache) {
		return nil
	}
	cache.UsedAt = time.Now().Unix()
	return db.Update(cache.ID, cache)
}

const (
	miB = 1024 * 1024

	defaultSweepInterval = time.Hour

	touchStale = time.Minute // how stale a completed entry's UsedAt may get before an access rewrites it, sparing hits the fsync

	// inUseGrace covers artifactURLTTL plus the touchStale lag so an entry outlives every signed URL
	// still usable for it, and no sweep cuts off a download in progress.
	inUseGrace = artifactURLTTL + touchStale

	// uploadStallTimeout is how long a reservation may sit without a chunk before it counts
	// as abandoned. Widening it also widens the window for findExactCache to hand a finalize
	// a stale reservation.
	uploadStallTimeout = 5 * time.Minute

	defaultMinFreeDisk = 1024 * miB
)

// Policy bounds what the cache server keeps: a retention window counted from last access,
// and size limits that evict least recently accessed first. A zero limit is no limit.
type Policy struct {
	Retention     time.Duration // Retention removes entries nothing has read or written within this window. Zero keeps them regardless of age.
	RepoSizeLimit int64         // RepoSizeLimit caps one repository's completed entries in bytes, evicting least recently accessed first.
	SizeLimit     int64         // SizeLimit caps every repository's completed entries together, in bytes.
	SweepInterval time.Duration // SweepInterval is the minimum time between two eviction sweeps.
	MinFreeDisk   int64         // MinFreeDisk is volume headroom the cache will not eat into. Tracks the runner's health-check floor rather than taking a key of its own.
}

func (p Policy) withDefaults() Policy {
	// The limits default in config.LoadDefault, so a written 0 means off.
	if p.MinFreeDisk <= 0 {
		p.MinFreeDisk = defaultMinFreeDisk
	}
	if p.SweepInterval <= 0 {
		p.SweepInterval = defaultSweepInterval
	}
	return p
}

func (h *Handler) gcCache() {
	if h.gcing.Load() {
		return
	}
	if !h.gcing.CompareAndSwap(false, true) {
		return
	}
	defer h.gcing.Store(false)

	if time.Since(h.gcAt) < h.policy.SweepInterval {
		h.logger.Debugf("skip gc: %v", h.gcAt.String())
		return
	}
	h.gcAt = time.Now()
	h.logger.Debugf("gc: %v", h.gcAt.String())

	db, err := h.openDB()
	if err != nil {
		return
	}
	defer db.Close()

	h.evictIncomplete(db)
	h.evictExpired(db)
	h.evictSuperseded(db)
	h.evictOversized(db)
	h.evictForFreeSpace(db)
}

// evictForFreeSpace bounds the volume itself, so it also covers bytes the cache never
// accounted for.
func (h *Handler) evictForFreeSpace(db *bolthold.Store) {
	free, err := h.freeDisk(h.dir)
	if err != nil {
		h.logger.Debugf("free disk check: %v", err) // unsupported platform, treat as unavailable rather than full
		return
	}
	if free >= uint64(h.policy.MinFreeDisk) {
		return
	}

	caches := h.completedByUse(db)
	total, shortfall := totalSize(caches), h.policy.MinFreeDisk-int64(free)
	if total <= shortfall {
		// Say so, or shedding everything and still being short reads as the backstop working.
		h.logger.Warnf("cache volume is %d MiB short of the free space floor with only %d MiB of cache on it; something else is filling it", shortfall/miB, total/miB)
	}
	h.evictTo(db, caches, total-shortfall, "the cache volume")
}

// evictIncomplete removes uploads that stopped part way, which are most likely broken.
func (h *Handler) evictIncomplete(db *bolthold.Store) {
	h.sweep(db, bolthold.
		Where("UsedAt").Lt(time.Now().Add(-uploadStallTimeout).Unix()).
		And("Complete").Eq(false).
		Index("UsedAt"))
}

func (h *Handler) evictExpired(db *bolthold.Store) {
	if h.policy.Retention <= 0 {
		return
	}
	// Never below inUseGrace, or a short retention would outrun a signed URL already issued.
	window := max(h.policy.Retention+touchStale, inUseGrace)
	h.sweep(db, bolthold.Where("UsedAt").Lt(time.Now().Add(-window).Unix()).Index("UsedAt"))
}

// evictSuperseded removes entries a newer one with the same key and version replaced. The
// aggregation includes Repo so two repos sharing a (key, version) do not evict each other.
func (h *Handler) evictSuperseded(db *bolthold.Store) {
	results, err := db.FindAggregate(&Cache{}, bolthold.Where("Complete").Eq(true).Index("Complete"), "Repo", "Key", "Version")
	if err != nil {
		h.logger.Warnf("find aggregate caches: %v", err)
		return
	}
	var caches []*Cache
	for _, result := range results {
		if result.Count() <= 1 {
			continue
		}
		result.Sort("CreatedAt")
		caches = caches[:0]
		result.Reduction(&caches)
		for _, cache := range caches[:len(caches)-1] {
			if inUse(cache) {
				continue
			}
			h.deleteCache(db, cache)
		}
	}
}

// evictOversized applies the per-repository limit, then the whole-store one. Only completed
// entries count, since only those carry a size measured at commit rather than claimed.
func (h *Handler) evictOversized(db *bolthold.Store) {
	if h.policy.RepoSizeLimit > 0 {
		byRepo := make(map[string][]*Cache)
		for _, cache := range h.completedByUse(db) {
			byRepo[cache.Repo] = append(byRepo[cache.Repo], cache)
		}
		for repo, caches := range byRepo {
			h.evictTo(db, caches, h.policy.RepoSizeLimit, "repository "+repo)
		}
	}
	h.evictTotal(db)
}

// evictTotal caps the store as a whole. It re-queries because the per-repo pass may have
// deleted rows an earlier result still holds.
func (h *Handler) evictTotal(db *bolthold.Store) {
	if h.policy.SizeLimit <= 0 {
		return
	}
	h.evictTo(db, h.completedByUse(db), h.policy.SizeLimit, "the cache")
}

// evictRepo reclaims space when a commit pushes a repo over, rather than at the next sweep.
func (h *Handler) evictRepo(db *bolthold.Store, repo string) {
	if h.policy.RepoSizeLimit <= 0 {
		return
	}
	h.evictTo(db, h.cachesByUse(db, bolthold.Where("Repo").Eq(repo).And("Complete").Eq(true).Index("Repo")), h.policy.RepoSizeLimit, "repository "+repo)
}

// evictTo deletes until caches fit limit. caches must be ordered by UsedAt ascending.
func (h *Handler) evictTo(db *bolthold.Store, caches []*Cache, limit int64, scope string) {
	// An entry bigger than the limit never fits, so it goes on its own account instead of
	// dragging every neighbour out first and then following them next sweep.
	fits := caches[:0]
	for _, cache := range caches {
		if cache.Size <= limit {
			fits = append(fits, cache)
			continue
		}
		if !inUse(cache) {
			h.logger.Warnf("cache %q is %d MiB on its own, over the limit for %s; dropping it", cache.Key, cache.Size/miB, scope)
			h.deleteCache(db, cache)
		}
	}
	caches = fits

	total := totalSize(caches)
	var freed int64
	for _, cache := range caches {
		if total <= limit {
			break
		}
		if inUse(cache) || !h.deleteCache(db, cache) {
			continue
		}
		total -= cache.Size
		freed += cache.Size
	}
	if freed > 0 {
		h.logger.Warnf("evicted %d MiB from %s, least recently used first", freed/miB, scope)
	}
}

// inUse reports whether an entry was read or written recently enough that removing it
// could break a download in progress.
func inUse(cache *Cache) bool {
	return time.Since(time.Unix(cache.UsedAt, 0)) < inUseGrace
}

// touchNeeded skips a write that would not change the second, and completed entries fresher than touchStale.
func touchNeeded(cache *Cache) bool {
	return cache.UsedAt < time.Now().Unix() && (!cache.Complete || time.Since(time.Unix(cache.UsedAt, 0)) >= touchStale)
}

// touch stamps UsedAt through the caller's store, a bolt write on the read path. It cannot
// go through touchCache, which opens its own store and would block on the exclusive lock
// for as long as the caller holds one.
func (h *Handler) touch(db *bolthold.Store, cache *Cache) {
	if !touchNeeded(cache) {
		return
	}
	cache.UsedAt = time.Now().Unix()
	if err := db.Update(cache.ID, cache); err != nil {
		h.logger.Warnf("touch cache: %v", err)
	}
}

func (h *Handler) sweep(db *bolthold.Store, query *bolthold.Query) {
	for _, cache := range h.caches(db, query) {
		h.deleteCache(db, cache)
	}
}

func (h *Handler) caches(db *bolthold.Store, query *bolthold.Query) []*Cache {
	var caches []*Cache
	if err := db.Find(&caches, query); err != nil {
		h.logger.Warnf("find caches: %v", err)
	}
	return caches
}

// cachesByUse returns matches least recently accessed first, sorting here rather than with
// bolthold's SortBy, which reflects over every field it compares.
func (h *Handler) cachesByUse(db *bolthold.Store, query *bolthold.Query) []*Cache {
	caches := h.caches(db, query)
	slices.SortFunc(caches, func(a, b *Cache) int { return cmp.Compare(a.UsedAt, b.UsedAt) })
	return caches
}

// completedByUse returns every entry the size limits count, least recently accessed first.
func (h *Handler) completedByUse(db *bolthold.Store) []*Cache {
	return h.cachesByUse(db, bolthold.Where("Complete").Eq(true).Index("Complete"))
}

func totalSize(caches []*Cache) int64 {
	var total int64
	for _, cache := range caches {
		total += cache.Size
	}
	return total
}

// deleteCache drops an entry and its bytes, reporting whether it went fully. The blob goes
// first, so a failed unlink leaves the row for the next sweep instead of orphaning bytes.
func (h *Handler) deleteCache(db *bolthold.Store, cache *Cache) bool {
	if err := h.storage.Remove(cache.ID); err != nil {
		h.logger.Warnf("remove cache blob: %v", err)
		return false
	}
	if err := db.Delete(cache.ID, cache); err != nil {
		h.logger.Warnf("delete cache: %v", err)
		return false
	}
	h.logger.Infof("deleted cache: %+v", cache)
	return true
}

func (h *Handler) responseJSON(w http.ResponseWriter, r *http.Request, code int, v ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	var data []byte
	if len(v) == 0 || v[0] == nil {
		data, _ = json.Marshal(struct{}{})
	} else if err, ok := v[0].(error); ok {
		h.logger.Errorf("%v %v: %v", r.Method, r.URL.Path, err)
		data, _ = json.Marshal(map[string]any{
			"error": err.Error(),
		})
	} else {
		data, _ = json.Marshal(v[0])
	}
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func parseContentRange(s string) (int64, int64, error) {
	// support the format like "bytes 11-22/*" only
	s, _, _ = strings.Cut(strings.TrimPrefix(s, "bytes "), "/")
	s1, s2, _ := strings.Cut(s, "-")

	start, err := strconv.ParseInt(s1, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %q: %w", s, err)
	}
	stop, err := strconv.ParseInt(s2, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse %q: %w", s, err)
	}
	return start, stop, nil
}
