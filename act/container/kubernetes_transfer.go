// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitea.com/gitea/runner/act/common"
	"gitea.com/gitea/runner/act/common/logging"
)

// KubernetesTransfer has job pods fetch large copies from the runner over the pod network instead of through the API server.
type KubernetesTransfer struct {
	Host     string // empty uses the outbound IP
	Port     uint16 // 0 picks a free port
	Image    string // has gitea-runner on its PATH, for an init container to copy into each job pod
	Fallback bool   // copy through the API server when a job pod cannot reach the runner, else fail
}

// KubernetesTransferCommand is the hidden runner subcommand job pods run the transfer helper with.
const KubernetesTransferCommand = "kubernetes-transfer"

// transferRequest goes to the helper on the exec stream's stdin, keeping the token out of the command line and the audit log.
type transferRequest struct {
	URL string `json:"url"`
	Pin string `json:"pin"` // hex SHA-256 of the runner's certificate
}

type transferServer struct {
	base    string
	pin     string
	mu      sync.Mutex
	pending map[string]transferOffer
}

type transferOffer struct {
	clients []net.IP
	write   func(io.Writer) error
	result  chan error
}

var (
	transferServersMu sync.Mutex
	transferServers   = map[string]*transferServer{}
)

// startTransferServer returns the runner's server for these settings, started on first use.
func startTransferServer(host string, port uint16) (*transferServer, error) {
	transferServersMu.Lock()
	defer transferServersMu.Unlock()
	configured := net.JoinHostPort(host, strconv.Itoa(int(port)))
	if server, ok := transferServers[configured]; ok {
		return server, nil
	}
	if host == "" {
		ip := common.GetOutboundIP()
		if ip == nil {
			return nil, errors.New("no outbound IP for job pods to reach the runner at, set kubernetes.transfer.host")
		}
		host = ip.String()
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1)} // trusted by its pin alone, so no CA, names or validity
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return nil, fmt.Errorf("listen for job pod transfers: %w", err)
	}
	pin := sha256.Sum256(certificate)
	server := &transferServer{base: "https://" + listener.Addr().String() + "/", pin: hex.EncodeToString(pin[:]), pending: map[string]transferOffer{}}
	httpServer := &http.Server{
		Handler:           server,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certificate}, PrivateKey: privateKey}}, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	go func() { _ = httpServer.ServeTLS(listener, "", "") }()
	transferServers[configured] = server
	return server, nil
}

// offer serves write once to one of clients until done, which reports whether a client took it and what write failed with.
func (s *transferServer) offer(clients []net.IP, write func(io.Writer) error) (request transferRequest, done func() (taken bool, err error)) {
	token := rand.Text()
	result := make(chan error, 1)
	s.mu.Lock()
	s.pending[token] = transferOffer{clients: clients, write: write, result: result}
	s.mu.Unlock()
	return transferRequest{URL: s.base + token, Pin: s.pin}, func() (bool, error) {
		s.mu.Lock()
		_, pending := s.pending[token]
		delete(s.pending, token)
		s.mu.Unlock()
		select {
		case err := <-result:
			return true, err
		default:
			return !pending, nil
		}
	}
}

func (s *transferServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	token := strings.TrimPrefix(r.URL.Path, "/")
	s.mu.Lock()
	offer, ok := s.pending[token]
	ok = ok && r.Method == http.MethodGet && slices.ContainsFunc(offer.clients, net.ParseIP(host).Equal)
	if ok {
		delete(s.pending, token)
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	err := writeGzip(w, offer.write)
	offer.result <- err
	if err != nil {
		panic(http.ErrAbortHandler) // a cut-off body fails the fetch, a short tar might not
	}
}

// FetchKubernetesTransfer reads a transferRequest from request and extracts the tar it points to into dest.
func FetchKubernetesTransfer(ctx context.Context, request io.Reader, dest string) error {
	var transfer transferRequest
	if err := json.NewDecoder(request).Decode(&transfer); err != nil {
		return fmt.Errorf("read transfer request: %w", err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Minute,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // VerifyConnection checks the pin instead
			VerifyConnection: func(state tls.ConnectionState) error {
				if sum := sha256.Sum256(state.PeerCertificates[0].Raw); hex.EncodeToString(sum[:]) != transfer.Pin {
					return errors.New("runner certificate does not match the pin")
				}
				return nil
			},
		},
	}}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, transfer.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch: %s", response.Status)
	}
	if err := os.MkdirAll(dest, 0o777); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", dest)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = response.Body, os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, response.Body) // tar stops at the first zero blocks, a cut-off body after them still fails
	return err
}

// InstallKubernetesTransferHelper copies the running binary to target.
func InstallKubernetesTransferHelper(target string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target) // a file left in a reused act volume would keep its mode
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(file, source)
	return errors.Join(err, file.Close())
}

// setupTransfer turns on transfers for the pod, with the runner pod's image unless one is set.
func (p *KubernetesPod) setupTransfer(runnerImage string) error {
	settings := p.options.Transfer
	if settings == nil {
		return nil
	}
	image := cmp.Or(settings.Image, runnerImage)
	var err error
	var server *transferServer
	if image == "" {
		err = errors.New("set kubernetes.transfer.image, the runner is no single-container pod in kubernetes.namespace to take it from")
	} else {
		server, err = startTransferServer(settings.Host, settings.Port)
	}
	if err != nil {
		if !settings.Fallback {
			return fmt.Errorf("kubernetes.transfer: %w", err)
		}
		logging.Warnf("Job pod copies go through the API server: %v", err)
		return nil
	}
	p.transfer, p.transferImage = server, image
	p.transferHelper = path.Join(p.job.GetActPath(), "bin", "gitea-runner")
	return nil
}
