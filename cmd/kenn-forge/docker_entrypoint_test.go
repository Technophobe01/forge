package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/kata"
)

// entrypointScript returns the absolute path to the production docker entrypoint,
// resolved from this test file's location so it works regardless of CWD.
func entrypointScript(t *testing.T) string {
	t.Helper()
	// The container entrypoint is a POSIX sh script for a Linux image; on
	// Windows there may be no sh, and a temp path would not survive the TOML.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX container entrypoint")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", "docker-entrypoint.sh")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("entrypoint script not found at %s: %v", p, err)
	}
	return p
}

// seedConfig runs docker-entrypoint.sh in seed-only mode with the given env and
// loads the generated config through the real loader. The returned config is
// what the server itself would see, so a bad seeded key/value fails the test
// here rather than silently breaking a container at startup.
func seedConfig(t *testing.T, home string, env map[string]string) *config.Config {
	t.Helper()
	runEntrypoint(t, home, env)
	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	require.NoErrorf(t, err, "generated config did not load")
	return cfg
}

func TestDockerEntrypointSeedsLoadableConfig(t *testing.T) {
	t.Run("defaults bind loopback with synonym allowlist", func(t *testing.T) {
		assert := assert.New(t)
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":          "18091",
			"MIDDLEMAN_INTERNAL_PORT": "18092",
		})
		assert.Equal("127.0.0.1", cfg.Host)
		assert.Equal(18092, cfg.Port) // the daemon binds the internal loopback port
		assert.Contains(cfg.AllowedHosts, "127.0.0.1:18091")
		assert.Contains(cfg.AllowedHosts, "localhost:18091")
		assert.False(cfg.TrustReverseProxy)
	})

	t.Run("operator allowed hosts are merged", func(t *testing.T) {
		assert := assert.New(t)
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":          "18091",
			"MIDDLEMAN_INTERNAL_PORT": "18092",
			"MIDDLEMAN_ALLOWED_HOSTS": "host.example:18091, other.lan",
		})
		assert.Contains(cfg.AllowedHosts, "127.0.0.1:18091") // synonyms still present
		assert.Contains(cfg.AllowedHosts, "host.example:18091")
		assert.Contains(cfg.AllowedHosts, "other.lan")
		assert.False(cfg.TrustReverseProxy)
	})

	t.Run("base path prefix is seeded", func(t *testing.T) {
		assert := assert.New(t)
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":      "18091",
			"MIDDLEMAN_BASE_PATH": "/middleman",
		})
		// config.Load normalizes a non-root prefix to a trailing slash.
		assert.Equal("/middleman/", cfg.BasePath)
	})

	t.Run("no base path defaults to root", func(t *testing.T) {
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT": "18091",
		})
		assert.Equal(t, "/", cfg.BasePath)
	})

	t.Run("hostile base path still produces valid TOML", func(t *testing.T) {
		assert := assert.New(t)
		// Quotes/backslashes must not break the generated TOML; config.Load
		// succeeding (inside seedConfig) is the core assertion.
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":      "18091",
			"MIDDLEMAN_BASE_PATH": "/ev\"il\\path",
		})
		assert.NotContains(cfg.BasePath, "\"")
		assert.NotContains(cfg.BasePath, "\\")
	})

	t.Run("trust_reverse_proxy only for truthy values", func(t *testing.T) {
		truthy := []string{"1", "true", "TRUE", "yes"}
		for _, v := range truthy {
			cfg := seedConfig(t, t.TempDir(), map[string]string{
				"MIDDLEMAN_ALLOWED_HOSTS":       "proxy.example",
				"MIDDLEMAN_TRUST_REVERSE_PROXY": v,
			})
			assert.Truef(t, cfg.TrustReverseProxy, "value %q should enable trust", v)
		}
		for _, v := range []string{"", "0", "false", "no"} {
			cfg := seedConfig(t, t.TempDir(), map[string]string{
				"MIDDLEMAN_ALLOWED_HOSTS":       "proxy.example",
				"MIDDLEMAN_TRUST_REVERSE_PROXY": v,
			})
			assert.Falsef(t, cfg.TrustReverseProxy, "value %q should not enable trust", v)
		}
	})

	t.Run("hostile allowed_hosts still produce valid TOML", func(t *testing.T) {
		assert := assert.New(t)
		// Quotes, backslashes, and a newline must not break the generated TOML;
		// they are stripped, leaving bare host tokens. config.Load succeeding is
		// the core assertion (a broken file would error).
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":          "18091",
			"MIDDLEMAN_ALLOWED_HOSTS": "ev\"il, na\\me, line\nbreak",
		})
		assert.Contains(cfg.AllowedHosts, "evil")
		assert.Contains(cfg.AllowedHosts, "name")
		// No residual quote/backslash survived into any entry.
		for _, h := range cfg.AllowedHosts {
			assert.NotContains(h, "\"")
			assert.NotContains(h, "\\")
		}
	})

	t.Run("existing config is not overwritten", func(t *testing.T) {
		home := t.TempDir()
		sentinel := "host = \"127.0.0.1\"\nport = 9999\n"
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(sentinel), 0o600))
		cfg := seedConfig(t, home, map[string]string{
			"MIDDLEMAN_PORT":          "18091",
			"MIDDLEMAN_INTERNAL_PORT": "18092",
		})
		assert.Equal(t, 9999, cfg.Port, "entrypoint must not clobber an existing config")
		assert.False(t, slices.Contains(cfg.AllowedHosts, "127.0.0.1:18091"))
	})
}

// seedKataCatalog runs the entrypoint (which seeds both the middleman config and
// the kata daemon catalog at $KATA_HOME/config.toml) and reads the catalog back
// through the real kata loader. A bad seeded daemon entry fails here rather than
// silently breaking the front-door kata proxy at container startup.
func seedKataCatalog(t *testing.T, home string, env map[string]string) kata.Catalog {
	t.Helper()
	seedConfig(t, home, env) // also asserts the middleman config itself loads
	t.Setenv("KATA_HOME", filepath.Join(home, "kata"))
	cat, err := kata.LoadCatalog()
	require.NoErrorf(t, err, "seeded kata catalog did not load")
	return cat
}

// runEntrypoint runs docker-entrypoint.sh in seed-only mode and returns its
// combined output, for asserting operator warnings and that files are (not) written.
func runEntrypoint(t *testing.T, home string, env map[string]string) string {
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
	require.NoErrorf(t, err, "entrypoint failed: %s", out)
	return string(out)
}

func TestDockerEntrypointSeedsProxyTargets(t *testing.T) {
	t.Run("roborev endpoint is seeded and loads", func(t *testing.T) {
		assert := assert.New(t)
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":             "18091",
			"MIDDLEMAN_INTERNAL_PORT":    "18092",
			"MIDDLEMAN_ROBOREV_ENDPOINT": "http://roborev:7373",
		})
		assert.Equal("http://roborev:7373", cfg.Roborev.Endpoint)
		assert.Equal("http://roborev:7373", cfg.RoborevEndpoint())
	})

	t.Run("no roborev endpoint leaves the panel unconfigured", func(t *testing.T) {
		assert := assert.New(t)
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":          "18091",
			"MIDDLEMAN_INTERNAL_PORT": "18092",
		})
		assert.Empty(cfg.Roborev.Endpoint)
	})

	t.Run("hostile roborev endpoint still produces valid TOML", func(t *testing.T) {
		assert := assert.New(t)
		// Quotes/backslashes/newlines must not break the [roborev] table;
		// config.Load succeeding (inside seedConfig) is the core assertion.
		cfg := seedConfig(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":             "18091",
			"MIDDLEMAN_ROBOREV_ENDPOINT": "http://ev\"il\\:7373\nbreak",
		})
		assert.NotEmpty(cfg.Roborev.Endpoint)
		assert.NotContains(cfg.Roborev.Endpoint, "\"")
		assert.NotContains(cfg.Roborev.Endpoint, "\\")
		assert.NotContains(cfg.Roborev.Endpoint, "\n")
	})

	t.Run("kata catalog is seeded and loads", func(t *testing.T) {
		assert := assert.New(t)
		cat := seedKataCatalog(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":     "18091",
			"MIDDLEMAN_KATA_URL": "http://kata:7777",
			"KATA_AUTH_TOKEN":    "test-token",
		})
		require.Len(t, cat.Daemons, 1)
		d := cat.Daemons[0]
		assert.Equal("http://kata:7777", d.URL)
		assert.Equal("KATA_AUTH_TOKEN", d.TokenEnv)
		assert.True(d.Default, "seeded daemon should be the active_daemon")
		assert.True(d.AllowInsecure)
		assert.Empty(d.Token, "catalog carries token_env, never the secret itself")
	})

	t.Run("hostile kata url still produces a loadable catalog", func(t *testing.T) {
		assert := assert.New(t)
		// LoadCatalog succeeding (inside seedKataCatalog) is the core assertion.
		cat := seedKataCatalog(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":     "18091",
			"MIDDLEMAN_KATA_URL": "http://ka\"ta\\:7777\nbreak",
			"KATA_AUTH_TOKEN":    "test-token",
		})
		require.Len(t, cat.Daemons, 1)
		u := cat.Daemons[0].URL
		assert.NotEmpty(u)
		assert.NotContains(u, "\"")
		assert.NotContains(u, "\\")
		assert.NotContains(u, "\n")
	})

	t.Run("kata url without auth token warns", func(t *testing.T) {
		// A catalog with no usable token would fail the proxy at runtime; the
		// entrypoint must surface that at seed time, not silently.
		out := runEntrypoint(t, t.TempDir(), map[string]string{
			"MIDDLEMAN_PORT":     "18091",
			"MIDDLEMAN_KATA_URL": "http://kata:7777",
			// KATA_AUTH_TOKEN intentionally unset
		})
		assert.Contains(t, out, "KATA_AUTH_TOKEN is empty")
	})

	t.Run("no kata url writes no catalog", func(t *testing.T) {
		home := t.TempDir()
		runEntrypoint(t, home, map[string]string{
			"MIDDLEMAN_PORT": "18091",
			// MIDDLEMAN_KATA_URL intentionally unset
		})
		_, err := os.Stat(filepath.Join(home, "kata", "config.toml"))
		assert.Truef(t, os.IsNotExist(err),
			"no kata catalog should be written without MIDDLEMAN_KATA_URL (stat err: %v)", err)
	})
}

// TestDockerEntrypointStartsKennForgeOnTheMiddlemanVolume pins the rename:
// forge reads none of the MIDDLEMAN_* variables, so without KENN_FORGE_HOME it
// would ignore the seeded /data/config.toml and start empty under
// /data/.kenn/forge, losing base_path and with it the Console route.
func TestDockerEntrypointStartsKennForgeOnTheMiddlemanVolume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX entrypoint")
	}
	home := t.TempDir()
	bin := t.TempDir()
	record := filepath.Join(t.TempDir(), "kenn-forge.calls")
	// A stub server that records how it was started, and a socat that just
	// waits to be stopped.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kenn-forge"), []byte(
		"#!/bin/sh\n"+
			"echo \"args=$*\" >> "+record+"\n"+
			"echo \"KENN_FORGE_HOME=${KENN_FORGE_HOME:-}\" >> "+record+"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "socat"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))

	cmd := exec.Command("sh", entrypointScript(t))
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"MIDDLEMAN_HOME=" + home,
		"MIDDLEMAN_PORT=18091",
		"MIDDLEMAN_INTERNAL_PORT=18092",
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "entrypoint failed: %s", out)

	calls, err := os.ReadFile(record)
	require.NoError(t, err, "kenn-forge was never started")
	assert.Contains(t, string(calls), "KENN_FORGE_HOME="+home)
	assert.Contains(t, string(calls), "args=serve")
}

// TestLegacyMiddlemanVolumeMigratesInPlace is what the image relies on: with
// KENN_FORGE_HOME on the volume and the seeded data_dir, forge's first start
// renames middleman.db to forge.db where it lies. The rename is one way, so an
// upgrade snapshots the volume first.
func TestLegacyMiddlemanVolumeMigratesInPlace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KENN_FORGE_HOME", home)
	seedConfig(t, home, map[string]string{"MIDDLEMAN_INTERNAL_PORT": "18092"})
	require.NoError(t, os.WriteFile(filepath.Join(home, "middleman.db"), []byte("legacy"), 0o600))

	_, err := config.LoadOrCreate(filepath.Join(home, "config.toml"))
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(home, "forge.db"))
	require.NoError(t, err, "middleman.db was not carried over")
	assert.Equal(t, "legacy", string(got))
	assert.NoFileExists(t, filepath.Join(home, "middleman.db"))
}

// TestDockerEntrypointLetsKennForgeFinishShuttingDown: on docker stop the
// shell's wait returns as soon as the trapped TERM arrives. If the entrypoint
// then exits, it is PID 1 and the kernel kills kenn-forge mid-shutdown,
// cutting off its writes to a database it may have just migrated.
func TestDockerEntrypointLetsKennForgeFinishShuttingDown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX entrypoint")
	}
	home, bin, marks := t.TempDir(), t.TempDir(), t.TempDir()
	started := filepath.Join(marks, "serve-started")
	done := filepath.Join(marks, "shutdown-finished")
	socatTermed := filepath.Join(marks, "socat-got-term")
	socatStopped := filepath.Join(marks, "socat-stopped-while-draining")
	// The server says when it is up, which is after the entrypoint installed
	// its trap; when it finishes draining it notes whether the bridge had
	// already been told to stop.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kenn-forge"), []byte(
		"#!/bin/sh\n"+
			"[ \"$1\" = serve ] || exit 0\n"+
			"trap 'sleep 1; [ -e "+socatTermed+" ] && touch "+socatStopped+"; touch "+done+"; exit 0' TERM\n"+
			"touch "+started+"\n"+
			"while :; do sleep 0.1; done\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "socat"), []byte(
		"#!/bin/sh\n"+
			"trap 'touch "+socatTermed+"; exit 0' TERM\n"+
			"while :; do sleep 0.1; done\n"), 0o755))

	cmd := exec.Command("sh", entrypointScript(t))
	cmd.Env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"MIDDLEMAN_HOME=" + home,
		"MIDDLEMAN_PORT=18091",
		"MIDDLEMAN_INTERNAL_PORT=18092",
	}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil },
		10*time.Second, 20*time.Millisecond, "kenn-forge serve never started")
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	_ = cmd.Wait()

	assert.FileExists(t, done, "the entrypoint exited before kenn-forge finished shutting down")
	assert.NoFileExists(t, socatStopped, "the bridge was stopped while kenn-forge was still draining")
}

// TestDockerEntrypointKeepsTheMiddlemanDatabaseBeforeForgeRenamesIt: forge's
// first start renames middleman.db to forge.db, and nothing renames it back,
// so the entrypoint keeps a copy an operator can roll back to.
func TestDockerEntrypointKeepsTheMiddlemanDatabaseBeforeForgeRenamesIt(t *testing.T) {
	home := t.TempDir()
	for name, body := range map[string]string{"middleman.db": "db", "middleman.db-wal": "wal"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte(body), 0o600))
	}
	runEntrypoint(t, home, nil)

	backups := preForgeBackups(t, home)
	require.Len(t, backups, 1, "one backup")
	assertFile(t, filepath.Join(backups[0], "middleman.db"), "db")
	assertFile(t, filepath.Join(backups[0], "middleman.db-wal"), "wal")

	// Once forge has migrated, there is nothing left to keep.
	require.NoError(t, os.Rename(filepath.Join(home, "middleman.db"), filepath.Join(home, "forge.db")))
	require.NoError(t, os.Remove(filepath.Join(home, "middleman.db-wal")))
	runEntrypoint(t, home, nil)
	assert.Len(t, preForgeBackups(t, home), 1, "no new backup once migrated")
}

// TestDockerEntrypointBridgesThePortOfTheConfigServeUses: a --config passed
// through to serve must also be the one the bridge reads its port from, or
// the published port forwards to a listener nothing is on.
func TestDockerEntrypointBridgesThePortOfTheConfigServeUses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX entrypoint")
	}
	home, bin := t.TempDir(), t.TempDir()
	record := filepath.Join(t.TempDir(), "calls")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kenn-forge"), []byte(
		"#!/bin/sh\necho \"$*\" >> "+record+"\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "socat"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	custom := filepath.Join(t.TempDir(), "custom.toml")

	for _, args := range [][]string{{"--config", custom}, {"--config=" + custom}} {
		require.NoError(t, os.WriteFile(record, nil, 0o600))
		cmd := exec.Command("sh", append([]string{entrypointScript(t)}, args...)...)
		cmd.Env = []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"MIDDLEMAN_HOME=" + home,
		}
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "entrypoint failed: %s", out)
		calls, err := os.ReadFile(record)
		require.NoError(t, err)
		assert.Contains(t, string(calls), "config read --config "+custom+" port", args)
	}
}

// preForgeBackups lists the backup directories the entrypoint published.
func preForgeBackups(t *testing.T, dir string) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(dir, "middleman.db.pre-forge.*"))
	require.NoError(t, err)
	return all
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err, path)
	assert.Equal(t, want, string(got), path)
}

// An interrupted copy (disk full, a killed container) leaves only a temporary
// directory, which must not pass for a backup.
func TestDockerEntrypointDoesNotTakeAnInterruptedCopyForABackup(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "middleman.db"), []byte("db"), 0o600))
	partial := filepath.Join(home, ".pre-forge-tmp.123")
	require.NoError(t, os.MkdirAll(partial, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(partial, "middleman.db"), []byte("d"), 0o600))

	runEntrypoint(t, home, nil)

	backups := preForgeBackups(t, home)
	require.Len(t, backups, 1, "a complete backup is taken despite the leftover")
	assertFile(t, filepath.Join(backups[0], "middleman.db"), "db")
	assert.NoDirExists(t, partial, "the leftover of the interrupted copy is cleared")
}

// Forge migrates the data_dir of the config serve uses; that is the database
// to keep, wherever it lives. No stub here: kenn-forge's own `config read`
// knows only the port, so the entrypoint must find data_dir itself.
func TestDockerEntrypointBacksUpTheDataDirForgeMigrates(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "middleman.db"), []byte("db"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("host = \"127.0.0.1\"\nport = 8092\ndata_dir = \""+other+"\"\n"), 0o600))

	runEntrypoint(t, home, nil)

	assert.Len(t, preForgeBackups(t, other), 1, "the configured data_dir is backed up")
	assert.Empty(t, preForgeBackups(t, home))
}

// After a rollback the old image writes to middleman.db again; the next
// upgrade must keep that newer database, not trust the first backup.
func TestDockerEntrypointBacksUpADatabaseNewerThanItsLastBackup(t *testing.T) {
	home := t.TempDir()
	db := filepath.Join(home, "middleman.db")
	require.NoError(t, os.WriteFile(db, []byte("v1"), 0o600))
	runEntrypoint(t, home, nil)
	require.Len(t, preForgeBackups(t, home), 1)

	runEntrypoint(t, home, nil)
	require.Len(t, preForgeBackups(t, home), 1, "an unchanged database keeps its one backup")

	require.NoError(t, os.WriteFile(db, []byte("v2"), 0o600))
	later := time.Now().Add(2 * time.Minute)
	require.NoError(t, os.Chtimes(db, later, later))
	runEntrypoint(t, home, nil)
	assert.Len(t, preForgeBackups(t, home), 2, "the newer database gets its own backup")
}

// Two backups in the same second must not land on one name: the second,
// without a WAL, would otherwise leave the first one's WAL beside its own
// database, a set that cannot be restored.
func TestDockerEntrypointKeepsBackupsFromTheSameSecondApart(t *testing.T) {
	home := t.TempDir()
	db, wal := filepath.Join(home, "middleman.db"), filepath.Join(home, "middleman.db-wal")
	require.NoError(t, os.WriteFile(db, []byte("v1"), 0o600))
	require.NoError(t, os.WriteFile(wal, []byte("w1"), 0o600))
	runEntrypoint(t, home, nil)

	require.NoError(t, os.WriteFile(db, []byte("v2"), 0o600))
	later := time.Now().Add(2 * time.Minute)
	require.NoError(t, os.Chtimes(db, later, later))
	require.NoError(t, os.Remove(wal))
	runEntrypoint(t, home, nil)

	backups := preForgeBackups(t, home)
	require.Len(t, backups, 2)
	sets := map[string]bool{}
	for _, b := range backups {
		body, err := os.ReadFile(filepath.Join(b, "middleman.db"))
		require.NoError(t, err)
		_, walErr := os.Stat(filepath.Join(b, "middleman.db-wal"))
		sets[string(body)+"+wal="+fmt.Sprint(walErr == nil)] = true
	}
	assert.Equal(t, map[string]bool{"v1+wal=true": true, "v2+wal=false": true}, sets)
}

// A middleman still holding the database (two containers on one volume) must
// not be copied mid-write; forge would refuse to migrate it anyway.
func TestDockerEntrypointWillNotCopyADatabaseInUse(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("needs util-linux flock, as in the image")
	}
	home := t.TempDir()
	lock := filepath.Join(home, "middleman.lock")
	require.NoError(t, os.WriteFile(filepath.Join(home, "middleman.db"), []byte("db"), 0o600))
	// -o: the lock stays with flock itself, so killing it releases the lock
	// rather than leaving it with an orphaned child.
	holder := exec.Command("flock", "-o", lock, "sleep", "30")
	require.NoError(t, holder.Start())
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	require.Eventually(t, func() bool {
		return exec.Command("flock", "-n", lock, "true").Run() != nil
	}, 5*time.Second, 20*time.Millisecond)

	cmd := exec.Command("sh", entrypointScript(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "MIDDLEMAN_SEED_ONLY=1", "MIDDLEMAN_HOME=" + home}
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "the entrypoint goes on while the database is in use: %s", out)
	assert.Contains(t, string(out), "in use")
	assert.Empty(t, preForgeBackups(t, home))
}
