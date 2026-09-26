package main

// One-Shot Verb Rendering
//
// Governing: ADR-0001 (Go + Charmbracelet stack owns the visual language),
// SPEC-0003 REQ "State Model" (the glyph carries the state, colour only
// decorates it), SPEC-0001 REQ "State Presentation" (legible from glyph and
// text alone in a monochrome terminal).
//
// TL;DR: every mutating one-shot verb has TWO renderings. On a terminal it
// gets the cockpit's palette — SPEC-0003 glyph in its state colour, the
// harness name, a faint context line, a hint when the result is trouble. A
// pipe, a script, or CI gets the plain line it always got, byte for byte;
// --json never reaches this file at all. The plain string is the contract
// and the styled one is decoration, so every call site hands over the plain
// form verbatim and emit picks between them per writer.
//
// @joestump-agent 09/26/2026 - Added: `harness restart NAME` printed one
// uncoloured line while `restart --all` animated. Single-harness start/stop/
// restart now get a spinner while the daemon works and a coloured transition
// record after; use-profile, reload, down, rm, run, trigger and daemon stop
// get the styled one-liner.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

// emit writes plain to w, or styled's rendering when w is a terminal. The
// TTY decision is per writer, like cliui.Report's: a verb whose stdout is
// piped stays plain even when stderr is a terminal.
//
// The styled form goes through lipgloss.Fprint, which downsamples to what w
// itself supports and honours NO_COLOR, so the stderr lines (trigger --wait,
// daemon stop) degrade by their own stream rather than stdout's.
func emit(w io.Writer, plain string, styled func(lifecycleStyle) string) {
	if cliui.WriterIsTTY(w) {
		_, _ = lipgloss.Fprint(w, styled(newLifecycleStyle()))
		return
	}
	fmt.Fprint(w, plain)
}

// plainLifecycleLine is the non-TTY record of one lifecycle result. It is the
// exact line start/stop/restart (single and --all) and `run` have always
// printed, and scripts parse it; change it and you break them.
func plainLifecycleLine(info protocol.HarnessInfo) string {
	return fmt.Sprintf("%s %s → %s\n", stateGlyph(info.State), info.Name, info.State)
}

// callLifecycle issues one start/stop/restart. It is the only place the verb
// becomes an RPC, so the plain path, the single-harness animation and the
// --all animation cannot drift into asking the daemon different things.
func callLifecycle(c *client.Client, verb, name, forDur string) (protocol.HarnessInfo, error) {
	switch verb {
	case "start":
		if forDur != "" {
			return c.StartFor(name, forDur)
		}
		return c.Start(name)
	case "stop":
		return c.Stop(name)
	case "restart":
		return c.Restart(name)
	}
	return protocol.HarnessInfo{}, fmt.Errorf("unknown lifecycle verb %q", verb)
}

// lifecycleOutcome is everything the styled record of one lifecycle verb
// needs, so the record is a pure function of it and a test can render it
// without a daemon.
type lifecycleOutcome struct {
	// action is the past-tense summary on the context line ("restarted").
	action string
	// before is the state read just before the op, "" when it was not read or
	// the read failed. It turns "→ running" into "failed → running".
	before string
	info   protocol.HarnessInfo
}

// troubled reports whether the resulting state is one the operator should go
// and look at.
func troubled(state string) bool {
	s := core.State(state)
	return s == core.StateFailed || s == core.StateDegraded
}

// renderOutcome is the styled record:
//
//	● claude-rc  failed → running
//	  restarted · pid 48211 · 3 restarts · remote-control claude
//
// with a `→ harness logs NAME` hint underneath when the harness came out
// failed or degraded. The glyph is SPEC-0003's, in the theme's state colour;
// the transition's two states are each in their own colour so the change reads
// at a glance.
func (s lifecycleStyle) renderOutcome(o lifecycleOutcome) string {
	info := o.info
	st := core.State(info.State)
	var b strings.Builder

	transition := s.state(info.State)
	if o.before != "" {
		transition = s.state(o.before) + s.faint.Render(" → ") + transition
	}
	fmt.Fprintf(&b, "%s %s  %s\n",
		s.th.RenderGlyph(st),
		s.name.Bold(true).Render(info.Name),
		transition)

	var ctx []string
	if o.action != "" {
		ctx = append(ctx, o.action)
	}
	if info.PID > 0 {
		ctx = append(ctx, fmt.Sprintf("pid %d", info.PID))
	}
	switch {
	case info.RestartCount == 1:
		ctx = append(ctx, "1 restart")
	case info.RestartCount > 1:
		ctx = append(ctx, fmt.Sprintf("%d restarts", info.RestartCount))
	}
	if info.LeaseUntil != "" {
		ctx = append(ctx, "lease until "+info.LeaseUntil)
	}
	if info.Description != "" {
		ctx = append(ctx, info.Description)
	}
	var line string
	if len(ctx) > 0 {
		line = s.faint.Render(strings.Join(ctx, " · "))
	}
	if info.Flapping {
		// Flapping is the one context fact that is a warning, not a detail.
		if line != "" {
			line += s.faint.Render(" · ")
		}
		line += s.warn.Render("flapping")
	}
	if line != "" {
		fmt.Fprintf(&b, "  %s\n", line)
	}

	if troubled(info.State) {
		hint := s.failed
		if st == core.StateDegraded {
			hint = s.warn
		}
		fmt.Fprintf(&b, "  %s\n", hint.Render("→ see why: harness logs "+info.Name))
	}
	return b.String()
}

// singleLifecycleModel is the one-row counterpart of lifecycleModel: a
// spinner on the harness while the daemon works, then nothing — the permanent
// record is written after the program exits (see lifecycleModel.View for why
// the final frame must be empty).
type singleLifecycleModel struct {
	verb        string
	name        string
	before      string
	op          func() (protocol.HarnessInfo, error)
	spin        spinner.Model
	style       lifecycleStyle
	done        bool
	interrupted bool
	info        protocol.HarnessInfo
	err         error
}

// singleDoneMsg carries the one RPC's result back into the program.
type singleDoneMsg struct {
	info protocol.HarnessInfo
	err  error
}

func newSingleLifecycleModel(verb, name, before string, op func() (protocol.HarnessInfo, error), st lifecycleStyle) *singleLifecycleModel {
	return &singleLifecycleModel{
		verb:   verb,
		name:   name,
		before: before,
		op:     op,
		spin:   spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(st.spin)),
		style:  st,
	}
}

func (m *singleLifecycleModel) Init() tea.Cmd {
	op := m.op
	return tea.Batch(m.spin.Tick, func() tea.Msg {
		info, err := op()
		return singleDoneMsg{info: info, err: err}
	})
}

func (m *singleLifecycleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.interrupted = true
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case singleDoneMsg:
		m.done = true
		m.info, m.err = msg.info, msg.err
		return m, tea.Quit
	}
	return m, nil
}

// View is the live frame only; it is empty once the run is over so the record
// written afterwards is the single copy on screen.
func (m *singleLifecycleModel) View() tea.View {
	if m.done || m.interrupted {
		return tea.NewView("")
	}
	return tea.NewView(m.content())
}

// content is the in-flight row: `⠋ claude-rc  restarting… was running`.
func (m *singleLifecycleModel) content() string {
	line := fmt.Sprintf("%s %s  %s", m.spin.View(),
		m.style.name.Bold(true).Render(m.name),
		m.style.faint.Render(strings.ToLower(verbGerund(m.verb))+"…"))
	if m.before != "" {
		line += m.style.faint.Render(" was ") + m.style.state(m.before)
	}
	return line
}

// runLifecycleOneAnimated renders one start/stop/restart on a terminal.
//
// The pre-read of the current state is display-only: it is what makes a
// restart read as a transition, and a failed read just drops the "from" half
// rather than failing a verb that would otherwise have worked.
//
// The op runs under a sync.Once. If Bubble Tea cannot start (no usable
// terminal after all), the fallback call below either performs the op or —
// when the program already launched it — waits for that same call, so the
// daemon is never asked twice.
func runLifecycleOneAnimated(c *client.Client, verb, name, forDur string) error {
	before := ""
	if cur, err := c.Describe(name); err == nil {
		before = cur.State
	}
	var (
		once sync.Once
		info protocol.HarnessInfo
		oerr error
	)
	op := func() (protocol.HarnessInfo, error) {
		once.Do(func() { info, oerr = callLifecycle(c, verb, name, forDur) })
		return info, oerr
	}

	st := newLifecycleStyle()
	m := newSingleLifecycleModel(verb, name, before, op, st)
	if _, err := tea.NewProgram(m).Run(); err != nil || (!m.done && !m.interrupted) {
		m.info, m.err = op()
		m.done = true
	}
	if !m.done {
		fmt.Fprint(os.Stdout, staleFrameCleanup)
		return fmt.Errorf("interrupted before the daemon answered; the %s of %s may still complete — check: harness describe %s", verb, name, name)
	}
	if m.err != nil {
		fmt.Fprint(os.Stdout, staleFrameCleanup)
		return m.err
	}
	writeFinalRecord(os.Stdout, st.renderOutcome(lifecycleOutcome{
		action: verbPast(verb),
		before: before,
		info:   m.info,
	}))
	return nil
}

// --- the static one-liners --------------------------------------------------

// ok renders the leading success mark every one-liner shares.
func (s lifecycleStyle) ok() string { return s.done.Render(cliui.LevelSuccess.Glyph()) }

// renderUseProfile: `✓ profile work active  3 harnesses · weekday agents`.
func (s lifecycleStyle) renderUseProfile(name string, ps []protocol.ProfileInfo) string {
	var ctx []string
	for _, p := range ps {
		if p.Name != name {
			continue
		}
		ctx = append(ctx, plural(len(p.Harnesses), "harness", "harnesses"))
		if p.Autostart {
			ctx = append(ctx, "autostart")
		}
		if p.Description != "" {
			ctx = append(ctx, p.Description)
		}
	}
	return fmt.Sprintf("%s %s %s %s%s\n", s.ok(), s.faint.Render("profile"),
		s.accent.Render(name), s.name.Render("active"), s.tail(ctx))
}

// renderReload: `✓ reloaded  12 harnesses` over a per-state tally in glyph +
// colour, the same summary the cockpit header gives.
func (s lifecycleStyle) renderReload(hs []protocol.HarnessInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  %s\n", s.ok(), s.name.Bold(true).Render("reloaded"),
		s.faint.Render(plural(len(hs), "harness", "harnesses")))
	if tally := s.stateTally(hs); tally != "" {
		fmt.Fprintf(&b, "  %s\n", tally)
	}
	return b.String()
}

// stateTally renders "○ 3 stopped  ● 8 running  ✖ 1 failed" in SPEC-0003
// canonical state order, skipping states nobody is in.
func (s lifecycleStyle) stateTally(hs []protocol.HarnessInfo) string {
	counts := map[core.State]int{}
	unknown := 0
	for _, h := range hs {
		st := core.State(h.State)
		if !st.Valid() {
			unknown++
			continue
		}
		counts[st]++
	}
	var parts []string
	for _, st := range core.States {
		if n := counts[st]; n > 0 {
			parts = append(parts, s.th.StateStyle(st).Render(fmt.Sprintf("%s %d %s", s.th.Glyph(st), n, st)))
		}
	}
	if unknown > 0 {
		parts = append(parts, s.faint.Render(fmt.Sprintf("· %d other", unknown)))
	}
	return strings.Join(parts, "  ")
}

// renderDown: `✓ project api down  2 harnesses stopped and deregistered` over
// the names that left.
func (s lifecycleStyle) renderDown(d protocol.ProjectDownData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s %s  %s\n", s.ok(), s.faint.Render("project"),
		s.accent.Render(d.Project), s.name.Render("down"),
		s.faint.Render(plural(len(d.Removed), "harness", "harnesses")+" stopped and deregistered"))
	for _, n := range d.Removed {
		fmt.Fprintf(&b, "  %s %s\n", s.th.RenderGlyph(core.StateStopped), s.faint.Render(n))
	}
	return b.String()
}

// renderRemove: `✓ removed api/web  project api`.
func (s lifecycleStyle) renderRemove(d protocol.RemoveData) string {
	var ctx []string
	if d.Project != "" {
		ctx = append(ctx, "project "+d.Project)
	}
	return fmt.Sprintf("%s %s %s%s\n", s.ok(), s.faint.Render("removed"),
		s.name.Bold(true).Render(d.Name), s.tail(ctx))
}

// renderTrigger styles what the daemon did with a trigger: started is a
// success, queued is a wait (amber), skipped is a warning — the run did not
// happen. The words are triggerLine's, so the styled and plain forms never
// disagree on what happened.
func (s lifecycleStyle) renderTrigger(td protocol.TriggerData) string {
	name := s.name.Bold(true).Render(td.Name)
	switch td.Decision {
	case protocol.TriggerStarted:
		what := "started"
		if td.Run != nil {
			what = fmt.Sprintf("started run #%d", td.Run.RunID)
		}
		return fmt.Sprintf("%s %s  %s\n", s.ok(), name, s.done.Render(what))
	case protocol.TriggerQueued:
		return fmt.Sprintf("%s %s  %s%s\n", s.warn.Render("◌"), name,
			s.warn.Render("queued"), s.tail([]string{"a run is in flight; this one waits behind it (on_overlap = queue)"}))
	}
	ctx := []string{"a run is in flight"}
	if td.Run != nil {
		ctx = append(ctx, fmt.Sprintf("recorded as run #%d", td.Run.RunID))
	}
	return fmt.Sprintf("%s %s  %s%s\n", s.warn.Render(cliui.LevelWarn.Glyph()), name,
		s.warn.Render("skipped"), s.tail(ctx))
}

// renderFinish styles how a followed run ended: success in mint, anything
// else in the failure colour.
func (s lifecycleStyle) renderFinish(name string, r protocol.RunInfo) string {
	mark, outcome := s.ok(), s.done.Render(r.Outcome)
	if r.Outcome != "success" {
		mark = s.failed.Render(cliui.LevelError.Glyph())
		outcome = s.failed.Render(r.Outcome)
	}
	// finishLine is "<name>: run #N <outcome> (exit N) after 3m"; keep its
	// suffix verbatim as faint context.
	plain := finishLine(name, r)
	suffix := strings.TrimPrefix(plain, fmt.Sprintf("%s: run #%d %s", name, r.RunID, r.Outcome))
	return fmt.Sprintf("%s %s  %s %s%s\n", mark, s.name.Bold(true).Render(name),
		s.faint.Render(fmt.Sprintf("run #%d", r.RunID)), outcome, s.faint.Render(suffix))
}

// renderDaemonStopping: `◌ daemon stopping  pid 4242` in the transient cyan.
func (s lifecycleStyle) renderDaemonStopping(pid int) string {
	return fmt.Sprintf("%s %s %s  %s\n", s.th.RenderGlyph(core.StateStopping),
		s.name.Bold(true).Render("daemon"), s.th.StateStyle(core.StateStopping).Render("stopping"),
		s.faint.Render(fmt.Sprintf("pid %d", pid)))
}

// tail renders faint trailing context, two spaces off the headline.
func (s lifecycleStyle) tail(ctx []string) string {
	if len(ctx) == 0 {
		return ""
	}
	return "  " + s.faint.Render(strings.Join(ctx, " · "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
