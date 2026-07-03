// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestLoadDefault_DefaultsWorkdirCleanupAge(t *testing.T) {
	cfg, err := LoadDefault("")
	require.NoError(t, err)
	assert.Equal(t, 24*time.Hour, cfg.Runner.WorkdirCleanupAge)
	assert.Equal(t, 10*time.Minute, cfg.Runner.IdleCleanupInterval)
}

func TestLoadDefault_DefaultsIdleFetchBackoffMax(t *testing.T) {
	cfg, err := LoadDefault("")
	require.NoError(t, err)
	assert.Equal(t, time.Minute, cfg.Runner.FetchIntervalMax)
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

// TestLoadDefault_MalformedYAMLReturnsParseError pins the error surfaced for
// invalid YAML to the canonical "parse config file" message rather than the
// "for defaults metadata" variant — i.e. the main yaml.Unmarshal runs first.
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

func TestLoadDefault_MalformedYAMLReturnsParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("runner:\n  capacity: [unterminated\n"), 0o600))

	_, err := LoadDefault(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config file")
	assert.NotContains(t, err.Error(), "defaults metadata")
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
