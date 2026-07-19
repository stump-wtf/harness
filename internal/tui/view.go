package tui

// Governing: SPEC-0001 REQ "Dashboard" (split cockpit: list + live peek +
// header/footer), REQ "Attached Mode" (embedded terminal + thin ribbon +
// read-only badge), REQ "State Presentation" (paired glyph+color rows), REQ
// "Zero And Error States", and the overlays. Layout follows docs/design/
// (day.png split cockpit, hop.png attached ribbon). Reuses core.State glyphs via
// the theme.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// View implements tea.Model.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	switch m.conn {
	case startNoDaemon:
		return m.viewNoDaemon()
	case startOtherErr:
		return m.theme.Banner().Render("harness: "+errString(m.connErr)) + "\n"
	}
	if m.reconn {
		return m.overlayBox("Reconnecting…", "The daemon connection dropped — your harnesses keep running.\nRetrying…")
	}

	var base string
	if m.mode == modeAttached {
		base = m.viewAttached()
	} else {
		base = m.viewDashboard()
	}

	switch m.overlay {
	case overlayHelp:
		return m.overlayBox("Keymap", m.help.View(m.keys))
	case overlayPalette:
		return m.viewPalette()
	case overlaySearch:
		return base // the search input renders inline in the dashboard footer
	case overlayProfile:
		return m.viewProfileSwitcher()
	case overlayForm:
		if m.form != nil {
			return m.overlayBox(formTitle(m.editing), m.form.View())
		}
	case overlayConfirm:
		return m.viewConfirm()
	}
	return base
}

// --- dashboard ------------------------------------------------------------

// viewDashboard renders the split cockpit (SPEC-0001 REQ "Dashboard").
func (m *Model) viewDashboard() string {
	header := m.viewHeader()
	footer := m.viewFooter()

	body := m.bodyHeight()
	listW := m.w * 2 / 5
	if listW < 24 {
		listW = min(m.w, 24)
	}
	peekW := m.w - listW - 1
	if peekW < 1 {
		peekW = 1
	}

	list := m.viewList(listW, body)
	peek := m.viewPeek(peekW, body)
	cols := lipgloss.JoinHorizontal(lipgloss.Top, list, " ", peek)

	parts := []string{header}
	if m.banner != "" {
		parts = append(parts, m.theme.Banner().Render("⚠ "+m.banner))
	}
	parts = append(parts, cols)
	if m.overlay == overlaySearch {
		parts = append(parts, m.search.View())
	} else if m.status != "" {
		parts = append(parts, m.theme.Faint().Render(m.status))
	}
	parts = append(parts, footer)
	return strings.Join(parts, "\n")
}

// viewHeader renders "harness · profile: X · daemon: local" (SPEC-0001 header).
func (m *Model) viewHeader() string {
	profile := "all"
	if p := activeProfile(m.profiles); p != nil {
		profile = p.Name
	}
	ident := m.daemonIdentity()
	left := m.theme.Header().Render("harness")
	mid := m.theme.Faint().Render("  profile: ") + m.theme.Header().Render(profile)
	right := m.theme.Faint().Render("  daemon: ") + m.theme.StateStyle(core.StateRunning).Render(ident)
	return left + mid + right + "\n" + m.theme.Faint().Render(strings.Repeat("─", maxInt(1, m.w)))
}

// daemonIdentity is "local" or "user@host" (SPEC-0001). We report local for the
// Unix-socket transport; a remote identity would come from the daemon info.
func (m *Model) daemonIdentity() string {
	if m.daemon.Version != "" {
		return "local · " + m.daemon.Version
	}
	return "local"
}

// viewList renders the harness rows (SPEC-0001 REQ "Dashboard" / "State
// Presentation": glyph/name/state/↻/uptime, degraded rows expanded).
func (m *Model) viewList(w, h int) string {
	v := m.visible()
	title := m.theme.Faint().Render(strings.ToUpper("harnesses"))
	if p := activeProfile(m.profiles); p != nil && !m.showAll {
		title += m.theme.Faint().Render(" · " + p.Name)
	}
	lines := []string{title, ""}

	if len(v) == 0 {
		empty := emptyStateText(profileName(m.profiles, m.showAll))
		lines = append(lines, m.theme.Faint().Render(empty))
		return m.theme.Box().Width(w).Height(h).Render(strings.Join(lines, "\n"))
	}

	for i, hnfo := range v {
		lines = append(lines, m.renderRow(hnfo, i == m.sel, w-2))
		if isDegraded(hnfo) {
			lines = append(lines, "   "+m.theme.StateStyle(core.StateDegraded).Render(flappingDetail(hnfo)))
		}
	}
	return m.theme.Box().Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

// renderRow renders one harness row. The colored glyph leads; name, state label,
// restart marker, and next-action follow — glyph + text are always present so a
// mono terminal is fully legible (SPEC-0001 REQ "State Presentation").
func (m *Model) renderRow(h protocol.HarnessInfo, selected bool, w int) string {
	st := core.State(h.State)
	glyph := m.theme.RenderGlyph(st)
	name := h.Name
	state := string(h.State)
	rest := restartMarker(h.RestartCount)
	next := nextActionText(h)

	right := strings.TrimSpace(rest + " " + next)
	left := fmt.Sprintf("%s %s", glyph, name)
	label := m.theme.Faint().Render(state)
	line := left + "  " + label
	if right != "" {
		line += "  " + m.theme.Faint().Render(right)
	}
	if selected {
		marker := m.theme.Header().Render("›")
		return marker + " " + line
	}
	return "  " + line
}

// viewPeek renders the live read-only tail + config summary (SPEC-0001 REQ
// "Dashboard": "live read-only tail ... plus its config summary").
func (m *Model) viewPeek(w, h int) string {
	sel, ok := m.selectedHarness()
	if !ok {
		return m.theme.Box().Width(w).Height(h).Render(m.theme.Faint().Render("no selection"))
	}
	head := m.theme.Header().Render(sel.Name) + " " +
		m.theme.Faint().Render("live preview · read-only")

	tail := m.peek.text
	if m.peek.name != sel.Name {
		tail = ""
	}
	tailLines := splitLines(tail)
	maxLines := h - 8
	if maxLines < 1 {
		maxLines = 1
	}
	if len(tailLines) > maxLines {
		tailLines = tailLines[len(tailLines)-maxLines:]
	}

	summary := []string{
		"",
		m.theme.Faint().Render("cmd     ") + sel.Cmd,
		m.theme.Faint().Render("backend ") + orDefault(sel.Backend, "native"),
		m.theme.Faint().Render("exit    ") + fmt.Sprintf("%d", sel.LastExitCode),
		m.theme.Faint().Render("restarts") + fmt.Sprintf(" %d", sel.RestartCount),
	}
	if sel.PID > 0 {
		summary = append(summary, m.theme.Faint().Render("pid     ")+fmt.Sprintf("%d", sel.PID))
	}

	content := head + "\n\n" + strings.Join(tailLines, "\n") + "\n" + strings.Join(summary, "\n")
	return m.theme.Box().Width(w).Height(h).Render(content)
}

// viewFooter is the key bar (SPEC-0001: `?` expands to full help).
func (m *Model) viewFooter() string {
	return m.help.ShortHelpView(m.keys.ShortHelp())
}

// --- attached -------------------------------------------------------------

// viewAttached renders the embedded terminal with the thin status ribbon
// (SPEC-0001 REQ "Attached Mode" / "Scrollback Substate").
func (m *Model) viewAttached() string {
	if m.att == nil {
		return m.viewDashboard()
	}
	ribbon := m.viewRibbon()
	var body string
	if m.att.substate == substateScrollback {
		body = m.viewScrollback()
	} else {
		body = m.att.view.render()
	}
	return ribbon + "\n" + body
}

// viewRibbon renders the thin status ribbon (harness · state · detach hint · hop
// affordance), flashing briefly after a hop (SPEC-0001 REQ "Harness Hop").
func (m *Model) viewRibbon() string {
	v := m.visible()
	pos := selectByName(v, m.att.name)
	posText := ""
	if pos >= 0 {
		posText = fmt.Sprintf(" · %d/%d", pos+1, len(v))
	}
	state := ""
	if h := m.harnessByName(m.att.name); h != nil {
		state = " " + m.theme.RenderState(core.State(h.State))
	}
	badge := ""
	if m.att.readOnly() {
		badge = "  " + m.theme.ReadOnlyBadge()
	}
	style := m.theme.Ribbon()
	if m.att.flash > 0 {
		style = style.Reverse(true) // the ribbon-flash on hop
	}
	left := style.Render(fmt.Sprintf(" attached: %s%s ", m.att.name, posText))
	hint := m.theme.Faint().Render("  [ prev · next ]  ·  ^b [ scrollback  ·  esc esc detach")
	return left + state + badge + hint
}

// viewScrollback renders the frozen scrollback view with the search line
// (SPEC-0001 REQ "Scrollback Substate").
func (m *Model) viewScrollback() string {
	sb := m.att.scroll
	end := sb.top + sb.height
	if end > len(sb.lines) {
		end = len(sb.lines)
	}
	var lines []string
	for i := sb.top; i < end; i++ {
		ln := sb.lines[i]
		if i == sb.currentMatchLine() {
			ln = m.theme.Selected().Render(ln)
		}
		lines = append(lines, ln)
	}
	status := m.theme.Faint().Render(fmt.Sprintf("-- SCROLLBACK %d-%d/%d --", sb.top, end, len(sb.lines)))
	if m.att.searchOn {
		status = m.att.search.View()
	} else if sb.term != "" {
		status = m.theme.Faint().Render(fmt.Sprintf("/%s  %d matches (n/N)", sb.term, len(sb.matches)))
	}
	return strings.Join(lines, "\n") + "\n" + status
}

// --- overlays -------------------------------------------------------------

// viewPalette renders the command palette (SPEC-0001 REQ "Command Palette").
func (m *Model) viewPalette() string {
	var rows []string
	rows = append(rows, m.pal.input.View(), "")
	limit := 10
	for i, c := range m.pal.filtered {
		if i >= limit {
			break
		}
		line := c.Display
		if i == m.pal.sel {
			line = m.theme.Selected().Render("› " + line)
		} else {
			line = "  " + m.theme.Faint().Render(line)
		}
		rows = append(rows, line)
	}
	if len(m.pal.filtered) == 0 {
		rows = append(rows, m.theme.Faint().Render("  no matches"))
	}
	return m.overlayBox("Command palette", strings.Join(rows, "\n"))
}

// viewProfileSwitcher renders the profile picker / start-stopped prompt
// (SPEC-0001 REQ "Profile Switcher").
func (m *Model) viewProfileSwitcher() string {
	if m.prof.askStart {
		body := fmt.Sprintf("Switch to %s and start its stopped harnesses?\n\n  y start stopped   ·   n just switch   ·   esc cancel", m.prof.pending)
		return m.overlayBox("Profile", body)
	}
	var rows []string
	for i, p := range m.profiles {
		line := fmt.Sprintf("%s  %s (%d)", p.Name, p.Description, len(p.Harnesses))
		if i == m.prof.sel {
			line = m.theme.Selected().Render("› " + line)
		} else {
			line = "  " + line
		}
		rows = append(rows, line)
	}
	if len(m.profiles) == 0 {
		rows = append(rows, m.theme.Faint().Render("no profiles defined"))
	}
	return m.overlayBox("Switch profile", strings.Join(rows, "\n"))
}

// viewConfirm renders the confirm dialog (SPEC-0001 REQ "Confirmation Guards").
func (m *Model) viewConfirm() string {
	body := m.confirm.prompt + "\n\n  " +
		m.theme.StateStyle(core.StateFailed).Render("y / ↵ confirm") + "    esc cancel"
	return m.overlayBox("Confirm", body)
}

// viewNoDaemon renders the no-daemon inline offer (SPEC-0001 scenario "Daemon
// not running").
func (m *Model) viewNoDaemon() string {
	return m.overlayBox("No daemon", noDaemonText(m.opts.Socket))
}

// overlayBox renders a titled bordered box (the Lip Gloss signature).
func (m *Model) overlayBox(title, body string) string {
	inner := m.theme.Header().Render(title) + "\n\n" + body
	return m.theme.Box().Padding(0, 1).Render(inner)
}

// --- small helpers --------------------------------------------------------

func formTitle(editing bool) string {
	if editing {
		return "Edit harness"
	}
	return "New harness"
}

func profileName(profiles []protocol.ProfileInfo, showAll bool) string {
	if showAll {
		return ""
	}
	if p := activeProfile(profiles); p != nil {
		return p.Name
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
