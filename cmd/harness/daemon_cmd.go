package main

// Daemon Subcommand Group
//
// `harness daemon` mirrors systemctl: run/start (aliases), stop, status. Bare
// `harness daemon` means run, which is the ADR-0005 systemd ExecStart form and
// must keep working.
//
// The old dispatch needed parseDaemonArgs to find the subcommand token by
// scanning past flag values via a hand-maintained skip-list (daemonFlagValues),
// because `--log-file stop` would otherwise be read as the stop subcommand.
// Cobra knows which flags take values from the flag set itself, so the skip-list
// and its maintenance burden are gone.
//
// It also needed resolveDaemonOpts, which re-parsed the daemon flags because
// main's global flag set had already resolved --socket to the default and the
// stop/status branches used that stale value — issue #149, where
// `harness daemon --socket X stop` reported success while stopping an unrelated
// daemon. With --socket persistent on the root command there is one value, so
// the two-flag-sets trap cannot recur.
//
// Governing: ADR-0016, SPEC-0010 REQ "Command Tree"; ADR-0005 (init owns the
// daemon; --detach is a dev convenience).
//
// @joestump-agent 08/19/2026 - Extracted from main.go's nested switch during
// the Cobra migration.

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
)

// daemonOpts carries the resolved daemon settings. Previously these were flag
// pointers dereferenced throughout runDaemon; making them a value lets the
// settings layer (ADR-0016) populate them from flag, env, file, or default
// without runDaemon knowing or caring which source won.
type daemonOpts struct {
	configPath string
	socketPath string
	ringLines  int
	sshEnable  bool
	sshListen  string
	logLevel   string
	logFile    string
	detach     bool
}

func newDaemonCmd(g *globalOpts) *cobra.Command {
	d := &daemonOpts{}

	daemon := &cobra.Command{
		Use:           "daemon",
		Short:         "run the supervision daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		// Bare `harness daemon` is `harness daemon run`.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCmd(cmd, g, d)
		},
	}

	// Daemon-only flags. --socket, --config and --json come from the root's
	// persistent set, so they work before or after the subcommand.
	daemon.PersistentFlags().IntVar(&d.ringLines, "scrollback", attach.DefaultRingLines, "per-harness scrollback ring depth (lines)")
	daemon.PersistentFlags().BoolVar(&d.sshEnable, "ssh", false, "enable the remote Wish SSH server (ADR-0004; overrides [server] enabled)")
	daemon.PersistentFlags().StringVar(&d.sshListen, "ssh-listen", "", "SSH bind address host:port (overrides [server] listen)")
	daemon.PersistentFlags().StringVar(&d.logLevel, "log-level", "", "log level: debug, info, warn, error")
	daemon.PersistentFlags().StringVar(&d.logFile, "log-file", "", "append logs to this file instead of stderr")
	daemon.PersistentFlags().BoolVar(&d.detach, "detach", false, "fork into the background; redirect stdio to --log-file (dev convenience; prefer systemd in production)")

	// The daemon prints "harness daemon <version>"; Cobra only wires --version
	// onto the root, so this one is declared explicitly.
	var showVersion bool
	daemon.PersistentFlags().BoolVar(&showVersion, "version", false, "print version and exit")
	// Cobra runs only the closest PersistentPreRunE, so this shadows the
	// root's and must resolve the globals itself — otherwise `daemon stop`
	// under HARNESS_SOCKET would dial the default socket, which is the exact
	// shape of issue #149.
	daemon.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if showVersion {
			cmd.Printf("harness daemon %s\n", buildinfo.Version)
			exitFn(0)
			return nil
		}
		return resolveGlobals(cmd, g)
	}
	daemon.SetOut(os.Stdout)

	run := &cobra.Command{
		Use: "run", Short: "run the supervision daemon (ADR-0005 ExecStart)",
		Aliases:       []string{"start"},
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonCmd(cmd, g, d)
		},
	}

	stop := &cobra.Command{
		Use: "stop", Short: "gracefully stop the running daemon",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdStopDaemon(g.opts())
		},
	}

	status := &cobra.Command{
		Use: "status", Short: "show daemon status",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(g.opts(), nil, cmdDaemonInfo)
		},
	}

	daemon.AddCommand(run, stop, status)
	return daemon
}

// runDaemonCmd resolves the daemon's settings through the precedence ladder and
// hands them to runDaemon. Splitting resolution from execution is what lets the
// same runDaemon serve a flag-driven dev invocation and an environment-driven
// container without branching inside it.
func runDaemonCmd(cmd *cobra.Command, g *globalOpts, d *daemonOpts) error {
	if err := resolveDaemonSettings(cmd, g, d); err != nil {
		return err
	}
	if d.detach {
		return detachDaemon(d.childArgs())
	}
	runDaemon(*d)
	return nil
}

// childArgs builds the argv for the detached child: an explicit `daemon run`
// plus the resolved daemon settings as flags. The child argv is reconstructed
// from the settings rather than copied from os.Args so the subcommand tokens
// (`daemon start`, `daemon run`, bare `daemon`) the caller used can never leak
// into it and double up against the re-prepended `daemon run` (issue #292).
func (d *daemonOpts) childArgs() []string {
	args := []string{"daemon", "run",
		"--config", d.configPath,
		"--socket", d.socketPath,
		"--scrollback", strconv.Itoa(d.ringLines),
	}
	if d.sshEnable {
		args = append(args, "--ssh")
	}
	if d.sshListen != "" {
		args = append(args, "--ssh-listen", d.sshListen)
	}
	if d.logLevel != "" {
		args = append(args, "--log-level", d.logLevel)
	}
	if d.logFile != "" {
		args = append(args, "--log-file", d.logFile)
	}
	return args
}
