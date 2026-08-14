// Command harness — styled help output.
//
// Governing: SPEC-0001 REQ "Zero And Error States" (shared voice + palette
// across cockpit and CLI); ADR-0002 (the CLI matches the TUI visual language).
// These functions reproduce the fang grammar (program line, usage block,
// command/flag tables, error hints) using only internal/tui/theme palette
// tokens. Text content is byte-identical to the previous plain-text help;
// only ANSI presentation changes. When stderr is not a TTY or --json is in
// effect, the plain text is emitted with no SGR sequences.
package main

import (
	"fmt"
	"os"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// helpText holds the plain-text content of each help section. These strings
// are the source of truth — the styled renderer wraps them in ANSI but never
// alters the words (SPEC-0001: verbs, flags, defaults, and hint strings are
// spec-quoted and must not change).
var helpText = struct {
	program     string
	desc        string
	usageLines  []string
	commands    [][2]string
	projectHead string
	projectCmds [][2]string
	projectNote string
	daemonHead  string
	daemonCmds  [][2]string
	flagsHead   string
	flags       [][2]string
}{
	program: "harness",
	desc:    "systemctl for your agents",
	usageLines: []string{
		"harness [--socket PATH] [--json] <command> [args]",
		"harness daemon <subcommand> [daemon-flags]",
	},
	commands: [][2]string{
		{"list", "list configured harnesses and their state (default)"},
		{"describe NAME", "show one harness in detail"},
		{"start NAME", "start (enable) a harness"},
		{"stop NAME", "stop (disable) a harness"},
		{"restart NAME", "restart a harness (clears a failed latch)"},
		{"logs NAME [--lines N] [--follow]", "show a harness's log tail"},
		{"profiles", "list profiles (active one flagged)"},
		{"use-profile NAME", "activate a profile"},
		{"reload", "re-read the daemon config"},
		{"daemon-info", "show daemon status"},
		{"doctor", "run health checks (config, daemon, harnesses)"},
		{"attach NAME [--ro]", "attach to a harness's terminal"},
	},
	projectHead: "project commands (repo-root harness.toml, discovered by walking up from cwd):",
	projectCmds: [][2]string{
		{"up", "register + start the project's harnesses (detached; takes no arguments)"},
		{"down [PROJECT]", "stop + deregister the project's harnesses"},
		{"ps", "list the project's harnesses (alias for list outside a project; takes no arguments)"},
	},
	projectNote: "inside a project, a bare NAME to describe/logs/start/stop/restart/attach\nalways resolves to <project>/NAME (a NAME containing \"/\" is taken as fully\nqualified); to address a global harness by its bare name, run the verb\noutside the project directory.",
	daemonHead: "daemon subcommands (see \"harness daemon --help\"):",
	daemonCmds: [][2]string{
		{"daemon start", "run the supervision daemon (ADR-0005 ExecStart; alias: run)"},
		{"daemon stop", "gracefully stop the running daemon"},
		{"daemon status", "alias for daemon-info"},
	},
	flagsHead: "flags:",
	flags: [][2]string{
		{"--socket PATH", "daemon socket (default $XDG_RUNTIME_DIR/harness.sock)"},
		{"--json", "machine-readable output"},
	},
}

// daemonHelpText holds the plain-text content of the daemon help.
var daemonHelpText = struct {
	program    string
	desc       string
	usageLines []string
	subHead    string
	subs       [][2]string
	flagHead   string
	flags      [][2]string
	exHead     string
	examples   [][2]string
}{
	program: "harness daemon",
	desc:    "supervise long-running harnesses",
	usageLines: []string{
		"harness daemon <subcommand> [flags]",
	},
	subHead: "subcommands:",
	subs: [][2]string{
		{"start", "run the supervision daemon in the foreground (alias: run; bare \"harness daemon\" defaults to start)"},
		{"stop", "gracefully stop the running daemon (SIGTERM)"},
		{"status", "show daemon status (alias: daemon-info)"},
	},
	flagHead: "start/run flags:",
	flags: [][2]string{
		{"--config PATH", "path to harness.toml"},
		{"--socket PATH", "control/data plane socket path"},
		{"--scrollback N", "per-harness scrollback ring depth (lines)"},
		{"--log-level LEVEL", "debug, info (default), warn, error"},
		{"--log-file PATH", "append logs to this file instead of stderr"},
		{"--detach", "fork into the background; redirect stdio to --log-file (default: $XDG_STATE_HOME/harness/harness-daemon.log)"},
		{"--ssh", "enable the remote Wish SSH server"},
		{"--ssh-listen H:P", "SSH bind address (overrides [server] listen)"},
		{"--version", "print version and exit"},
	},
	exHead: "examples:",
	examples: [][2]string{
		{"harness daemon start --detach", "run in the background"},
		{"harness daemon stop", "stop the running daemon"},
		{"harness daemon status", "check if the daemon is up"},
	},
}

// usage renders the root help to stderr. When stderr is a TTY and --json is
// off, the output is styled with the theme palette; otherwise plain text.
func usage() {
	out := os.Stderr
	if cliui.JSON() || !cliui.WriterIsTTY(out) {
		fmt.Fprint(out, plainUsage())
		return
	}
	fmt.Fprint(out, styledUsage(theme.Default()))
}

// daemonUsage renders the daemon help to stderr, same styling rules as usage().
func daemonUsage() {
	out := os.Stderr
	if cliui.JSON() || !cliui.WriterIsTTY(out) {
		fmt.Fprint(out, plainDaemonUsage())
		return
	}
	fmt.Fprint(out, styledDaemonUsage(theme.Default()))
}

// plainUsage returns the original plain-text root help, byte-identical to
// the pre-styling output so piping/grep still works after stripping SGR.
func plainUsage() string {
	var b strings.Builder
	b.WriteString("harness — systemctl for your agents\n\n")
	b.WriteString("usage:\n")
	for _, l := range helpText.usageLines {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	b.WriteString("\ncommands:\n")
	for _, c := range helpText.commands {
		fmt.Fprintf(&b, "  %-21s %s\n", c[0], c[1])
	}
	b.WriteString("\n")
	b.WriteString(helpText.projectHead + "\n")
	for _, c := range helpText.projectCmds {
		fmt.Fprintf(&b, "  %-21s %s\n", c[0], c[1])
	}
	b.WriteString("\n")
	b.WriteString(helpText.projectNote + "\n")
	b.WriteString("\n")
	b.WriteString(helpText.daemonHead + "\n")
	for _, c := range helpText.daemonCmds {
		fmt.Fprintf(&b, "  %-21s %s\n", c[0], c[1])
	}
	b.WriteString("\n")
	b.WriteString(helpText.flagsHead + "\n")
	for _, f := range helpText.flags {
		fmt.Fprintf(&b, "  %-21s %s\n", f[0], f[1])
	}
	b.WriteString("\n")
	return b.String()
}

// plainDaemonUsage returns the original plain-text daemon help.
func plainDaemonUsage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n", daemonHelpText.program, daemonHelpText.desc)
	b.WriteString("usage:\n")
	for _, l := range daemonHelpText.usageLines {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	b.WriteString("\n")
	b.WriteString(daemonHelpText.subHead + "\n")
	for _, s := range daemonHelpText.subs {
		fmt.Fprintf(&b, "  %-21s %s\n", s[0], s[1])
	}
	b.WriteString("\n")
	b.WriteString(daemonHelpText.flagHead + "\n")
	for _, f := range daemonHelpText.flags {
		fmt.Fprintf(&b, "  %-21s %s\n", f[0], f[1])
	}
	b.WriteString("\n")
	b.WriteString(daemonHelpText.exHead + "\n")
	for _, e := range daemonHelpText.examples {
		fmt.Fprintf(&b, "  %-21s %s\n", e[0], e[1])
	}
	b.WriteString("\n")
	return b.String()
}

// styledUsage builds the root help with fang-grammar styling via the theme
// palette. The text content matches plainUsage() exactly.
func styledUsage(th *theme.Theme) string {
	var lines []string

	lines = append(lines,
		th.HelpProgram().Render(helpText.program)+" "+
			th.HelpDesc().Render("— "+helpText.desc),
	)

	lines = append(lines, "", th.HelpUsage().Render("usage:"))
	for _, l := range helpText.usageLines {
		lines = append(lines, "  "+th.HelpArg().Render(l))
	}

	lines = append(lines, "", th.HelpSection().Render("commands:"))
	for _, c := range helpText.commands {
		lines = append(lines, styledRow(th, c[0], c[1], 21, true))
	}

	lines = append(lines, "")
	lines = append(lines, th.HelpSection().Render(helpText.projectHead))
	for _, c := range helpText.projectCmds {
		lines = append(lines, styledRow(th, c[0], c[1], 21, true))
	}
	lines = append(lines, "")
	lines = append(lines, th.HelpDesc2().Render(helpText.projectNote))

	lines = append(lines, "")
	lines = append(lines, th.HelpSection().Render(helpText.daemonHead))
	for _, c := range helpText.daemonCmds {
		lines = append(lines, styledRow(th, c[0], c[1], 21, true))
	}

	lines = append(lines, "")
	lines = append(lines, th.HelpSection().Render(helpText.flagsHead))
	for _, f := range helpText.flags {
		lines = append(lines, styledRow(th, f[0], f[1], 21, false))
	}

	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// styledDaemonUsage builds the daemon help with fang-grammar styling.
func styledDaemonUsage(th *theme.Theme) string {
	var lines []string

	lines = append(lines,
		th.HelpProgram().Render(daemonHelpText.program)+" "+
			th.HelpDesc().Render("— "+daemonHelpText.desc),
	)

	lines = append(lines, "", th.HelpUsage().Render("usage:"))
	for _, l := range daemonHelpText.usageLines {
		lines = append(lines, "  "+th.HelpArg().Render(l))
	}

	lines = append(lines, "", th.HelpSection().Render(daemonHelpText.subHead))
	for _, s := range daemonHelpText.subs {
		lines = append(lines, styledRow(th, s[0], s[1], 21, true))
	}

	lines = append(lines, "", th.HelpSection().Render(daemonHelpText.flagHead))
	for _, f := range daemonHelpText.flags {
		lines = append(lines, styledRow(th, f[0], f[1], 21, false))
	}

	lines = append(lines, "", th.HelpSection().Render(daemonHelpText.exHead))
	for _, e := range daemonHelpText.examples {
		lines = append(lines, styledRow(th, e[0], e[1], 21, true))
	}

	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// styledRow renders a table row: the name (verb or flag) left-padded to width
// and styled, followed by the description in Dim. For verb rows the name uses
// Accent; for flag rows Cyan. The padding is applied before rendering so the
// description column aligns consistently in both styled and plain output.
func styledRow(th *theme.Theme, name, desc string, width int, isVerb bool) string {
	nameStyle := th.HelpVerb()
	if !isVerb {
		nameStyle = th.HelpFlag()
	}
	// Pad the name to `width` chars + 1 separator space, matching the
	// plain output's fmt %-21s + " " alignment.
	pad := width - len(name)
	if pad < 0 {
		pad = 0
	}
	return nameStyle.Render(name+strings.Repeat(" ", pad+1)) + th.HelpDesc2().Render(desc)
}
