// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.com/gitea/runner/act/common/logging/logtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientCertIsValidatedAndAbsoluteOnLoadReadOnUseAndOfferedOnlyWhenAccepted(t *testing.T) {
	require.True(t, (&Runner{Insecure: true}).TLSConfig().InsecureSkipVerify)

	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	writePair := func() []byte {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		template := &x509.Certificate{SerialNumber: big.NewInt(1)}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		require.NoError(t, err)
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
		require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
		return der
	}
	load := func(certFile, keyFile string) (*Config, error) {
		return LoadDefault(write(t, "runner:\n  client_cert_file: '"+certFile+"'\n  client_key_file: '"+keyFile+"'\n"))
	}
	writePair()

	_, err := load(certPath, "")
	require.ErrorContains(t, err, "must be set together")
	_, err = load(keyPath, keyPath)
	require.ErrorContains(t, err, "load runner client certificate")

	t.Chdir(dir)
	cfg, err := load("client.crt", "client.key")
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(cfg.Runner.ClientCertFile))
	require.True(t, filepath.IsAbs(cfg.Runner.ClientKeyFile))
	renewed := writePair()
	info := &tls.CertificateRequestInfo{Version: tls.VersionTLS13, SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256}}
	pair, err := cfg.Runner.TLSConfig().GetClientCertificate(info)
	require.NoError(t, err)
	require.Equal(t, renewed, pair.Certificate[0])
	info.AcceptableCAs = [][]byte{[]byte("another CA")}
	pair, err = cfg.Runner.TLSConfig().GetClientCertificate(info)
	require.NoError(t, err)
	require.Empty(t, pair.Certificate)
}

func TestLoadDefault_ExpandsEnvironmentValues(t *testing.T) {
	t.Setenv("RUNNER_CAPACITY", "4")
	t.Setenv("RUNNER_LABEL", "linux:host")
	t.Setenv("RUNNER_KEY", "fetch_timeout")
	t.Setenv("RUNNER_EMPTY", "")
	t.Setenv("RUNNER_MISSING", "")
	require.NoError(t, os.Unsetenv("RUNNER_MISSING"))
	path := write(t, `runner:
  capacity: ${RUNNER_CAPACITY}
  labels:
    - ${RUNNER_LABEL}
  envs:
    LITERAL: $${RUNNER_LABEL}
  ${RUNNER_KEY}: 1s
cache:
  enabled: ${RUNNER_EMPTY}
`)

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.Runner.Capacity)
	assert.Equal(t, []string{"linux:host"}, cfg.Runner.Labels)
	assert.Equal(t, map[string]string{"LITERAL": "${RUNNER_LABEL}"}, cfg.Runner.Envs)
	assert.Equal(t, 5*time.Second, cfg.Runner.FetchTimeout)
	assert.True(t, *cfg.Cache.Enabled)

	_, err = LoadDefault(write(t, "runner:\n\n  capacity: ${RUNNER_MISSING}\n  file: ${RUNNER_MISSING}\n"))
	require.ErrorContains(t, err, "line 3: ${RUNNER_MISSING} is not set\nline 4: ${RUNNER_MISSING} is not set")
	_, err = LoadDefault(write(t, "runner:\n\n  capacity: ${RUNNER_LABEL}\n"))
	require.ErrorContains(t, err, "line 3:")
}

func TestLoadDefault_CacheS3(t *testing.T) {
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access.key")
	require.NoError(t, os.WriteFile(accessPath, []byte("access\n"), 0o600))
	path := filepath.Join(dir, "config.yaml")
	content := `
cache:
  s3:
    endpoint: "http://minio:9000"
    bucket: "caches"
    prefix: "/runners/"
    access_key_file: "` + accessPath + `"
    secret_key: "secret"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, CacheS3{
		Endpoint: "http://minio:9000", Bucket: "caches", Prefix: "runners",
		AccessKey: "access", AccessKeyFile: accessPath, SecretKey: "secret",
	}, *cfg.Cache.S3)

	require.NoError(t, os.WriteFile(path, []byte(content+`  external_server: "http://cache.invalid/"
  external_secret: "shh"
`), 0o600))
	_, err = LoadDefault(path)
	assert.ErrorContains(t, err, "cache.s3 and cache.external_server cannot both be set")
}

func TestLoadDefault_ToolCacheMode(t *testing.T) {
	path := write(t, "runner:\n  tool_cache_mode: shared\n")
	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, ToolCacheModeShared, cfg.Runner.ToolCacheMode)

	require.NoError(t, os.WriteFile(path, []byte("runner:\n  tool_cache_mode: everyone\n"), 0o600))
	_, err = LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_cache_mode")
}

func TestLoadDefault_RejectsInvalidLogReportLevel(t *testing.T) {
	_, err := LoadDefault(write(t, "runner:\n  log_report_level: loud\n"))
	require.ErrorContains(t, err, "runner.log_report_level")
}

func TestLoadDefault_LoadsExtraHeaders(t *testing.T) {
	path := write(t, "runner:\n  extra_headers:\n    X-Proxy-Token: proxy-token\n")
	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"X-Proxy-Token": "proxy-token"}, cfg.Runner.ExtraHeaders)

	for _, headers := range []string{
		"X-Proxy Token: proxy-token\n",
		"X-Proxy-Token: |\n      proxy-token\n",
		"X-Proxy-Token: a\n    x-proxy-token: b\n",
	} {
		require.NoError(t, os.WriteFile(path, []byte("runner:\n  extra_headers:\n    "+headers), 0o600))
		_, err = LoadDefault(path)
		assert.ErrorContains(t, err, "runner.extra_headers", headers)
	}
}

func TestLoadDefault_Defaults(t *testing.T) {
	cfg, err := LoadDefault("")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, cfg.Runner.WorkdirCleanupAge)
	assert.Equal(t, 10*time.Minute, cfg.Runner.IdleCleanupInterval)
	assert.Equal(t, DefaultImage, cfg.Runner.DefaultImage)
	assert.Equal(t, ToolCacheModeNone, cfg.Runner.ToolCacheMode)
	assert.False(t, cfg.HealthCheck.Enabled)
	assert.Equal(t, int64(1024), cfg.HealthCheck.MinFreeDiskSpaceMB)
	assert.Empty(t, cfg.HealthCheck.Script)
	assert.Equal(t, 30*time.Second, cfg.HealthCheck.Interval)
	assert.Equal(t, 10*time.Second, cfg.HealthCheck.Timeout)
	assert.False(t, cfg.Metrics.Enabled)
}

func TestLoadDefault_DiskAndReadinessSettings(t *testing.T) {
	cfg, err := LoadDefault(write(t, `
health_check:
  enabled: true
  min_free_disk_space_mb: 4096
  script: /usr/local/bin/runner-health
  interval: 15s
  timeout: 3s
metrics:
  readiness_grace: 45s
`))
	require.NoError(t, err)
	assert.True(t, cfg.HealthCheck.Enabled)
	assert.Equal(t, int64(4096), cfg.HealthCheck.MinFreeDiskSpaceMB)
	assert.Equal(t, "/usr/local/bin/runner-health", cfg.HealthCheck.Script)
	assert.Equal(t, 15*time.Second, cfg.HealthCheck.Interval)
	assert.Equal(t, 3*time.Second, cfg.HealthCheck.Timeout)
	assert.Equal(t, 45*time.Second, cfg.Metrics.ReadinessGrace)
}

func TestLoadDefault_WorkdirCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, yaml    string
		age, interval time.Duration
	}{
		{"configured age", "\nrunner:\n  workdir_cleanup_age: 2h30m\n", 150 * time.Minute, 10 * time.Minute},
		{"configured interval", "\nrunner:\n  idle_cleanup_interval: 45m\n", 24 * time.Hour, 45 * time.Minute},
		{"disabled", "\nrunner:\n  workdir_cleanup_age: 0s\n  idle_cleanup_interval: 0s\n", 0, 0},
		{"negative", "\nrunner:\n  workdir_cleanup_age: -1s\n  idle_cleanup_interval: -1s\n", -time.Second, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadDefault(write(t, tc.yaml))
			require.NoError(t, err)
			assert.Equal(t, tc.age, cfg.Runner.WorkdirCleanupAge)
			assert.Equal(t, tc.interval, cfg.Runner.IdleCleanupInterval)
		})
	}
}

func TestLoadDefault_LoadsPostTaskScript(t *testing.T) {
	for _, tc := range []struct {
		name, timeout string
		want          time.Duration
	}{
		{"configured timeout", "  post_task_script_timeout: 2m\n", 2 * time.Minute},
		{"default timeout", "", 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadDefault(write(t, "\nrunner:\n  post_task_script: /usr/local/bin/post-task.sh\n"+tc.timeout))
			require.NoError(t, err)
			assert.Equal(t, "/usr/local/bin/post-task.sh", cfg.Runner.PostTaskScript)
			assert.Equal(t, tc.want, cfg.Runner.PostTaskScriptTimeout)
		})
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadDefault_LoadsCacheEviction(t *testing.T) {
	t.Run("sizes accept any spelling of the unit", func(t *testing.T) {
		cfg, err := LoadDefault(write(t, "cache:\n  retention: 336h\n  repo_size_limit: 50gb\n  size_limit: 1TiB\n  sweep_interval: 15m\n"))
		require.NoError(t, err)
		assert.Equal(t, 336*time.Hour, cfg.Cache.Retention)
		assert.Equal(t, Size(50*1024*1024*1024), cfg.Cache.RepoSizeLimit)
		assert.Equal(t, Size(1024*1024*1024*1024), cfg.Cache.SizeLimit)
		assert.Equal(t, 15*time.Minute, cfg.Cache.SweepInterval)
	})

	t.Run("zero turns a limit off where an absent key keeps its default", func(t *testing.T) {
		cfg, err := LoadDefault(write(t, "cache:\n  repo_size_limit: 0\n"))
		require.NoError(t, err)
		assert.Zero(t, cfg.Cache.RepoSizeLimit)
		assert.Equal(t, DefaultCache().Retention, cfg.Cache.Retention, "an absent key still defaults")
	})

	t.Run("a bad size names the offending value", func(t *testing.T) {
		_, err := LoadDefault(write(t, "cache:\n  repo_size_limit: banana\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "banana")
	})
}

func TestLoadDefault_LoadsJobLogs(t *testing.T) {
	cfg, err := LoadDefault(write(t, "log:\n  job:\n    dir: /var/log/jobs\n    retention: 0s\n    max_size: 100MB\n"))
	require.NoError(t, err)
	assert.Equal(t, "/var/log/jobs", cfg.Log.Job.Dir)
	assert.Zero(t, cfg.Log.Job.Retention, "zero keeps every task directory")
	assert.Equal(t, Size(100*1024*1024), cfg.Log.Job.MaxSize)
}

func TestLoadDefault_LoadsJobHooks(t *testing.T) {
	cfg, err := LoadDefault(write(t, `
runner:
  hooks:
    job_started: /hooks/started.sh
    job_completed: /hooks/completed.sh
`))
	require.NoError(t, err)
	assert.Equal(t, "/hooks/started.sh", cfg.Runner.Hooks.JobStarted)
	assert.Equal(t, "/hooks/completed.sh", cfg.Runner.Hooks.JobCompleted)
}

func TestLoadDefault_MalformedYAMLReturnsParseError(t *testing.T) {
	_, err := LoadDefault(write(t, "runner:\n  capacity: [unterminated\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config file")
	assert.NotContains(t, err.Error(), "defaults metadata")
}

func TestLoadDefault_WarnsOnUnknownKeysButStillLoads(t *testing.T) {
	hook := logtest.SetDefault(t)

	cfg, err := LoadDefault(write(t, "container:\n  volumes:\n    - /host:/ctr\n  privileged: true\n"))
	require.NoError(t, err)
	assert.True(t, cfg.Container.Privileged)
	require.Len(t, hook.AllRecords(), 1)
	assert.Contains(t, hook.LastRecord().Message, "field volumes not found")
}

func TestContainerNetworkCreateOptions(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       ContainerNetworkCreateOptions
	}{
		{"enable_ipv6 true unmarshals to non-nil true", "container:\n  network_create_options:\n    enable_ipv6: true\n", ContainerNetworkCreateOptions{EnableIPv6: new(true)}},
		{"enable_ipv6 false unmarshals to non-nil false", "container:\n  network_create_options:\n    enable_ipv6: false\n", ContainerNetworkCreateOptions{EnableIPv6: new(false)}},
		{"enable_ipv4 false unmarshals to non-nil false", "container:\n  network_create_options:\n    enable_ipv4: false\n", ContainerNetworkCreateOptions{EnableIPv4: new(false)}},
		{"omitted keys stay nil", "container:\n  network_create_options:\n    enable_ipv4: true\n", ContainerNetworkCreateOptions{EnableIPv4: new(true)}},
		{"omitted block leaves both nil", "container:\n  network: \"\"\n", ContainerNetworkCreateOptions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadDefault(write(t, tc.yaml))
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Container.NetworkCreateOptions)
		})
	}
}

func TestLoadDefault_ExternalCacheSecret(t *testing.T) {
	const externalCache = "\ncache:\n  enabled: true\n  external_server: \"http://cache.invalid/\"\n"
	for _, tc := range []struct {
		name, yaml, fileContent, wantSecret, wantErr string
	}{
		{name: "without secret", yaml: externalCache, wantErr: "external_secret"},
		{name: "inline secret", yaml: externalCache + "  external_secret: \"shh\"\n"},
		{
			name: "from file", yaml: externalCache + "  external_secret_file: \"$DIR/cache.secret\"\n",
			fileContent: "  s3cr3t\n", wantSecret: "s3cr3t",
		},
		{
			name: "from file when disabled", yaml: "\ncache:\n  enabled: false\n  external_secret_file: \"$DIR/cache.secret\"\n",
			fileContent: "s3cr3t", wantSecret: "s3cr3t",
		},
		{
			name: "inline and file conflict", yaml: externalCache + "  external_secret: \"inline\"\n  external_secret_file: \"$DIR/cache.secret\"\n",
			fileContent: "s3cr3t", wantErr: "both set",
		},
		{name: "missing file", yaml: externalCache + "  external_secret_file: \"$DIR/absent.secret\"\n", wantErr: "read cache.external_secret_file"},
		{
			name: "empty file", yaml: externalCache + "  external_secret_file: \"$DIR/cache.secret\"\n",
			fileContent: "\n  \n", wantErr: "contains no secret",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.fileContent != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "cache.secret"), []byte(tc.fileContent), 0o600))
			}
			path := filepath.Join(dir, "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(tc.yaml, "$DIR", dir)), 0o600))
			cfg, err := LoadDefault(path)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
				if tc.wantSecret != "" {
					assert.Equal(t, tc.wantSecret, cfg.Cache.ExternalSecret)
				}
			}
		})
	}
}

func TestLoadDefault_ShippedConfigsChangeNothing(t *testing.T) {
	hook := logtest.SetDefault(t)

	defaults, err := LoadDefault("")
	require.NoError(t, err)

	dir := t.TempDir()
	for name, content := range map[string][]byte{"config.example.yaml": Example, "minimal.yaml": []byte(Minimal)} {
		file := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(file, content, 0o600))

		cfg, err := LoadDefault(file)
		require.NoError(t, err, name)
		assert.Equal(t, defaults, cfg, name)
	}
	assert.Empty(t, hook.AllRecords())
}

func TestLoadDefault_ClampsFetchTimeout(t *testing.T) {
	cfg, err := LoadDefault(write(t, `
runner:
  fetch_timeout: 120s
`))
	require.NoError(t, err)
	assert.Equal(t, RequestTimeout, cfg.Runner.FetchTimeout)
}
