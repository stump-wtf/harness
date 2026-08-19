package main

// Governing: ADR-0001 (Go + Charmbracelet stack owns the visual language) and
// SPEC-0003 (paired glyph + adaptive color — the animation is decorative, the
// glyph carries the state). This is the animated rendering for the --all
// lifecycle verbs (start/stop/restart --all): a Bubble Tea program that walks
// the harnesses one at a time, spinner on the harness currently being acted
// on, SPEC-0003 glyph + state once the daemon answers, and an overall
// progress bar. On exit the final frame stays in the terminal, so the run
// leaves the same per-harness record the plain renderer printed — just
// animated while it happens.
//
// Non-TTY (pipe, script, CI) never enters Bubble Tea: lifecycleAll falls back
// to the plain line-per-harness output, and --json keeps its machine
// contract, so nothing scriptable changes.

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// lifecycleStyle bundles the lipgloss styles for the animated run, resolved
// once from the shared theme so the CLI animation and the cockpit TUI speak
// the same visual language. State coloring goes through the theme's own
// StateStyle (SPEC-0001 mapping) so "running" is mint here exactly as it is
// in `harness list`.
type lifecycleStyle struct {
	th     *theme.Theme
	title  lipgloss.Style
	name   lipgloss.Style
	faint  lipgloss.Style
	done   lipgloss.Style
	failed lipgloss.Style
	spin   lipgloss.Style
}

func newLifecycleStyle() lifecycleStyle {
	th := theme.Default()
	col := th.Colors()
	return lifecycleStyle{
		th:     th,
		title:  lipgloss.NewStyle().Bold(true).Foreground(col.Accent),
		name:   lipgloss.NewStyle().Foreground(col.Fg),
		faint:  lipgloss.NewStyle().Foreground(col.Faint),
		done:   lipgloss.NewStyle().Foreground(col.Mint),
		failed: lipgloss.NewStyle().Foreground(col.Coral),
		spin:   lipgloss.NewStyle().Foreground(col.Cyan),
	}
}

// state renders a daemon state through the theme's SPEC-0001 state color.
func (s lifecycleStyle) state(state string) string {
	return s.th.StateStyle(core.State(state)).Render(state)
}

// rowState is one harness's progress through the animated run.
type rowState struct {
	name    string
	status  string // "", "ok", "failed"
	state   string // resulting daemon state ("" until the daemon answers)
	err     string
	running bool // spinner is on this row
}

// lifecycleModel drives the animated --all run.
type lifecycleModel struct {
	verb     string
	client   *client.Client
	names    []string
	rows     []rowState
	idx      int
	spin     spinner.Model
	bar      progress.Model
	style    lifecycleStyle
	quitting bool
	// interrupted records that the user aborted the run rather than the run
	// finishing. It is the difference between "everything worked" and "we
	// stopped asking", and only the model can tell them apart.
	interrupted bool
	errs        []string
}

// opDoneMsg reports the result of one lifecycle call.
type opDoneMsg struct {
	idx  int
	info protocol.HarnessInfo
	err  error
}

// newLifecycleModel builds the model; the client is shared with the caller
// (closed after the program exits, not by the program).
func newLifecycleModel(verb string, c *client.Client, names []string) *lifecycleModel {
	st := newLifecycleStyle()
	return &lifecycleModel{
		verb:   verb,
		client: c,
		names:  names,
		rows:   newRows(names),
		spin:   spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(st.spin)),
		bar:    progress.New(progress.WithWidth(30), progress.WithScaled(true)),
		style:  st,
	}
}

func newRows(names []string) []rowState {
	rows := make([]rowState, len(names))
	for i, n := range names {
		rows[i] = rowState{name: n}
	}
	return rows
}

// runOp issues the verb's control call for row i as a tea.Cmd.
func (m *lifecycleModel) runOp(i int) tea.Cmd {
	name, verb, c := m.names[i], m.verb, m.client
	return func() tea.Msg {
		var (
			info protocol.HarnessInfo
			err  error
		)
		switch verb {
		case "start":
			info, err = c.Start(name)
		case "stop":
			info, err = c.Stop(name)
		case "restart":
			info, err = c.Restart(name)
		}
		return opDoneMsg{idx: i, info: info, err: err}
	}
}

func (m *lifecycleModel) Init() tea.Cmd {
	m.rows[0].running = true
	return tea.Batch(m.spin.Tick, m.runOp(0))
}

func (m *lifecycleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" || msg.String() == "esc" {
			m.quitting = true
			m.interrupted = true
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case progress.FrameMsg:
		bar, cmd := m.bar.Update(msg)
		m.bar = bar
		return m, cmd
	case opDoneMsg:
		row := &m.rows[msg.idx]
		row.running = false
		if msg.err != nil {
			row.status = "failed"
			row.err = msg.err.Error()
			m.errs = append(m.errs, fmt.Sprintf("%s: %v", row.name, msg.err))
		} else {
			row.status = "ok"
			row.state = msg.info.State
		}
		next := m.idx + 1
		if next >= len(m.names) {
			m.idx = next
			m.quitting = true
			return m, tea.Batch(m.bar.SetPercent(1), tea.Quit)
		}
		m.idx = next
		m.rows[next].running = true
		pct := float64(next) / float64(len(m.names))
		return m, tea.Batch(m.bar.SetPercent(pct), m.runOp(next))
	}
	return m, nil
}

func (m *lifecycleModel) View() tea.View {
	return tea.NewView(m.content())
}

// finalView is the frame that stays in the terminal after the program exits:
// the completed rows and summary, without the live spinner/progress chrome.
// Bubble Tea v2 clears its last frame on exit, so the caller prints this
// after Run returns — the run leaves a permanent record, not a flash.
func (m *lifecycleModel) finalView() string {
	m.quitting = true
	return m.content()
}

func (m *lifecycleModel) content() string {
	var b strings.Builder
	if !m.quitting {
		b.WriteString(m.style.title.Render(fmt.Sprintf("%s %d harnesses", verbGerund(m.verb), len(m.names))))
		b.WriteString("\n\n")
	}
	for i := range m.rows {
		r := &m.rows[i]
		switch {
		case r.running:
			fmt.Fprintf(&b, "%s %s %s\n",
				m.spin.View(),
				m.style.name.Render(r.name),
				m.style.faint.Render(m.verb+"ing…"))
		case r.status == "ok":
			fmt.Fprintf(&b, "%s %s %s\n",
				m.style.done.Render("✓"),
				m.style.name.Render(r.name),
				m.style.state(r.state))
		case r.status == "failed":
			fmt.Fprintf(&b, "%s %s %s\n",
				m.style.failed.Render("✗"),
				m.style.name.Render(r.name),
				m.style.failed.Render(firstLine(r.err)))
		default:
			fmt.Fprintf(&b, "  %s\n", m.style.faint.Render(r.name))
		}
	}
	if m.quitting {
		if len(m.errs) > 0 {
			fmt.Fprintf(&b, "\n%s %d failed\n", m.style.failed.Render("✗"), len(m.errs))
		} else if m.idx >= len(m.names) {
			fmt.Fprintf(&b, "\n%s %d harnesses %s\n", m.style.done.Render("✓"), len(m.names), verbPast(m.verb))
		} else {
			b.WriteString("\ninterrupted\n")
		}
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(m.bar.View())
	b.WriteString(m.style.faint.Render("  ctrl-c to abort"))
	return b.String()
}

// verbGerund/verbPast give the titles natural English per verb.
func verbGerund(verb string) string {
	switch verb {
	case "start":
		return "Starting"
	case "stop":
		return "Stopping"
	default:
		return "Restarting"
	}
}

func verbPast(verb string) string {
	switch verb {
	case "start":
		return "started"
	case "stop":
		return "stopped"
	default:
		return "restarted"
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// result is the run's verdict as the caller's exit status: nil only when every
// harness was acted on and every call succeeded.
//
// An abort is its own outcome, not an absence of errors. `stop --all` killed
// with ctrl-c after 2 of 20 collected no per-harness error, so reporting only
// m.errs exited 0 and told a script the fleet was down when 18 of it was still
// up. Interruption reports the count it got through, and still carries any
// failures underneath it.
func (m *lifecycleModel) result() error {
	var err error
	if len(m.errs) > 0 {
		err = fmt.Errorf("%d failed:\n  %s", len(m.errs), strings.Join(m.errs, "\n  "))
	}
	if !m.interrupted {
		return err
	}
	if err != nil {
		return fmt.Errorf("interrupted after %d of %d harnesses; %w", m.idx, len(m.names), err)
	}
	return fmt.Errorf("interrupted after %d of %d harnesses", m.idx, len(m.names))
}

// runLifecycleAnimated renders the animated --all run and reports its verdict.
func runLifecycleAnimated(verb string, c *client.Client, names []string) error {
	m := newLifecycleModel(verb, c, names)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		// A rendering failure must not mask the work: fall through with the
		// errors collected so far plus the render failure itself.
		m.errs = append(m.errs, fmt.Sprintf("render: %v", err))
	}
	// Bubble Tea v2's graceful shutdown re-renders the final model state via
	// p.render(model) before stopRenderer, which writes the completed rows +
	// summary to stdout. The renderer then erases that frame on close (inline
	// mode: EraseScreenBelow), but the erase is unreliable across terminals
	// and timing windows — producing a duplicate of the final output. We own
	// the permanent record via finalView below, so clear the last frame
	// explicitly before printing it.
	fmt.Print("\033[2J\033[H")
	fmt.Print(m.finalView())
	return m.result()
}
