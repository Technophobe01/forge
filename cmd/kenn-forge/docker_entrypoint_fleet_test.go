package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
)

const testFleetOrigin = "https://hub.example.test:8448"

// standaloneConfig is a config the entrypoint seeded earlier for a forge served
// under a sub-path, then edited by forge itself: repos, tables, and a one-line
// allowed_hosts array, the shape a running instance has when it becomes a hub.
const standaloneConfig = `sync_interval = "5m"
host = "127.0.0.1"
port = 8092
base_path = "/team/forge/"
allowed_hosts = ["127.0.0.1:8091", "localhost:8091", "forge-team:8091"]

[[repos]]
  owner = "example-org"
  name = "widget"

[modes]
  pulls = true
  workspaces = true
`

// runEntrypointFailing runs the entrypoint in seed-only mode, requires it to
// fail, and returns its combined output.
func runEntrypointFailing(t *testing.T, home string, env map[string]string) string {
	t.Helper()
	cmd := exec.Command("sh", entrypointScript(t))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"MIDDLEMAN_SEED_ONLY=1",
		"MIDDLEMAN_HOME=" + home,
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	require.Errorf(t, err, "entrypoint should have failed: %s", out)
	return string(out)
}

func writeConfig(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func preFleetBackups(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "config.toml.pre-fleet.*"))
	require.NoError(t, err)
	return matches
}

func TestDockerEntrypointSeedsAFleetHub(t *testing.T) {
	assert := assert.New(t)
	cfg := seedConfig(t, t.TempDir(), map[string]string{
		"MIDDLEMAN_PORT":          "18091",
		"MIDDLEMAN_INTERNAL_PORT": "18092",
		"MIDDLEMAN_FLEET_ORIGIN":  testFleetOrigin + "/",
	})
	assert.True(cfg.Fleet.Enabled)
	assert.Equal(config.FleetRoleHub, cfg.Fleet.Role)
	assert.Equal(testFleetOrigin, cfg.Fleet.BaseURL)
	assert.Equal("/", cfg.BasePath)
	assert.True(cfg.API.RequireAuth)
	assert.Contains(cfg.AllowedHosts, "hub.example.test:8448")
	assert.Contains(cfg.AllowedHosts, "127.0.0.1:18091", "the healthcheck host stays allowed")
	assert.Equal(18092, cfg.Port)
}

func TestDockerEntrypointMakesAStandaloneConfigAHubOnce(t *testing.T) {
	assert := assert.New(t)
	home := t.TempDir()
	path := writeConfig(t, home, standaloneConfig)
	env := map[string]string{"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin}

	cfg := seedConfig(t, home, env)
	assert.True(cfg.Fleet.Enabled)
	assert.Equal(config.FleetRoleHub, cfg.Fleet.Role)
	assert.Equal(testFleetOrigin, cfg.Fleet.BaseURL)
	assert.Equal("/", cfg.BasePath)
	assert.True(cfg.API.RequireAuth)
	assert.Equal([]string{
		"127.0.0.1:8091", "localhost:8091", "forge-team:8091", "hub.example.test:8448",
	}, cfg.AllowedHosts)
	require.Len(t, cfg.Repos, 1, "the rest of the config is kept")
	assert.Equal("widget", cfg.Repos[0].Name)
	assert.Equal(8092, cfg.Port)

	backups := preFleetBackups(t, home)
	require.Len(t, backups, 1)
	kept, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(standaloneConfig, string(kept), "the backup is the config as it was")

	converted, err := os.ReadFile(path)
	require.NoError(t, err)
	seedConfig(t, home, env)
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(string(converted), string(again), "a hub config is not edited again")
	assert.Len(preFleetBackups(t, home), 1, "and is not backed up again")
}

func TestDockerEntrypointTurnsOnAuthInAnExistingAPITable(t *testing.T) {
	home := t.TempDir()
	writeConfig(t, home, "host = \"127.0.0.1\"\nport = 8092\n\n[api]\n  require_auth = false\n")
	cfg := seedConfig(t, home, map[string]string{"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin})
	assert.True(t, cfg.API.RequireAuth)
	assert.Contains(t, cfg.AllowedHosts, "hub.example.test:8448")
}

func TestDockerEntrypointLeavesAnExistingFleetToForge(t *testing.T) {
	home := t.TempDir()
	body := "host = \"127.0.0.1\"\nport = 8092\nallowed_hosts = [\"hub.example.test:8448\"]\n\n" +
		"[api]\n  require_auth = true\n\n" +
		"[fleet]\n  enabled = true\n  role = \"hub\"\n  base_url = \"https://elsewhere.example.test\"\n"
	path := writeConfig(t, home, body)
	out := runEntrypoint(t, home, map[string]string{"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin})
	assert.Contains(t, out, "base_url is not "+testFleetOrigin)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, body, string(after))
	assert.Empty(t, preFleetBackups(t, home))
}

func TestDockerEntrypointRefusesAFleetHubItCannotMake(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"sub-path":  {"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin, "MIDDLEMAN_BASE_PATH": "/team/forge"},
		"cleartext": {"MIDDLEMAN_FLEET_ORIGIN": "http://hub.example.test:8448"},
		"path":      {"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin + "/forge"},
		"userinfo":  {"MIDDLEMAN_FLEET_ORIGIN": "https://user@hub.example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			runEntrypointFailing(t, home, env)
			_, err := os.Stat(filepath.Join(home, "config.toml"))
			assert.Truef(t, os.IsNotExist(err), "nothing is seeded (stat err: %v)", err)
		})
	}

	t.Run("multi-line allowed_hosts", func(t *testing.T) {
		home := t.TempDir()
		body := "host = \"127.0.0.1\"\nport = 8092\nallowed_hosts = [\n  \"127.0.0.1:8091\",\n]\n"
		path := writeConfig(t, home, body)
		out := runEntrypointFailing(t, home, map[string]string{"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin})
		assert.Contains(t, out, "spans several lines")
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, body, string(after), "the config is left as it was")
		assert.Empty(t, preFleetBackups(t, home))
	})
}
