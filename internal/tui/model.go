// Package tui is the harness cockpit: a keyboard-driven Bubble Tea client onto
// the daemon (SPEC-0001), a thin consumer of the protocol via internal/client
// (SPEC-0002, ADR-0002). It is a small mode machine — Dashboard and Attached —
// with an overlay layer (palette, profile switcher, harness form, confirm,
// help) and designed zero/error states.
//
// Governing: SPEC-0001 (all requirements) and its design.md split-cockpit;
// ADR-0001 (Bubble Tea / Bubbles / Lip Gloss / Huh / harmonica), ADR-0002 (thin
// client survives daemon loss), ADR-0003 (embedded terminal), ADR-0006 (file is
// truth), ADR-0007 (scrollback). See docs/design/ (day.png, hop.png).
package tui

import (
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/keys"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// mode is a primary UI mode (SPEC-0001 REQ "Mode Machine").
type mode int

const (
	modeDashboard mode = iota
	modeAttached
)

// overlay is the active overlay above the primary mode, if any.
type overlay int

const (
	overlayNone overlay = iota
	overlayPalette
	overlayProfile
	overlayForm
	overlayConfirm
	overlayHelp
	overlaySearch
)

// Options configures a Model.
type Options struct {
	Socket      string
	ConfigPath  string
	Version     string
	SkipConfirm bool               // --yes-style: skip confirm dialogs.
	Renderer    *lipgloss.Renderer // optional; nil uses the default.
	// ReadOnly forces every attach opened by this Model to be a read-only
	// attach (protocol.AttachRO): the daemon streams output but drops this
	// client's input. It exists so a read-only remote SSH session — or a local
	// invocation that only wants to watch — can never drive a harness's
	// terminal. Governing: ADR-0008 (read-only attach), SPEC-0002.
	ReadOnly bool
	// AttachOnly, when set, makes `harness attach <name>` skip the dashboard
	// and boot straight into an attached session for the named harness.
	// Detach quits the program (there's no dashboard to return to). This lets
	// the CLI one-shot verb reuse the TUI's embedded terminal + status bar +
	// detach chords instead of the raw stdout pipe.
	AttachOnly string
	dial       dialer // injectable; nil uses realDial.
}

// paletteState is the command-palette overlay state (SPEC-0001 REQ "Command
// Palette").
type paletteState struct {
	input    textinput.Model
	all      []Command
	filtered []Command
	sel      int
}

// profState is the profile-switcher overlay state (SPEC-0001 REQ "Profile
// Switcher").
type profState struct {
	sel      int
	askStart bool   // second step: offer to start stopped members
	pending  string // profile chosen, awaiting the start-stopped answer
}

// confirmState is a destructive-action confirm dialog (SPEC-0001 REQ
// "Confirmation Guards").
type confirmState struct {
	action Action
	target string
	prompt string
}

// formInputs binds the Huh form's string fields; toForm converts to the typed
// HarnessForm for TOML serialization.
type formInputs struct {
	name, cmd, args, workdir, envFile, delay, backend, description string
	enabled                                                        bool
}

// Model is the root Bubble Tea model.
type Model struct {
	opts    Options
	theme   *theme.Theme
	keys    keys.KeyMap
	help    help.Model
	spinner spinner.Model
	w, h    int

	// attachOnlyPending is set when opts.AttachOnly is non-empty; onConnected
	// consumes it to auto-attach to the named harness instead of landing on
	// the dashboard. Used by `harness attach <name>`.
	attachOnlyPending string

	// connection (two conns: control + events/attach — see readloop.go).
	ctrl    Controller
	attach  attachConn
	events  chan tea.Msg
	done    chan struct{}
	conn    startCondition
	connErr error
	reconn  bool

	// data
	harnesses []protocol.HarnessInfo
	profiles  []protocol.ProfileInfo
	daemon    protocol.DaemonInfo
	banner    string // config parse banner (last-good config, ADR-0006)
	status    string // transient status line

	// dashboard
	sel         int
	showAll     bool
	peek        logsMsg
	search      textinput.Model
	searchQuery string

	// modes / overlays
	mode    mode
	overlay overlay
	att     *attachState
	pal     paletteState
	prof    profState
	confirm confirmState
	form    tea.Model // *huh.Form when overlayForm
	fInputs formInputs
	editing bool // form is editing (e) vs new (n)

	quitting  bool
	closeOnce sync.Once
}

// New builds a Model from options.
func New(opts Options) *Model {
	if opts.dial == nil {
		opts.dial = realDial
	}
	th := theme.New(opts.Renderer, theme.DefaultPalette())
	pi := textinput.New()
	pi.Prompt = "› "
	pi.Placeholder = "verb target…  (e.g. restart reduit-agent)"
	pi.CharLimit = 128

	si := textinput.New()
	si.Prompt = "/"
	si.Placeholder = "filter harnesses…"
	si.CharLimit = 64

	hp := help.New()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = th.Faint()
	return &Model{
		opts:              opts,
		theme:             th,
		keys:              keys.Default(),
		help:              hp,
		spinner:           sp,
		pal:               paletteState{input: pi},
		search:            si,
		attachOnlyPending: opts.AttachOnly,
	}
}

// Init dials the daemon and starts the periodic tick.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.connectCmd(), tick(), probeSizeCmd())
}

// probeSizeCmd is a fallback for environments where Bubble Tea doesn't detect
// stdout as a TTY and thus never sends a WindowSizeMsg (some IDE terminals,
// nested ptys, etc.). It queries stdout's pty size directly and synthesizes a
// WindowSizeMsg. If Bubble Tea already sent one, the real one wins (it arrives
// first); this only fires if m.w is still 0.
type probeSizeMsg struct{ w, h int }

func probeSizeCmd() tea.Cmd {
	return func() tea.Msg {
		// Query stdout's pty size directly. Bubble Tea's own checkResize
		// does the same thing, but only when it detects stdout as a TTY
		// (sets ttyOutput). In some environments that detection fails, and
		// no WindowSizeMsg is ever sent — this fallback covers that.
		w, h, err := term.GetSize(os.Stdout.Fd())
		if err != nil || w <= 0 || h <= 0 {
			return probeSizeMsg{0, 0}
		}
		return probeSizeMsg{w, h}
	}
}

// connectedMsg is the result of dialing both connections.
type connectedMsg struct {
	ctrl   Controller
	attach attachConn
	err    error
}

// connectCmd dials the control + events/attach connections.
func (m *Model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		ctrl, att, err := m.opts.dial(m.opts.Socket, m.opts.Version)
		return connectedMsg{ctrl: ctrl, attach: att, err: err}
	}
}

// visible returns the harnesses under the active profile (or all), further
// narrowed by the live search query when one is set (SPEC-0001 REQ "Dashboard"
// + `/` search).
func (m *Model) visible() []protocol.HarnessInfo {
	out := filterByProfile(m.harnesses, activeProfile(m.profiles), m.showAll)
	if m.searchQuery == "" {
		return out
	}
	q := strings.ToLower(m.searchQuery)
	var filtered []protocol.HarnessInfo
	for _, h := range out {
		if strings.Contains(strings.ToLower(h.Name), q) {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

// selectedHarness returns the currently selected visible harness, or a zero
// value + false when the list is empty.
func (m *Model) selectedHarness() (protocol.HarnessInfo, bool) {
	v := m.visible()
	if m.sel < 0 || m.sel >= len(v) {
		return protocol.HarnessInfo{}, false
	}
	return v[m.sel], true
}

// clampSel keeps the selection in range after the list changes.
func (m *Model) clampSel() {
	n := len(m.visible())
	if n == 0 {
		m.sel = 0
		return
	}
	m.sel = clamp(m.sel, 0, n-1)
}

// startReadLoop spins up the single frame dispatch goroutine on the events/
// attach connection and returns the Cmd that drains it.
func (m *Model) startReadLoop() tea.Cmd {
	m.events = make(chan tea.Msg, 64)
	m.done = make(chan struct{})
	go runReadLoop(m.attach.Conn(), m.events, m.done)
	return waitForFrame(m.events)
}

// stopReadLoop tears down the read loop (on disconnect/quit).
func (m *Model) stopReadLoop() {
	if m.done != nil {
		close(m.done)
		m.done = nil
	}
}

// Close releases the model's two daemon connections and unblocks its read-loop
// goroutine. Bubble Tea returns on QuitMsg WITHOUT delivering it to Update (see
// tea.eventLoop), so a Model can never clean up from inside its own loop. A host
// that ends the program out-of-band — notably a remote SSH session closing,
// where the daemon is long-lived and every attach/detach would otherwise leak
// two socket connections plus the read-loop goroutine — MUST call Close after
// Program.Run returns. It is safe to call exactly once; call it only after the
// Bubble Tea program has stopped so it never races Update's access to these
// fields. The read-loop goroutine holds the *protocol.Conn directly (not these
// fields), so closing the connections here safely unblocks its ReadFrame, and
// closing done unblocks any pending channel emit.
func (m *Model) Close() {
	m.closeOnce.Do(func() {
		if m.done != nil {
			close(m.done)
			m.done = nil
		}
		if m.attach != nil {
			_ = m.attach.Close()
		}
		if m.ctrl != nil {
			_ = m.ctrl.Close()
		}
	})
}
