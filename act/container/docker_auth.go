// Copyright 2023 The Gitea Authors. All rights reserved.
// Copyright 2021 The nektos/act Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"gitea.com/gitea/runner/act/common"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
)

func LoadDockerAuthConfig(ctx context.Context, image string) (registry.AuthConfig, error) {
	logger := common.Logger(ctx)
	cfg, err := loadDockerConfig(ctx)
	if err != nil {
		logger.Warnf("Could not load docker config: %v", err)
		return registry.AuthConfig{}, err
	}
	registryKey := registryAuthConfigKey("docker.io")
	if image != "" {
		if registryRef, refErr := reference.ParseNormalizedNamed(image); refErr != nil {
			logger.Warnf("Could not normalize image reference: %v", refErr)
		} else {
			registryKey = registryAuthConfigKey(reference.Domain(registryRef))
		}
	}

	authConfig, err := cfg.authConfig(ctx, registryKey)
	if err != nil {
		logger.Warnf("Could not get auth config from docker config: %v", err)
		return registry.AuthConfig{}, err
	}

	return authConfig, nil
}

func LoadDockerAuthConfigs(ctx context.Context) map[string]registry.AuthConfig {
	logger := common.Logger(ctx)
	cfg, err := loadDockerConfig(ctx)
	if err != nil {
		logger.Warnf("Could not load docker config: %v", err)
		return nil
	}
	authConfigs, err := cfg.allAuthConfigs(ctx)
	if err != nil {
		logger.Warnf("Could not get docker auth configs: %v", err)
		return nil
	}
	return authConfigs
}

func registryAuthConfigKey(domainName string) string {
	if domainName == "docker.io" || domainName == "index.docker.io" {
		return "https://index.docker.io/v1/"
	}
	return domainName
}

type dockerConfig struct {
	Auths       map[string]registry.AuthConfig `json:"auths"`
	CredsStore  string                         `json:"credsStore"`
	CredHelpers map[string]string              `json:"credHelpers"`
	env         map[string]registry.AuthConfig // from DOCKER_AUTH_CONFIG, nil when unset or unusable
}

func loadDockerConfig(ctx context.Context) (*dockerConfig, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		if home == "" && runtime.GOOS != "windows" {
			if current, err := user.Current(); err == nil {
				home = current.HomeDir
			}
		}
		dir = filepath.Join(home, ".docker")
	}
	cfg := &dockerConfig{}
	filename := filepath.Join(dir, "config.json")
	file, err := os.Open(filename)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("loading config file: %w", err)
	}
	if err == nil {
		defer file.Close()
		if err := json.NewDecoder(file).Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parsing config file (%s): %w", filename, err)
		}
	}
	for address, auth := range cfg.Auths {
		if auth.Auth != "" {
			if auth.Username, auth.Password, err = decodeAuth(auth.Auth); err != nil {
				return nil, fmt.Errorf("parsing config file (%s): %w", filename, err)
			}
		}
		auth.Auth, auth.ServerAddress = "", address
		cfg.Auths[address] = auth
	}
	if value := os.Getenv("DOCKER_AUTH_CONFIG"); value != "" {
		if cfg.env, err = parseDockerAuthConfigEnv(value); err != nil {
			common.Logger(ctx).Warnf("Failed to create credential store from DOCKER_AUTH_CONFIG: %v", err)
		}
	}
	return cfg, nil
}

func decodeAuth(auth string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "", err
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok || username == "" {
		return "", "", errors.New("invalid auth configuration file")
	}
	return username, strings.Trim(password, "\x00"), nil
}

func parseDockerAuthConfigEnv(value string) (map[string]registry.AuthConfig, error) {
	var env struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("DOCKER_AUTH_CONFIG does not support more than one JSON object")
	}
	auths := make(map[string]registry.AuthConfig, len(env.Auths))
	for address, entry := range env.Auths {
		if entry.Auth == "" {
			return nil, fmt.Errorf("DOCKER_AUTH_CONFIG environment variable is missing key `auth` for %s", address)
		}
		username, password, err := decodeAuth(entry.Auth)
		if err != nil {
			return nil, err
		}
		auths[address] = registry.AuthConfig{Username: username, Password: password, ServerAddress: address}
	}
	return auths, nil
}

func (c *dockerConfig) authConfig(ctx context.Context, key string) (registry.AuthConfig, error) {
	if auth, ok := c.env[key]; ok {
		return auth, nil
	}
	helper := c.CredsStore
	if named, ok := c.CredHelpers[key]; ok {
		helper = named
	}
	if helper == "" {
		return c.fileAuth(key), nil
	}
	auth := c.fileAuth(key)
	stored, err := helperCredentials(ctx, helper, key)
	if err != nil {
		return auth, err
	}
	auth.Username, auth.Password, auth.IdentityToken, auth.ServerAddress = stored.Username, stored.Password, stored.IdentityToken, stored.ServerAddress
	return auth, nil
}

func (c *dockerConfig) fileAuth(key string) registry.AuthConfig {
	if auth, ok := c.Auths[key]; ok {
		return auth
	}
	for address, auth := range c.Auths {
		if registryHostname(address) == key {
			return auth
		}
	}
	return registry.AuthConfig{}
}

func (c *dockerConfig) allAuthConfigs(ctx context.Context) (map[string]registry.AuthConfig, error) {
	var auths map[string]registry.AuthConfig
	if c.CredsStore == "" {
		auths = maps.Clone(c.Auths)
	} else {
		var err error
		if auths, err = c.helperAuthConfigs(ctx); err != nil {
			if c.env == nil {
				return nil, err
			}
			common.Logger(ctx).Warnf("Could not list the credentials of docker-credential-%s: %v", c.CredsStore, err)
		}
	}
	if auths == nil {
		auths = map[string]registry.AuthConfig{}
	}
	maps.Copy(auths, c.env)
	if c.CredsStore == "" {
		c.Auths = maps.Clone(auths) // docker's file store holds the DOCKER_AUTH_CONFIG entries after listing, and credHelpers set to "" read it
	}
	for address := range c.CredHelpers {
		auth, err := c.authConfig(ctx, registryAuthConfigKey(address))
		if err != nil {
			common.Logger(ctx).Warnf("Failed to get credentials for registry: %s: %v", address, err)
			continue
		}
		auths[address] = auth
	}
	return auths, nil
}

func (c *dockerConfig) helperAuthConfigs(ctx context.Context) (map[string]registry.AuthConfig, error) {
	out, err := runCredentialHelper(ctx, c.CredsStore, "list", "unused")
	if err != nil {
		return nil, fmt.Errorf("error listing credentials - err: %w, out: `%s`", err, strings.TrimSpace(string(out)))
	}
	var addresses map[string]string
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&addresses); err != nil {
		return nil, err
	}
	auths := make(map[string]registry.AuthConfig, len(addresses))
	for address := range addresses {
		stored, err := helperCredentials(ctx, c.CredsStore, address)
		if err != nil {
			return nil, err
		}
		auth := c.Auths[address]
		auth.Username, auth.Password, auth.IdentityToken = stored.Username, stored.Password, stored.IdentityToken
		if auth.ServerAddress == "" {
			auth.ServerAddress = stored.ServerAddress
		}
		auths[address] = auth
	}
	return auths, nil
}

func helperCredentials(ctx context.Context, helper, address string) (registry.AuthConfig, error) {
	out, err := runCredentialHelper(ctx, helper, "get", address)
	if err != nil {
		if strings.TrimSpace(string(out)) == "credentials not found in native keychain" {
			return registry.AuthConfig{}, nil
		}
		return registry.AuthConfig{}, fmt.Errorf("error getting credentials - err: %w, out: `%s`", err, strings.TrimSpace(string(out)))
	}
	var stored struct{ Username, Secret string }
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&stored); err != nil {
		return registry.AuthConfig{}, err
	}
	if stored.Username == "<token>" {
		return registry.AuthConfig{IdentityToken: stored.Secret, ServerAddress: address}, nil
	}
	return registry.AuthConfig{Username: stored.Username, Password: stored.Secret, ServerAddress: address}, nil
}

func runCredentialHelper(ctx context.Context, helper, action, input string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, action)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func registryHostname(address string) string {
	if _, rest, ok := strings.Cut(address, "://"); ok {
		if u, err := url.Parse(address); err == nil && u.Hostname() != "" {
			if u.Port() == "" {
				return u.Hostname()
			}
			return net.JoinHostPort(u.Hostname(), u.Port())
		}
		if host, _, _ := strings.Cut(rest, "/"); host != "" {
			if colon := strings.LastIndex(host, ":"); strings.Count(host, ":") > 1 && !strings.HasPrefix(host, "[") && isDigits(host[colon+1:]) {
				return net.JoinHostPort(host[:colon], host[colon+1:])
			}
			return host
		}
	}
	host, _, _ := strings.Cut(address, "/")
	return host
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
