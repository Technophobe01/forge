package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
)

const testFleetOrigin = "https://hub.example.test:8448"

// standaloneConfig is a config the entrypoint seeded earlier for a forge served
// under a sub-path, the shape a running instance has when it becomes a hub.
const standaloneConfig = `sync_interval = "5m"
host = "127.0.0.1"
port = 8092
base_path = "/team/forge/"
allowed_hosts = ["127.0.0.1:8091", "localhost:8091", "forge-team:8091"]

[[repos]]
  owner = "example-org"
  name = "widget"
`

var (
	fleetHubOnce sync.Once
	fleetHubDir  string
	fleetHubErr  error
)

// fleetHubPath builds the image's kenn-forge-fleet-hub once and returns a PATH
// that finds it first, as the image's /usr/local/bin does.
func fleetHubPath(t *testing.T) string {
	t.Helper()
	fleetHubOnce.Do(func() {
		fleetHubDir, fleetHubErr = os.MkdirTemp("", "fleethub-bin")
		if fleetHubErr != nil {
			return
		}
		out, err := exec.Command("go", "build", "-o",
			filepath.Join(fleetHubDir, "kenn-forge-fleet-hub"), "go.kenn.io/forge/docker/fleethub").CombinedOutput()
		if err != nil {
			fleetHubErr = err
			t.Logf("build kenn-forge-fleet-hub: %s", out)
		}
	})
	require.NoError(t, fleetHubErr, "build kenn-forge-fleet-hub")
	return fleetHubDir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// fleetEnv is env plus the PATH that finds kenn-forge-fleet-hub. runEntrypoint
// sets PATH first; a later duplicate key wins for exec.
func fleetEnv(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{"PATH": fleetHubPath(t)}
	for k, v := range env {
		out[k] = v
	}
	return out
}

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

func preFleetBackups(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "config.toml.pre-fleet.*"))
	require.NoError(t, err)
	return matches
}

func TestDockerEntrypointSeedsAFleetHub(t *testing.T) {
	assert := assert.New(t)
	cfg := seedConfig(t, t.TempDir(), fleetEnv(t, map[string]string{
		"MIDDLEMAN_PORT":          "18091",
		"MIDDLEMAN_INTERNAL_PORT": "18092",
		"MIDDLEMAN_FLEET_ORIGIN":  testFleetOrigin,
	}))
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
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(standaloneConfig), 0o600))
	env := fleetEnv(t, map[string]string{"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin})

	cfg := seedConfig(t, home, env)
	assert.True(cfg.Fleet.Enabled)
	assert.Equal("/", cfg.BasePath)
	assert.Contains(cfg.AllowedHosts, "forge-team:8091")
	assert.Contains(cfg.AllowedHosts, "hub.example.test:8448")
	require.Len(t, cfg.Repos, 1)

	backups := preFleetBackups(t, home)
	require.Len(t, backups, 1)
	kept, err := os.ReadFile(backups[0])
	require.NoError(t, err)
	assert.Equal(standaloneConfig, string(kept))

	converted, err := os.ReadFile(path)
	require.NoError(t, err)
	seedConfig(t, home, env)
	again, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(string(converted), string(again), "a hub config is not edited again")
	assert.Len(preFleetBackups(t, home), 1)
}

func TestDockerEntrypointRefusesAFleetHubItCannotMake(t *testing.T) {
	t.Run("sub-path", func(t *testing.T) {
		home := t.TempDir()
		out := runEntrypointFailing(t, home, fleetEnv(t, map[string]string{
			"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin,
			"MIDDLEMAN_BASE_PATH":    "/team/forge",
		}))
		assert.Contains(t, out, "serves at the root path")
		_, err := os.Stat(filepath.Join(home, "config.toml"))
		assert.Truef(t, os.IsNotExist(err), "nothing is seeded (stat err: %v)", err)
	})

	t.Run("bad origin", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(standaloneConfig), 0o600))
		runEntrypointFailing(t, home, fleetEnv(t, map[string]string{
			"MIDDLEMAN_FLEET_ORIGIN": "http://hub.example.test:8448",
		}))
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, standaloneConfig, string(after))
		assert.Empty(t, preFleetBackups(t, home))
	})

	t.Run("helper missing", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(standaloneConfig), 0o600))
		out := runEntrypointFailing(t, home, map[string]string{
			"PATH":                   "/usr/bin:/bin",
			"MIDDLEMAN_FLEET_ORIGIN": testFleetOrigin,
		})
		assert.Contains(t, out, "kenn-forge-fleet-hub is not installed")
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, standaloneConfig, string(after))
	})
}
