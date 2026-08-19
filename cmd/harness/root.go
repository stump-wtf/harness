package main

// Cobra Command Tree
//
// The whole CLI surface, declared once. This replaces three hand-rolled parsers
// that existed only because the standard library's flag package halts at the
// first positional: parseInterleaved (parse, peel one positional, re-parse),
// parseDaemonArgs (scan for a subcommand token while skipping flag values via a
// hand-maintained skip-list), and resolveDaemonOpts (re-parse the daemon flags
// because the global flag set had already resolved the wrong socket). Cobra
// does all three natively.
//
// Two things are deliberately NOT delegated to Cobra:
//
//   * Help. help.go renders a theme-aware, mono-legible usage screen that the
//     TUI palette drives and that a dozen tests pin. Cobra's default help would
//     replace it with something generic, so the command tree borrows ours via
//     SetHelpFunc/SetUsageFunc instead.
//   * Errors. cliui owns the styled error box and the exit code, so every
//     command runs with SilenceErrors and SilenceUsage and returns its error up
//     to main, which hands it to cliui.Fatal exactly as before. Letting Cobra
//     print would produce two error messages and the wrong exit code.
//
// Flag scoping is load-bearing and matches what shipped before: --socket,
// --config and --json are persistent (usable before or after the verb), while
// --lines, --follow, --ro and --all are local to their verbs. Promoting a
// verb-local flag to a persistent one would silently widen which commands
// accept it; TestCLIVerbFlagBeforeVerbIsAnError pins that boundary.
//
// Governing: ADR-0016 (Cobra owns the command tree), SPEC-0010 REQ "Command
// Tree".
//
// @joestump-agent 08/19/2026 - Introduced with the ADR-0016 migration,
// replacing main.go's hand-rolled dispatch.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
)

// globalOpts holds the persistent flags. Verb-local flags live on their own
// commands and are copied into verbOpts by each command's RunE.
type globalOpts struct {
	socket     string
	configPath string
	json       bool
}

// newRootCmd builds the full command tree. It is a constructor rather than a
// package-level var so tests can build an isolated tree per case without flag
// state leaking between them.
func newRootCmd() *cobra.Command {
	g := &globalOpts{}

	root := &cobra.Command{
		Use:           "harness",
		Short:         "systemctl for your agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Bare `harness` opens the cockpit TUI (SPEC-0001). Any leftover
		// positional here is an unknown verb, and Args below rejects it before
		// RunE ever runs.
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(g.socket, g.configPath)
		},
		// Every command resolves the persistent settings through the ADR-0016
		// ladder before it runs, so `HARNESS_SOCKET=… harness ls` behaves
		// exactly like `harness --socket … ls`. Cobra runs only the closest
		// PersistentPreRunE in the chain, so the daemon group declares its own
		// and calls this explicitly.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return resolveGlobals(cmd, g)
		},
		// Replaces Cobra's legacyArgs, whose "unknown command %q for %q"
		// wording is not the calm single-line message cliui.Fatal expects.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q (run `harness -h` for the list)", args[0])
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&g.socket, "socket", "", "daemon socket path")
	root.PersistentFlags().StringVar(&g.configPath, "config", "", "harness.toml path (TUI harness form writes here)")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "machine-readable JSON output")

	// --version prints "harness <version>" exactly as the hand-rolled flag did.
	root.Version = buildinfo.Version
	root.SetVersionTemplate("harness {{.Version}}\n")

	// Borrow help.go's styled renderer rather than Cobra's generic one.
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if cmd.Name() == "daemon" {
			daemonUsage()
			return
		}
		usage()
	})
	root.SetUsageFunc(func(cmd *cobra.Command) error {
		if cmd.Name() == "daemon" {
			daemonUsage()
			return nil
		}
		usage()
		return nil
	})

	root.AddCommand(
		newSimpleCmd(g, "list", "list harnesses in the active profile", nameNone),
		newSimpleCmd(g, "ps", "list the project's harnesses", nameNone),
		newSimpleCmd(g, "up", "register + start the project's harnesses", nameNone),
		newSimpleCmd(g, "down", "stop + deregister the project's harnesses", nameOptional),
		newSimpleCmd(g, "describe", "show one harness in detail", nameRequired),
		newSimpleCmd(g, "profiles", "list profiles (active one flagged)", nameNone),
		newSimpleCmd(g, "use-profile", "activate a profile", nameRequired),
		newSimpleCmd(g, "reload", "re-read the daemon config", nameNone),
		newLifecycleCmd(g, "start", "start a harness"),
		newLifecycleCmd(g, "stop", "stop a harness"),
		newLifecycleCmd(g, "restart", "restart a harness"),
		newLogsCmd(g),
		newAttachCmd(g),
		newDoctorCmd(g),
		newDaemonCmd(g),
	)

	return root
}

// nameArity describes how a verb treats its single positional argument. The
// three behaviours existed before as requireName/rejectName/neither; naming
// them keeps the command constructors declarative.
type nameArity int

const (
	nameNone     nameArity = iota // a positional is an error (up, ps, list…)
	nameOptional                  // a positional may be supplied (down)
	nameRequired                  // a positional must be supplied (describe, logs…)
)

// bindName validates the positional against the verb's arity and returns it.
func bindName(verb string, arity nameArity, args []string) (string, error) {
	var name string
	if len(args) > 0 {
		name = args[0]
	}
	switch arity {
	case nameNone:
		if name != "" {
			return "", fmt.Errorf("harness %s takes no arguments (got %q)", verb, name)
		}
	case nameRequired:
		if name == "" {
			return "", fmt.Errorf("this command requires a harness/profile name")
		}
	}
	return name, nil
}

// opts snapshots the persistent flags into the verbOpts the handlers take.
func (g *globalOpts) opts() verbOpts {
	return verbOpts{socket: g.socket, configPath: g.configPath, json: g.json}
}

// newSimpleCmd builds a verb that takes only the persistent flags plus an
// optional/required/forbidden name.
func newSimpleCmd(g *globalOpts, verb, short string, arity nameArity) *cobra.Command {
	return &cobra.Command{
		Use:           verb,
		Short:         short,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := bindName(verb, arity, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name = name
			return run(verb, o)
		},
	}
}

// newLifecycleCmd builds start/stop/restart, which additionally take --all.
// With --all the name requirement is lifted, matching the previous dispatch.
func newLifecycleCmd(g *globalOpts, verb, short string) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:           verb,
		Short:         short,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			arity := nameRequired
			if all {
				arity = nameOptional
			}
			name, err := bindName(verb, arity, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.all = name, all
			return run(verb, o)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "apply to every harness (name is ignored)")
	return cmd
}

func newLogsCmd(g *globalOpts) *cobra.Command {
	var (
		lines  int
		follow bool
	)
	cmd := &cobra.Command{
		Use:           "logs",
		Short:         "tail a harness's log",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := bindName("logs", nameRequired, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.lines, o.follow = name, lines, follow
			return run("logs", o)
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 200, "number of trailing lines")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new output")
	return cmd
}

func newAttachCmd(g *globalOpts) *cobra.Command {
	var ro bool
	cmd := &cobra.Command{
		Use:           "attach",
		Short:         "attach to a harness's terminal",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := bindName("attach", nameRequired, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.ro = name, ro
			return run("attach", o)
		},
	}
	cmd.Flags().BoolVar(&ro, "ro", false, "read-only (ignore keystrokes)")
	return cmd
}

// newDoctorCmd keeps doctor's own reporting and exit code: it never returns an
// error to cliui.Fatal, it exits directly, exactly as the previous dispatch did.
func newDoctorCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:           "doctor",
		Short:         "run health checks (config, daemon, harnesses)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			exitFn(runDoctor(g.opts()))
			return nil
		},
	}
}

// exitFn is os.Exit in production and a capture point in tests, so a command
// that owns its own exit code (doctor) stays testable in-process.
var exitFn = os.Exit
