package main

// Settings Resolution Wiring
//
// Where the Cobra flag sets meet the ADR-0016 precedence ladder. Every process
// setting lands here, and nothing downstream needs to know whether a value came
// from a flag, the environment, the file, or a default.
//
// The order this runs in matters. Flags are bound first (so an explicitly typed
// flag can win), then the config file is located — itself a resolved setting,
// since HARNESS_CONFIG may name it — and only then is the file read for its
// scalar keys. A missing file is not an error: SPEC-0010 REQ "Fileless
// Operation" requires a container with no TOML at all to come up on environment
// variables alone.
//
// Governing: ADR-0016, SPEC-0010 REQ "Precedence Order", REQ "Fileless
// Operation".
//
// @joestump-agent 08/19/2026 - Introduced with the ADR-0016 environment layer.

import (
	"errors"

	"github.com/spf13/cobra"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/settings"
)

// newResolver builds a resolver seeded with the runtime-computed defaults. The
// socket and config defaults depend on the XDG environment and scrollback comes
// from the attach package, so none of the three can be a compile-time constant
// in the registry.
func newResolver(cmd *cobra.Command) *settings.Resolver {
	r := settings.New()
	r.SetDefault("socket", protocol.DefaultSocketPath())
	r.SetDefault("config", config.DefaultPath())
	r.SetDefault("scrollback", attach.DefaultRingLines)
	if cmd != nil {
		r.BindFlags(cmd.Flags())
	}
	return r
}

// resolveGlobals fills the persistent flags from the ladder. Called by every
// command's PersistentPreRunE so a bare `harness ls` under HARNESS_SOCKET
// behaves the same as `harness --socket … ls`.
func resolveGlobals(cmd *cobra.Command, g *globalOpts) error {
	r := newResolver(cmd)

	configPath, err := r.String("config")
	if err != nil {
		return err
	}
	// Read the file before resolving the rest, so file-backed settings can
	// contribute. Absence is expected and fine; a real parse failure is not.
	if err := r.ReadConfigFile(configPath); err != nil && !errors.Is(err, settings.ErrNoConfigFile) {
		return err
	}

	socket, err := r.String("socket")
	if err != nil {
		return err
	}
	jsonOut, err := r.Bool("json")
	if err != nil {
		return err
	}

	g.configPath, g.socket, g.json = configPath, socket, jsonOut
	// Re-apply now that HARNESS_JSON and the file have had their say; main()
	// seeded this from a raw argv scan so help could render correctly before
	// this ran.
	cliui.SetJSON(jsonOut)
	return nil
}

// resolveDaemonSettings fills the daemon's settings from the ladder, on top of
// the globals.
func resolveDaemonSettings(cmd *cobra.Command, g *globalOpts, d *daemonOpts) error {
	r := newResolver(cmd)

	configPath, err := r.String("config")
	if err != nil {
		return err
	}
	if err := r.ReadConfigFile(configPath); err != nil && !errors.Is(err, settings.ErrNoConfigFile) {
		return err
	}

	socket, err := r.String("socket")
	if err != nil {
		return err
	}
	ring, err := r.Int("scrollback")
	if err != nil {
		return err
	}
	sshEnable, err := r.Bool("ssh")
	if err != nil {
		return err
	}
	sshListen, err := r.String("ssh-listen")
	if err != nil {
		return err
	}
	logLevel, err := r.String("log-level")
	if err != nil {
		return err
	}
	logFile, err := r.String("log-file")
	if err != nil {
		return err
	}

	d.configPath, d.socketPath = configPath, socket
	d.ringLines, d.sshEnable, d.sshListen = ring, sshEnable, sshListen
	d.logLevel, d.logFile = logLevel, logFile

	g.configPath, g.socket = configPath, socket
	return nil
}

// resolveReport returns every setting with its winning source, for `harness
// doctor` (SPEC-0010 REQ "Source Attribution").
func resolveReport(cmd *cobra.Command) ([]settings.Resolved, error) {
	r := newResolver(cmd)

	configPath, err := r.String("config")
	if err != nil {
		return nil, err
	}
	if err := r.ReadConfigFile(configPath); err != nil && !errors.Is(err, settings.ErrNoConfigFile) {
		return nil, err
	}
	return r.ResolveAll()
}
