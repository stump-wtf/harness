package main

// Governing: ADR-0017 (ephemeral scratchpad harnesses), SPEC-0011 REQ
// "Scratchpad Creation" — the screen/tmux/shpool replacement: `harness run
// claude opus-5` mints a randomly-named scratchpad, starts it, prints the
// name, and touches no files. The first positional selects the harness kind
// when it names one; otherwise the whole invocation runs as a generic command
// (`sh -c`). `--kind` overrides the heuristic for the rare collision.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// kindAliases maps a first positional to the adapter enum (ADR-0011). "claude"
// is the word people type; the enum value is "claude-code".
var kindAliases = map[string]string{
	"crush":       "crush",
	"claude":      "claude-code",
	"claude-code": "claude-code",
	"codex":       "codex",
	"generic":     "generic",
}

// newRunCmd builds `harness run [flags] ARG...`.
func newRunCmd(g *globalOpts) *cobra.Command {
	var workdir, kind, name string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "run a throwaway scratchpad harness (random name, dies with the daemon)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			def, slug := scratchpadDef(kind, name, args)
			if workdir != "" {
				// Relative --workdir resolves against the caller's cwd, not
				// the daemon's (SPEC-0011 REQ "Scratchpad Creation").
				abs, err := filepath.Abs(workdir)
				if err != nil {
					return fmt.Errorf("harness run: --workdir: %w", err)
				}
				def.Workdir = abs
			}
			o := g.opts()
			o.name = slug
			return withClient(o, nil, func(c *client.Client, o verbOpts) error {
				return cmdRun(c, o, def)
			})
		},
	}
	// Interspersed parsing is off (SPEC-0011 REQ "Scratchpad Creation"):
	// positionals after the first belong to the invoked command, not to
	// `run` — `harness run htop -t` must reach the daemon as `-c "htop -t"`,
	// not die on cobra's unknown-shorthand-flag error. Run's own flags
	// (--kind, --name, --workdir) still parse when they lead the invocation.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&workdir, "workdir", "", "working directory (default: the caller's cwd)")
	cmd.Flags().StringVar(&kind, "kind", "", "harness kind override (crush, claude-code, codex, generic)")
	cmd.Flags().StringVar(&name, "name", "", "name slug override (a random suffix is still appended)")
	return cmd
}

// scratchpadDef maps positionals onto a wire definition and the slug to mint
// from. First positional dispatch: a known kind word consumes itself and the
// rest become args; anything else means the entire invocation is a generic
// command run via `sh -c`. --kind (when set) replaces the heuristic.
func scratchpadDef(kind, name string, args []string) (protocol.ProjectHarness, string) {
	adapter := kind
	if adapter == "" {
		if k, ok := kindAliases[args[0]]; ok {
			adapter = k
			args = args[1:]
		} else {
			adapter = "generic"
		}
	}
	words := args
	if adapter == "generic" && (kind == "" || kind == "generic") {
		// The generic fallback runs the invocation as one shell command.
		args = []string{"-c", strings.Join(args, " ")}
		words = strings.Fields(strings.Join(words, " "))
	}
	def := protocol.ProjectHarness{Harness: adapter, Args: args, Enabled: true}
	if name != "" {
		def.Name = name
		return def, name
	}
	return def, adapter + " " + strings.Join(words, " ")
}

// cmdRun issues the scratch_run and prints the minted name.
func cmdRun(c *client.Client, o verbOpts, def protocol.ProjectHarness) error {
	data, err := c.ScratchRun(def, o.name)
	if err != nil {
		return fmt.Errorf("harness run: %w", err)
	}
	if o.json {
		return printJSON(data)
	}
	fmt.Fprintf(os.Stdout, "%s %s → %s\n", stateGlyph(data.Info.State), data.Name, data.Info.State)
	return nil
}
