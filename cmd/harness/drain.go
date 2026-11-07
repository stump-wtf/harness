package main

// The Operator Drain Verbs
//
// `harness drain` takes a harness out of service without touching the run that
// is already going: no source starts a new one (cron, a channel, a webhook,
// `harness trigger`, a catch_up, a self-triggered queue), and whatever the
// in-flight run earns for `on_overlap = "queue"` stays queued for `undrain`.
// `harness undrain` puts it back in service and starts that held firing.
//
// `drain --wait` is the half that makes the pair usable from a packaging
// script: it blocks until every drained harness is idle and exits non-zero when
// its --timeout passes, so
//
//	harness drain --all && harness drain --wait --timeout 30m && brew services restart harness
//
// upgrades a daemon without SIGTERMing an agent mid-run. A daemon restart
// resumes every harness (the drain gate is runtime state, never persisted),
// which is what that chain wants: the wait is the gate, the restart is the
// resume.
//
// `--all` expands to the daemon's own harness list here, exactly as the
// lifecycle verbs do, so the daemon always receives explicit names. `--wait`
// with no name waits on whatever an earlier call drained.
//
// Governing: stump.wtf/harness#828; ADR-0062 (proposed); SPEC-0021 REQ-4
// "Admission", REQ-14 "Release and hold reasons".
//
// @joestump-agent 11/06/2026 - Added for stump.wtf/harness#828.

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
)

// newDrainCmd builds `harness drain NAME|--all [--discard] [--wait] [--timeout D]`.
func newDrainCmd(g *globalOpts) *cobra.Command {
	var (
		all     bool
		discard bool
		wait    bool
		forDur  string
	)
	cmd := &cobra.Command{
		Use:           "drain [NAME]",
		Short:         "take a harness out of service: start no new run, let the run in flight finish",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The flag combinations are checked before the name, so a script
			// that typed the wrong flag is told about the flag, not about a
			// name it deliberately left out.
			if forDur != "" && !wait {
				return fmt.Errorf("cannot combine --timeout without --wait")
			}
			if discard && wait {
				return fmt.Errorf("cannot combine --discard and --wait (--discard drops the queued firing now; --wait only waits)")
			}
			if forDur != "" {
				d, err := time.ParseDuration(forDur)
				if err != nil || d <= 0 {
					return fmt.Errorf("invalid --timeout duration %q: must be a positive duration (e.g. 30m)", forDur)
				}
			}
			// --wait alone is the restart gate: it waits on harnesses an
			// earlier call drained, so it needs no name.
			arity := nameRequired
			if all || wait {
				arity = nameOptional
			}
			name, err := bindName("drain", arity, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.all, o.discard, o.wait, o.timeout = name, all, discard, wait, forDur
			return run("drain", o)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "apply to every harness (name is ignored)")
	cmd.Flags().BoolVar(&discard, "discard", false, "drop a queued firing instead of holding it for undrain")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until every drained harness is idle; non-zero exit on timeout")
	cmd.Flags().StringVar(&forDur, "timeout", "", "bound --wait by this duration (e.g. 30m); bare --wait waits without a deadline")
	return cmd
}

// newUndrainCmd builds `harness undrain NAME|--all`.
func newUndrainCmd(g *globalOpts) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:           "undrain [NAME]",
		Short:         "put a drained harness back in service and start the firing the drain held",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			arity := nameRequired
			if all {
				arity = nameOptional
			}
			name, err := bindName("undrain", arity, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.all = name, all
			return run("undrain", o)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "apply to every harness (name is ignored)")
	return cmd
}

// drainVerb maps the verb to its handler, the way lifecycle does for
// start/stop/restart.
func drainVerb(verb string) func(*client.Client, verbOpts) error {
	if verb == "undrain" {
		return cmdUndrain
	}
	return cmdDrain
}

// cmdDrain answers the drain verb: arm the gate on the named harnesses, wait
// out the runs in flight when asked, and print each harness's drain state.
func cmdDrain(c *client.Client, o verbOpts) error {
	names, err := drainNames(c, o)
	if err != nil {
		return err
	}
	infos, err := c.Drain(names, o.discard, o.wait, o.timeout)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(infos)
	}
	printDrainInfos(infos, o.wait && len(names) == 0)
	return nil
}

// cmdUndrain answers the undrain verb: clear the gate and start what it held.
func cmdUndrain(c *client.Client, o verbOpts) error {
	names, err := drainNames(c, o)
	if err != nil {
		return err
	}
	infos, err := c.Undrain(names)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(infos)
	}
	printDrainInfos(infos, false)
	return nil
}

// drainNames resolves the verb's targets into the explicit list the daemon
// expects: one name, every harness for --all (read from the daemon, the way
// lifecycleAll resolves it), or nothing for a nameless --wait.
func drainNames(c *client.Client, o verbOpts) ([]string, error) {
	if o.name != "" {
		return []string{o.name}, nil
	}
	if !o.all {
		return nil, nil
	}
	hs, err := c.List()
	if err != nil {
		return nil, fmt.Errorf("--all: list harnesses: %w", err)
	}
	if len(hs) == 0 {
		return nil, fmt.Errorf("--all: no harnesses configured")
	}
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.Name)
	}
	return names, nil
}

// printDrainInfos prints one line per harness. The wait-only call (drain
// --wait, no target) gets the verdict line instead, because it changed nothing.
func printDrainInfos(infos []protocol.HarnessInfo, waitedOnly bool) {
	for _, info := range infos {
		fmt.Print(plainDrainLine(info))
	}
	if waitedOnly {
		fmt.Println("drained: every drained harness is idle")
	}
}

// plainDrainLine is the script-readable form of one harness's drain state:
// glyph, name, the drain label, and the lifecycle state underneath it. An
// undrained harness has no label, so it reads "undrained".
func plainDrainLine(info protocol.HarnessInfo) string {
	label := info.Drain
	if label == "" {
		label = "undrained"
	}
	return fmt.Sprintf("%s %s: %s (%s)\n", stateGlyph(info.State), info.Name, label, info.State)
}
