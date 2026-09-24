// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefault_RejectsExternalServerWithoutSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: true
  external_server: "http://cache.invalid/"
`), 0o600))

	_, err := LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external_secret")
}

func TestLoadDefault_AcceptsExternalServerWithSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: true
  external_server: "http://cache.invalid/"
  external_secret: "shh"
`), 0o600))

	_, err := LoadDefault(path)
	require.NoError(t, err)
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
	cfg, err := LoadDefault("")
	require.NoError(t, err)
	assert.Equal(t, ToolCacheModeNone, cfg.Runner.ToolCacheMode)

	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("runner:\n  tool_cache_mode: shared\n"), 0o600))
	cfg, err = LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, ToolCacheModeShared, cfg.Runner.ToolCacheMode)

	require.NoError(t, os.WriteFile(path, []byte("runner:\n  tool_cache_mode: everyone\n"), 0o600))
	_, err = LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_cache_mode")
}

func TestLoadDefault_DefaultsWorkdirCleanupAge(t *testing.T) {
	cfg, err := LoadDefault("")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, cfg.Runner.WorkdirCleanupAge)
	assert.Equal(t, 10*time.Minute, cfg.Runner.IdleCleanupInterval)
	assert.Equal(t, DefaultImage, cfg.Runner.DefaultImage)
}

func TestLoadDefault_HealthChecksAreOptIn(t *testing.T) {
	cfg, err := LoadDefault("")
	require.NoError(t, err)
	assert.False(t, cfg.HealthCheck.Enabled)
	assert.Equal(t, int64(1024), cfg.HealthCheck.MinFreeDiskSpaceMB)
	assert.Empty(t, cfg.HealthCheck.Script)
	assert.Equal(t, 30*time.Second, cfg.HealthCheck.Interval)
	assert.Equal(t, 10*time.Second, cfg.HealthCheck.Timeout)
	assert.False(t, cfg.Metrics.Enabled)
}

func TestLoadDefault_DiskAndReadinessSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
health_check:
  enabled: true
  min_free_disk_space_mb: 4096
  script: /usr/local/bin/runner-health
  interval: 15s
  timeout: 3s
metrics:
  readiness_grace: 45s
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.True(t, cfg.HealthCheck.Enabled)
	assert.Equal(t, int64(4096), cfg.HealthCheck.MinFreeDiskSpaceMB)
	assert.Equal(t, "/usr/local/bin/runner-health", cfg.HealthCheck.Script)
	assert.Equal(t, 15*time.Second, cfg.HealthCheck.Interval)
	assert.Equal(t, 3*time.Second, cfg.HealthCheck.Timeout)
	assert.Equal(t, 45*time.Second, cfg.Metrics.ReadinessGrace)
}

func TestLoadDefault_UsesConfiguredWorkdirCleanupAge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  workdir_cleanup_age: 2h30m
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, 150*time.Minute, cfg.Runner.WorkdirCleanupAge)
}

func TestLoadDefault_UsesConfiguredIdleCleanupInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  idle_cleanup_interval: 45m
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, 45*time.Minute, cfg.Runner.IdleCleanupInterval)
}

func TestLoadDefault_AllowsDisablingWorkdirCleanup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  workdir_cleanup_age: 0s
  idle_cleanup_interval: 0s
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), cfg.Runner.WorkdirCleanupAge)
	assert.Equal(t, time.Duration(0), cfg.Runner.IdleCleanupInterval)
}

func TestLoadDefault_AllowsNegativeWorkdirCleanupValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  workdir_cleanup_age: -1s
  idle_cleanup_interval: -1s
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, -1*time.Second, cfg.Runner.WorkdirCleanupAge)
	assert.Equal(t, -1*time.Second, cfg.Runner.IdleCleanupInterval)
}

func TestLoadDefault_LoadsPostTaskScript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  post_task_script: /usr/local/bin/post-task.sh
  post_task_script_timeout: 2m
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, "/usr/local/bin/post-task.sh", cfg.Runner.PostTaskScript)
	assert.Equal(t, 2*time.Minute, cfg.Runner.PostTaskScriptTimeout)
}

func TestLoadDefault_DefaultsPostTaskScriptTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  post_task_script: /usr/local/bin/post-task.sh
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.Runner.PostTaskScriptTimeout)
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
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  hooks:
    job_started: /hooks/started.sh
    job_completed: /hooks/completed.sh
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, "/hooks/started.sh", cfg.Runner.Hooks.JobStarted)
	assert.Equal(t, "/hooks/completed.sh", cfg.Runner.Hooks.JobCompleted)
}

// TestLoadDefault_MalformedYAMLReturnsParseError pins the error surfaced for
// invalid YAML to the canonical "parse config file" message rather than the
// "for defaults metadata" variant — i.e. the main yaml.Unmarshal runs first.
func TestLoadDefault_MalformedYAMLReturnsParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("runner:\n  capacity: [unterminated\n"), 0o600))

	_, err := LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config file")
	assert.NotContains(t, err.Error(), "defaults metadata")
}

func TestLoadDefault_WarnsOnUnknownKeysButStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("container:\n  volumes:\n    - /host:/ctr\n  privileged: true\n"), 0o600))

	hook := test.NewGlobal()
	defer hook.Reset()

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.True(t, cfg.Container.Privileged)
	require.Len(t, hook.Entries, 1)
	assert.Contains(t, hook.LastEntry().Message, "field volumes not found")
}

func TestContainerNetworkCreateOptions(t *testing.T) {
	// Verify that the enable_ipv4/enable_ipv6 YAML keys unmarshal into the *bool fields,
	// distinguishing an explicit true/false from an omitted key (nil). A nil here is
	// forwarded as-is to Docker, which applies its own default.
	loadOptions := func(t *testing.T, yaml string) ContainerNetworkCreateOptions {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

		cfg, err := LoadDefault(path)
		require.NoError(t, err)
		return cfg.Container.NetworkCreateOptions
	}

	t.Run("enable_ipv6 true unmarshals to non-nil true", func(t *testing.T) {
		opts := loadOptions(t, "container:\n  network_create_options:\n    enable_ipv6: true\n")
		require.NotNil(t, opts.EnableIPv6)
		assert.True(t, *opts.EnableIPv6)
	})

	t.Run("enable_ipv6 false unmarshals to non-nil false", func(t *testing.T) {
		opts := loadOptions(t, "container:\n  network_create_options:\n    enable_ipv6: false\n")
		require.NotNil(t, opts.EnableIPv6)
		assert.False(t, *opts.EnableIPv6)
	})

	t.Run("enable_ipv4 false unmarshals to non-nil false", func(t *testing.T) {
		opts := loadOptions(t, "container:\n  network_create_options:\n    enable_ipv4: false\n")
		require.NotNil(t, opts.EnableIPv4)
		assert.False(t, *opts.EnableIPv4)
	})

	t.Run("omitted keys stay nil", func(t *testing.T) {
		opts := loadOptions(t, "container:\n  network_create_options:\n    enable_ipv4: true\n")
		require.NotNil(t, opts.EnableIPv4)
		assert.True(t, *opts.EnableIPv4)
		assert.Nil(t, opts.EnableIPv6, "an omitted enable_ipv6 must remain nil so Docker's default applies")
	})

	t.Run("omitted block leaves both nil", func(t *testing.T) {
		opts := loadOptions(t, "container:\n  network: \"\"\n")
		assert.Nil(t, opts.EnableIPv4)
		assert.Nil(t, opts.EnableIPv6)
	})
}

func TestLoadDefault_ReadsExternalSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "cache.secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("  s3cr3t\n"), 0o600))

	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: true
  external_server: "http://cache.invalid/"
  external_secret_file: "`+secretPath+`"
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", cfg.Cache.ExternalSecret)
}

func TestLoadDefault_ReadsExternalSecretFromFileWhenCacheDisabled(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "cache.secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("s3cr3t"), 0o600))

	// the file has to be resolved even when cache is disabled
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: false
  external_secret_file: "`+secretPath+`"
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", cfg.Cache.ExternalSecret)
}

func TestLoadDefault_RejectsBothExternalSecretAndFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "cache.secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("s3cr3t"), 0o600))

	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: true
  external_server: "http://cache.invalid/"
  external_secret: "inline"
  external_secret_file: "`+secretPath+`"
`), 0o600))

	_, err := LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both set")
}

func TestLoadDefault_RejectsMissingExternalSecretFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: true
  external_server: "http://cache.invalid/"
  external_secret_file: "`+filepath.Join(dir, "absent.secret")+`"
`), 0o600))

	_, err := LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read cache.external_secret_file")
}

func TestLoadDefault_RejectsEmptyExternalSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "cache.secret")
	require.NoError(t, os.WriteFile(secretPath, []byte("\n  \n"), 0o600))

	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
cache:
  enabled: true
  external_server: "http://cache.invalid/"
  external_secret_file: "`+secretPath+`"
`), 0o600))

	_, err := LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains no secret")
}

// The shipped configs must parse, hold no key the config does not know, and leave
// every option at its default, as all of their values are commented out.
func TestLoadDefault_ShippedConfigsChangeNothing(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()

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
	assert.Empty(t, hook.AllEntries())
}

func TestLoadDefault_ClampsFetchTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
runner:
  fetch_timeout: 120s
`), 0o600))

	cfg, err := LoadDefault(path)
	require.NoError(t, err)
	assert.Equal(t, RequestTimeout, cfg.Runner.FetchTimeout)
}
