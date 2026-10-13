package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
)

// standalone is a config the image seeded for a forge served under a
// sub-path behind a forwarding proxy, then grew: repos, tables, and a comment
// with brackets after allowed_hosts, which no line edit could be trusted with.
const standalone = `sync_interval = "5m"
host = "127.0.0.1"
port = 8092
base_path = "/team/forge/"
allowed_hosts = ["127.0.0.1:8091", "localhost:8091", "forge-team:8091"] # [::1] not used
trust_reverse_proxy = true

[[repos]]
  owner = "example-org"
  name = "widget"

[modes]
  pulls = true
  workspaces = true
`

var fixedNow = func() time.Time { return time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC) }

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestMakeHubConvertsAStandaloneConfig(t *testing.T) {
	assert := assert.New(t)
	path := writeConfig(t, standalone)
	var log bytes.Buffer
	require.NoError(t, makeHub(path, "https://Hub.Example.test:8448/", fixedNow, &log))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.True(cfg.Fleet.Enabled)
	assert.Equal(config.FleetRoleHub, cfg.Fleet.Role)
	assert.Equal("https://hub.example.test:8448", cfg.Fleet.BaseURL)
	assert.Equal("/", cfg.BasePath)
	assert.True(cfg.API.RequireAuth)
	assert.False(cfg.TrustReverseProxy, "a hub validates the Host its peers send, as setup leaves it")
	assert.Equal([]string{
		"127.0.0.1:8091", "localhost:8091", "forge-team:8091", "hub.example.test:8448",
	}, cfg.AllowedHosts)
	assert.Equal("127.0.0.1", cfg.Host)
	assert.Equal(8092, cfg.Port, "the listener the entrypoint bridges is left alone")
	require.Len(t, cfg.Repos, 1)
	assert.Equal("widget", cfg.Repos[0].Name)

	backup := path + ".pre-fleet.20261012T190000Z"
	kept, err := os.ReadFile(backup)
	require.NoError(t, err)
	assert.Equal(standalone, string(kept))
	assert.Contains(log.String(), "is now a fleet hub at https://hub.example.test:8448")
}

func TestMakeHubDropsTheDefaultPort(t *testing.T) {
	path := writeConfig(t, standalone)
	require.NoError(t, makeHub(path, "https://hub.example.test:443", fixedNow, &bytes.Buffer{}))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "https://hub.example.test", cfg.Fleet.BaseURL)
	assert.Contains(t, cfg.AllowedHosts, "hub.example.test", "the Host a peer sends for :443")
}

func TestMakeHubLeavesAnEnrolledFleetAlone(t *testing.T) {
	path := writeConfig(t, standalone)
	require.NoError(t, makeHub(path, "https://hub.example.test:8448", fixedNow, &bytes.Buffer{}))
	converted, err := os.ReadFile(path)
	require.NoError(t, err)

	var log bytes.Buffer
	later := func() time.Time { return fixedNow().Add(time.Hour) }
	require.NoError(t, makeHub(path, "https://elsewhere.example.test", later, &log))
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(converted), string(again))
	assert.Contains(t, log.String(), "already in a fleet at https://hub.example.test:8448")
	matches, err := filepath.Glob(path + ".pre-fleet.*")
	require.NoError(t, err)
	assert.Len(t, matches, 1, "no second backup")

	log.Reset()
	require.NoError(t, makeHub(path, "https://hub.example.test:8448", later, &log))
	assert.Empty(t, log.String(), "the same origin again is quiet")
}

func TestMakeHubRefusesWhatItCannotMake(t *testing.T) {
	for name, origin := range map[string]string{
		"cleartext": "http://hub.example.test",
		"path":      "https://hub.example.test/forge",
		"userinfo":  "https://user@hub.example.test",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, standalone)
			require.Error(t, makeHub(path, origin, fixedNow, &bytes.Buffer{}))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, standalone, string(after))
			matches, err := filepath.Glob(path + ".*")
			require.NoError(t, err)
			assert.Empty(t, matches, "no backup or temporary file is left")
		})
	}

	t.Run("unloadable config", func(t *testing.T) {
		path := writeConfig(t, "host = \"127.0.0.1\"\nport = \"not a number\"\n")
		require.Error(t, makeHub(path, "https://hub.example.test", fixedNow, &bytes.Buffer{}))
		matches, err := filepath.Glob(path + ".*")
		require.NoError(t, err)
		assert.Empty(t, matches)
	})

	t.Run("missing config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		err := makeHub(path, "https://hub.example.test", fixedNow, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "load")
	})
}

func TestMakeHubTurnsOnAuthInAnExistingAPITable(t *testing.T) {
	path := writeConfig(t, "host = \"127.0.0.1\"\nport = 8092\n\n[api]\n  require_auth = false\n")
	require.NoError(t, makeHub(path, "https://hub.example.test", fixedNow, &bytes.Buffer{}))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.True(t, cfg.API.RequireAuth)
	assert.Equal(t, []string{"hub.example.test"}, cfg.AllowedHosts)
}

func TestMakeHubLeavesAFleetTurnedOffAlone(t *testing.T) {
	path := writeConfig(t, standalone)
	require.NoError(t, makeHub(path, "https://hub.example.test:8448", fixedNow, &bytes.Buffer{}))
	// Turn federation off as Settings does: the rest of [fleet] stays.
	cfg, err := config.Load(path)
	require.NoError(t, err)
	cfg.Fleet.Enabled = false
	require.NoError(t, cfg.Save(path))
	off, err := os.ReadFile(path)
	require.NoError(t, err)

	var log bytes.Buffer
	require.NoError(t, makeHub(path, "https://hub.example.test:8448", func() time.Time {
		return fixedNow().Add(time.Hour)
	}, &log))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(off), string(after), "federation stays off")
	assert.Contains(t, log.String(), "federation turned off")
	matches, err := filepath.Glob(path + ".pre-fleet.*")
	require.NoError(t, err)
	assert.Len(t, matches, 1, "no second backup")
}

func TestMakeHubDropsTailscaleServeIdentity(t *testing.T) {
	path := writeConfig(t, "host = \"127.0.0.1\"\nport = 8092\n\n[api]\n  require_auth = true\n"+
		"  [api.tailscale_serve]\n    enabled = true\n    allowed_users = [\"user@example.test\"]\n")
	var log bytes.Buffer
	require.NoError(t, makeHub(path, "https://hub.example.test", fixedNow, &log))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.False(t, cfg.API.TailscaleServe.Enabled, "every peer is loopback behind the bridge")
	assert.Empty(t, cfg.API.TailscaleServe.AllowedUsers)
	assert.True(t, cfg.API.RequireAuth)
	assert.Contains(t, log.String(), "trusted Tailscale Serve identities")
}
