// Command kenn-forge-fleet-hub makes a container's forge config a fleet hub.
//
// `kenn-forge fleet setup hub --origin` writes the same settings, but it also
// installs a host service, so it cannot run inside the image. The container
// entrypoint runs this instead, before kenn-forge starts:
//
//	kenn-forge-fleet-hub --config /data/config.toml --origin https://hub.example
//
// It reads the config with forge's own loader, applies the hub settings setup
// applies, and writes the result with forge's own writer, so nothing here edits
// TOML by hand. Two differences from setup are deliberate: the hosts already
// allowed are kept (the container's healthcheck and in-network clients use
// them), and the listener is left alone (the entrypoint's socat bridge dials it).
//
// A config whose fleet is already enabled belongs to forge, which records its
// members there, so it is left unchanged.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"time"

	"go.kenn.io/forge/internal/config"
	"go.kenn.io/forge/internal/federation"
)

func main() {
	fs := flag.NewFlagSet("kenn-forge-fleet-hub", flag.ContinueOnError)
	path := fs.String("config", "", "forge config to make a fleet hub")
	origin := fs.String("origin", "", "the hub's federation origin (https://host[:port])")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *path == "" || *origin == "" {
		fmt.Fprintln(os.Stderr, "kenn-forge-fleet-hub: --config and --origin are required")
		os.Exit(2)
	}
	if err := makeHub(*path, *origin, time.Now, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "kenn-forge-fleet-hub: %v\n", err)
		os.Exit(1)
	}
}

// makeHub converts the config at path into a fleet hub at rawOrigin, keeping
// the file it replaced as <path>.pre-fleet.<UTC time>.
func makeHub(path, rawOrigin string, now func() time.Time, log io.Writer) error {
	origin, err := federation.CanonicalOrigin(rawOrigin)
	if err != nil {
		return err
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("parse canonical origin: %w", err)
	}
	host := parsed.Host

	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	if cfg.Fleet.Enabled {
		if cfg.Fleet.BaseURL != origin {
			fmt.Fprintf(log, "kenn-forge: warning: %s is already in a fleet at %s, not %s; leaving it as it is\n",
				path, cfg.Fleet.BaseURL, origin)
		}
		return nil
	}
	if cfg.Fleet.RoleOrDefault() == config.FleetRoleSpoke || cfg.Fleet.Hub != nil {
		return errors.New("an enrolled spoke must be revoked and restored to standalone state before it can be a hub")
	}

	cfg.BasePath = "/"
	if !slices.Contains(cfg.AllowedHosts, host) {
		cfg.AllowedHosts = append(cfg.AllowedHosts, host)
	}
	cfg.TrustReverseProxy = false
	cfg.API.RequireAuth = true
	cfg.API.TailscaleServe = config.TailscaleServeAPI{}
	cfg.Fleet.Enabled = true
	cfg.Fleet.Role = config.FleetRoleHub
	cfg.Fleet.BaseURL = origin

	tmp := fmt.Sprintf("%s.fleet-tmp.%d", path, os.Getpid())
	if err := cfg.Save(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write the hub config: %w", err)
	}
	if _, err := config.Load(tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("the hub config does not load back: %w", err)
	}
	backup := fmt.Sprintf("%s.pre-fleet.%s", path, now().UTC().Format("20060102T150405Z"))
	if err := copyFile(path, backup); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("keep %s: %w", backup, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	fmt.Fprintf(log, "kenn-forge: %s is now a fleet hub at %s (the old file is %s)\n", path, origin, backup)
	return nil
}

func copyFile(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
