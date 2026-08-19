// Command harness — styled help output.
//
// Governing: SPEC-0001 REQ "Zero And Error States" (shared voice + palette
// across cockpit and CLI); ADR-0002 (the CLI matches the TUI visual language).
// These functions reproduce the fang grammar (program line, usage block,
// command/flag tables, error hints) using only internal/tui/theme palette
// tokens. The words are those of the previous plain-text help — verbs, flags,
// defaults and hint strings are spec-quoted and unchanged — but the layout is
// generated rather than hand-wrapped, so long descriptions wrap at helpWidth
// instead of at whatever column the old raw string happened to break on. When
// stderr is not a TTY or --json is in effect, the plain text is emitted with
// no SGR sequences.
package main

import (
	"fmt"
	"os"
	"strings"

	"charm.land/lipgloss/v2"

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
		{"doctor", "run health checks (config, daemon, harnesses)"},
		{"attach NAME [--ro]", "attach to a harness's terminal"},
	},
	projectHead: "project commands (repo-root harness.toml, discovered by walking up from cwd):",
	projectCmds: [][2]string{
		{"up", "register + start the project's harnesses (detached; takes no arguments)"},
		{"down [PROJECT]", "stop + deregister the project's harnesses"},
		{"rm NAME", "stop + deregister one registered harness"},
		{"ps", "list the project's harnesses (alias for list outside a project; takes no arguments)"},
	},
	projectNote: "inside a project, a bare NAME to describe/logs/start/stop/restart/attach\nalways resolves to <project>/NAME (a NAME containing \"/\" is taken as fully\nqualified); to address a global harness by its bare name, run the verb\noutside the project directory.",
	daemonHead:  "daemon subcommands (see \"harness daemon --help\"):",
	daemonCmds: [][2]string{
		{"daemon start", "run the supervision daemon (ADR-0005 ExecStart; alias: run)"},
		{"daemon stop", "gracefully stop the running daemon"},
		{"daemon status", "show daemon status"},
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
		{"status", "show daemon status"},
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

// --- layout ---------------------------------------------------------------

const (
	// helpIndent is the left margin on every table row and usage line.
	helpIndent = "  "
	// helpNameWidth is the width of the name column; the description column
	// starts one space past it — the column the hand-written help used.
	helpNameWidth = 20
	// helpWidth is the column descriptions wrap at, so a long description
	// folds at a word boundary into the description column instead of being
	// hard-wrapped into column 0 by the terminal.
	helpWidth = 80
	// helpDescCol is the 0-based column descriptions (and their continuation
	// lines) start in.
	helpDescCol = len(helpIndent) + helpNameWidth + 1
	// helpExampleWidth is the name column for the examples table, whose
	// left-hand side is a whole command line rather than a single verb.
	helpExampleWidth = 36
	// helpExampleCol is helpDescCol for that table.
	helpExampleCol = len(helpIndent) + helpExampleWidth + 1
)

// styler renders the individual help elements. The plain renderer returns its
// input untouched; the themed one wraps it in palette colors.
//
// Both renderers drive the same layout code with a different styler, so the
// styled and plain output cannot drift apart in content or alignment. They
// previously had separate layout paths, and the styled one silently dropped
// the two-space row indent — invisible to a test that compares the two after
// strings.TrimSpace.
type styler struct {
	program    func(string) string
	desc       func(string) string
	usageLabel func(string) string
	section    func(string) string
	arg        func(string) string
	verb       func(string) string
	flag       func(string) string
	body       func(string) string
}

// plainStyler renders every element as bare text (no SGR sequences).
func plainStyler() styler {
	id := func(s string) string { return s }
	return styler{program: id, desc: id, usageLabel: id, section: id, arg: id, verb: id, flag: id, body: id}
}

// themeStyler renders every element through the theme palette.
func themeStyler(th *theme.Theme) styler {
	wrap := func(s lipgloss.Style) func(string) string {
		return func(v string) string { return s.Render(v) }
	}
	return styler{
		program:    wrap(th.HelpProgram()),
		desc:       wrap(th.HelpDesc()),
		usageLabel: wrap(th.HelpUsage()),
		section:    wrap(th.HelpSection()),
		arg:        wrap(th.HelpArg()),
		verb:       wrap(th.HelpVerb()),
		flag:       wrap(th.HelpFlag()),
		body:       wrap(th.HelpDesc2()),
	}
}

// helpPad returns the run of spaces separating a row's name from its
// description: enough to reach width, plus the single column gap. A name that
// overflows the column keeps a three-space gap so the two halves still read as
// separate columns instead of running together.
func helpPad(name string, width int) string {
	pad := width - len([]rune(name))
	if pad < 2 {
		pad = 2
	}
	return strings.Repeat(" ", pad+1)
}

// wrapDesc folds desc onto lines that end before helpWidth. The first line
// starts at column start (just after the name); continuations start at
// helpDescCol. A single word longer than the available width is left to
// overflow rather than broken mid-token — flag spellings and paths must stay
// greppable.
func wrapDesc(desc string, start, cont int) []string {
	words := strings.Fields(desc)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	width := helpWidth - start
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
			width = helpWidth - cont
			continue
		}
		line += " " + w
	}
	return append(out, line)
}

// row lays out one name/description table row in a name column of the given
// width. isVerb picks the name color (Accent for verbs, Cyan for flags); it
// has no effect on the plain renderer.
func row(s styler, name, desc string, width int, isVerb bool) []string {
	nameStyle := s.verb
	if !isVerb {
		nameStyle = s.flag
	}
	pad := helpPad(name, width)
	descCol := len(helpIndent) + width + 1
	segs := wrapDesc(desc, len(helpIndent)+len([]rune(name))+len(pad), descCol)
	out := []string{helpIndent + nameStyle(name) + pad + s.body(segs[0])}
	cont := strings.Repeat(" ", descCol)
	for _, seg := range segs[1:] {
		out = append(out, cont+s.body(seg))
	}
	return out
}

// renderUsage lays out the root help through the given styler.
func renderUsage(s styler) string {
	lines := []string{
		s.program(helpText.program) + " " + s.desc("— "+helpText.desc),
		"",
		s.usageLabel("usage:"),
	}
	for _, l := range helpText.usageLines {
		lines = append(lines, helpIndent+s.arg(l))
	}

	lines = append(lines, "", s.section("commands:"))
	for _, c := range helpText.commands {
		lines = append(lines, row(s, c[0], c[1], helpNameWidth, true)...)
	}

	lines = append(lines, "", s.section(helpText.projectHead))
	for _, c := range helpText.projectCmds {
		lines = append(lines, row(s, c[0], c[1], helpNameWidth, true)...)
	}

	lines = append(lines, "")
	for _, l := range strings.Split(helpText.projectNote, "\n") {
		lines = append(lines, helpIndent+s.body(l))
	}

	lines = append(lines, "", s.section(helpText.daemonHead))
	for _, c := range helpText.daemonCmds {
		lines = append(lines, row(s, c[0], c[1], helpNameWidth, true)...)
	}

	lines = append(lines, "", s.section(helpText.flagsHead))
	for _, f := range helpText.flags {
		lines = append(lines, row(s, f[0], f[1], helpNameWidth, false)...)
	}

	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// renderDaemonUsage lays out the daemon help through the given styler.
func renderDaemonUsage(s styler) string {
	lines := []string{
		s.program(daemonHelpText.program) + " " + s.desc("— "+daemonHelpText.desc),
		"",
		s.usageLabel("usage:"),
	}
	for _, l := range daemonHelpText.usageLines {
		lines = append(lines, helpIndent+s.arg(l))
	}

	lines = append(lines, "", s.section(daemonHelpText.subHead))
	for _, sub := range daemonHelpText.subs {
		lines = append(lines, row(s, sub[0], sub[1], helpNameWidth, true)...)
	}

	lines = append(lines, "", s.section(daemonHelpText.flagHead))
	for _, f := range daemonHelpText.flags {
		lines = append(lines, row(s, f[0], f[1], helpNameWidth, false)...)
	}

	lines = append(lines, "", s.section(daemonHelpText.exHead))
	for _, e := range daemonHelpText.examples {
		lines = append(lines, row(s, e[0], e[1], helpExampleWidth, true)...)
	}

	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// plainUsage returns the root help as bare text, so piping and grep keep
// working when stderr is not a terminal.
func plainUsage() string { return renderUsage(plainStyler()) }

// plainDaemonUsage returns the daemon help as bare text.
func plainDaemonUsage() string { return renderDaemonUsage(plainStyler()) }

// styledUsage builds the root help with fang-grammar styling via the theme
// palette. Same layout and same words as plainUsage(), plus SGR sequences.
func styledUsage(th *theme.Theme) string { return renderUsage(themeStyler(th)) }

// styledDaemonUsage builds the daemon help with fang-grammar styling.
func styledDaemonUsage(th *theme.Theme) string { return renderDaemonUsage(themeStyler(th)) }

// hasJSONArg reports whether --json was passed, by scanning the raw argv. Used
// only to seed cliui before flag.Parse, which can invoke Usage before the
// parsed value is available; the parsed flag is authoritative afterwards.
func hasJSONArg(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-json", "--json", "-json=true", "--json=true":
			return true
		}
	}
	return false
}
