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

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// View implements tea.Model. Bubble Tea v2 made terminal features declarative:
// the alt screen and mouse reporting are fields on the returned View, re-asserted
// every frame, rather than startup options plus Enter/Exit commands. That is what
// makes shift-passthrough (#49) a plain state flip — m.mouseReleased simply drops
// MouseMode for as long as it is set, instead of racing a DisableMouse command
// against the events still queued behind it.
func (m *Model) View() tea.View {
	v := tea.NewView(m.content())
	v.AltScreen = true
	if !m.mouseReleased {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// content renders the frame body — everything below the terminal-feature layer
// that View declares.
func (m *Model) content() string {
	if m.quitting {
		return ""
	}
	switch m.conn {
	case startNoDaemon:
		return m.viewNoDaemon()
	case startOtherErr:
		return m.theme.Banner().Render("harness: "+errString(m.connErr)) + "\n"
	}
	// Attach-only mode (`harness attach <name>`): while we're still connecting
	// or waiting for the first state refresh to resolve the named harness,
	// show a minimal "connecting" surface instead of flashing the dashboard.
	if m.opts.AttachOnly != "" && m.att == nil && !m.reconn {
		return m.overlayBox("Attaching…", fmt.Sprintf("Opening a session to %s…", m.opts.AttachOnly))
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
// paneInner converts a pane's target OUTER width (the column budget it occupies
// on the dashboard row) into the content width to hand lipgloss: the rounded
// Box border occupies one column on each side, so the content must be 2 narrower
// or the rendered pane overflows its budget (and, summed across both panes,
// pushes the dashboard past the terminal edge and wraps).
func paneInner(w int) int {
	return maxInt(1, w-2)
}

// paneBorderRows is the vertical cost of a pane's rounded Box border. Panes
// are handed their CONTENT budget by viewDashboard and render content + this
// many border rows; the borders are exactly what bodyHeight()'s
// headerRows/footerRows over-reservation pays for, so a pane must always pad
// out to content+paneBorderRows — sparse content included (#180) — and its
// content must clamp to the budget, because lipgloss pads up but never
// truncates down (#179).
const paneBorderRows = 2

func (m *Model) viewDashboard() string {
	header := m.viewHeader()
	footer := m.viewFooter()

	body := m.bodyHeight()

	// Below the split cockpit's floor — header (2) + bordered pane (3:
	// one content row plus its borders) + footer (1) = 6 rows — no pane layout
	// can land on exactly m.h, so degrade to a single summary row and pad or
	// truncate the frame to the window (#179: the alt screen scrolls on any
	// excess line, which is the failure #144 banned).
	if m.h < 6 {
		parts := []string{header}
		if m.banner != "" {
			parts = append(parts, m.theme.Banner().Render("⚠ "+m.banner))
		}
		if m.skewNotice != "" {
			parts = append(parts, m.theme.Banner().Render("⚠ "+m.skewNotice))
		}
		parts = append(parts,
			m.theme.Faint().Render(fmt.Sprintf("%d harnesses · terminal too short for the split view", len(m.visible()))),
			footer)
		lines := strings.Split(strings.Join(parts, "\n"), "\n")
		for len(lines) < m.h {
			lines = append(lines, "")
		}
		return strings.Join(lines[:m.h], "\n")
	}

	listW := m.w * 2 / 5
	if listW < 24 {
		listW = min(m.w, 24)
	}
	peekW := m.w - listW - 1
	if peekW < 1 {
		peekW = 1
	}

	// The panes receive `body` as their CONTENT budget and render body+2 rows
	// total (see viewList/viewPeek): the +2 of borders is exactly the
	// header/footer over-reservation in bodyHeight(), so sparse content must
	// still pad out to it or the frame lands short of m.h (#180).
	list := m.viewList(listW, body)
	peek := m.viewPeek(peekW, body)
	cols := lipgloss.JoinHorizontal(lipgloss.Top, list, " ", peek)

	parts := []string{header}
	if m.banner != "" {
		parts = append(parts, m.theme.Banner().Render("⚠ "+m.banner))
	}
	if m.skewNotice != "" {
		parts = append(parts, m.theme.Banner().Render("⚠ "+m.skewNotice+" (esc dismisses)"))
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
		// The empty state flows through the same clamps below: its multi-line
		// text used to bypass them, wrap at narrow pane widths, and overflow
		// the frame (#179's dashboard-empty case).
		empty := emptyStateText(profileName(m.profiles, m.showAll))
		lines = append(lines, strings.Split(m.theme.Faint().Render(empty), "\n")...)
	}

	// Viewport: render only the window [listOffset..] that fits within the
	// box interior. Box().Height(h) sets content height; the border is drawn
	// outside it and is already paid for by bodyHeight's header/footer
	// over-reservation. The title + blank line consume 2 rows.
	maxContent := h
	contentBudget := maxContent - 2 // title + blank after title

	offset := m.listOffset
	if offset < 0 || offset >= len(v) {
		offset = 0
	}

	// Count rendered lines (degraded rows take 2) to compare against the
	// budget — comparing harness count to row budget is wrong when degraded
	// rows are present (issue #148 "rows vs harnesses").
	renderedLinesOf := func(endIdx int) int {
		n := 2 // title + blank
		for i := offset; i < endIdx && i < len(v); i++ {
			n++
			if isDegraded(v[i]) {
				n++
			}
		}
		return n
	}
	totalRenderedLines := renderedLinesOf(len(v))

	// Determine if we need a scroll indicator before rendering, so we can
	// reserve its row in the budget.
	needsIndicator := offset > 0 || totalRenderedLines > maxContent
	if needsIndicator {
		contentBudget--
	}
	if contentBudget < 1 {
		contentBudget = 1
	}

	var rendered int
	lastRenderedIdx := offset - 1
	for i := offset; i < len(v); i++ {
		rowLines := 1
		if isDegraded(v[i]) {
			rowLines = 2
		}
		if rendered+rowLines > contentBudget {
			break // don't half-render a degraded row
		}
		lines = append(lines, m.renderRow(v[i], i == m.sel, w-2))
		rendered++
		if isDegraded(v[i]) {
			lines = append(lines, "   "+m.theme.StateStyle(core.StateDegraded).Render(flappingDetail(v[i])))
			rendered++
		}
		lastRenderedIdx = i
	}

	// Scroll indicators: ↑N / ↓N counts when content extends beyond the
	// viewport (SPEC-0001 "state legibility over decoration").
	aboveHarnesses := offset
	belowHarnesses := len(v) - lastRenderedIdx - 1
	if aboveHarnesses > 0 || belowHarnesses > 0 {
		indicator := ""
		if aboveHarnesses > 0 {
			indicator += fmt.Sprintf("↑%d", aboveHarnesses)
		}
		if belowHarnesses > 0 {
			if indicator != "" {
				indicator += " "
			}
			indicator += fmt.Sprintf("↓%d", belowHarnesses)
		}
		lines = append(lines, m.theme.Faint().Render(indicator))
	}

	// Clamp to box interior (issue #144 invariant).
	if maxContent < 1 {
		maxContent = 1
	}
	if len(lines) > maxContent {
		lines = lines[:maxContent]
	}
	// Clamp to box WIDTH too: a row wider than the pane interior wraps inside
	// the box, and every wrapped row is a frame row the height clamp above
	// never counted — the pane outgrows its budget and scrolls the window
	// (#179, exposed at narrow widths by the {60,8} matrix case).
	inner := paneInner(w)
	for i, ln := range lines {
		if lipgloss.Width(ln) > inner {
			lines[i] = ansi.Truncate(ln, inner, "")
		}
	}
	return m.theme.Box().Width(w).Height(h + paneBorderRows).Render(strings.Join(lines, "\n"))
}

// renderRow renders one harness row. The colored glyph leads; name, state label,
// restart marker, and next-action follow — glyph + text are always present so a
// mono terminal is fully legible (SPEC-0001 REQ "State Presentation").
func (m *Model) renderRow(h protocol.HarnessInfo, selected bool, w int) string {
	st := core.State(h.State)
	// Transient states get the live spinner frame in place of the static
	// glyph so the row reads as "alive" while the harness is booting/
	// bouncing (SPEC-0001 REQ "State Presentation": cyan + spinner).
	var glyph string
	switch st {
	case core.StateStarting, core.StateRestarting, core.StateStopping:
		glyph = m.spinner.View()
	default:
		glyph = m.theme.RenderGlyph(st)
	}
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
		return m.theme.Box().Width(w).Height(h + paneBorderRows).Render(m.theme.Faint().Render("no selection"))
	}
	tail := m.peek.text
	peekCols, peekRows := m.peek.cols, m.peek.rows
	if m.peek.name != sel.Name {
		tail, peekCols, peekRows = "", 0, 0
	}

	head := m.theme.Header().Render(sel.Name) + " " +
		m.theme.Faint().Render("live preview · read-only")

	// A prompt harness carries no configured cmd — surface the prompt, what
	// the user actually wrote (ADR-0011 spawn-time synthesis).
	what := m.theme.Faint().Render("cmd     ") + sel.Cmd
	if sel.Prompt != "" {
		what = m.theme.Faint().Render("prompt  ") + sel.Prompt
	}
	summary := []string{"", what}
	if sel.Model != "" {
		summary = append(summary, m.theme.Faint().Render("model   ")+sel.Model)
	}
	if sel.AutoAccept {
		summary = append(summary, m.theme.Faint().Render("yolo    ")+"auto_accept on")
	}
	summary = append(summary,
		m.theme.Faint().Render("backend ")+orDefault(sel.Backend, "native"),
		m.theme.Faint().Render("exit    ")+fmt.Sprintf("%d", sel.LastExitCode),
		m.theme.Faint().Render("restarts")+fmt.Sprintf(" %d", sel.RestartCount),
	)
	if sel.PID > 0 {
		summary = append(summary, m.theme.Faint().Render("pid     ")+fmt.Sprintf("%d", sel.PID))
	}

	// Derive the tail budget from the actual summary length rather than a
	// hard-coded constant (issue #144 trigger B). Layout within the content
	// height h: head(1) + blank(1) + tail(N) + summary(len(summary)).
	maxLines := h - 2 - len(summary) // head + blank-before-tail
	if maxLines < 1 {
		maxLines = 1
	}

	// Render the peek through a client-side vt emulator so full-screen TUIs
	// show their current screen rather than a transcript of every repaint
	// (issue #147). The emulator replays the raw PTY bytes and we render its
	// cell grid via renderNoCursor — inert by construction. Cached via peekCache
	// so the replay only fires when the tail or dimensions change.
	//
	// The emulator MUST be sized to the guest's authoritative viewport (carried
	// on the logs reply from the same Mux the attach plane resizes), not to the
	// pane. A tail drawn for a 156-column guest replayed into a 90-column
	// emulator wraps every line and drops cursor-addressed content into the
	// wrong cells — the smooshed peek you get after detaching from a full-window
	// attach, which is the "not 100%x100%" bug arriving through the dashboard's
	// seam instead of the attached view's. Sized correctly, the screen
	// reconstructs faithfully and the pane simply CROPS it (width below, height
	// via the bottom-anchored slice here): honestly cut beats plausibly wrong.
	// A daemon with no viewport to report (no Mux, or predating ProtoMinor 6)
	// leaves us the pane's own geometry — the historical behaviour.
	knownViewport := peekCols > 0 && peekRows > 0
	if !knownViewport {
		peekCols, peekRows = paneInner(w), maxLines+1
	}
	screenStr := m.peekCache.render(tail, peekCols, peekRows)
	tailLines := trimBlankTail(splitLines(screenStr))
	if len(tailLines) > maxLines {
		tailLines = tailLines[len(tailLines)-maxLines:]
	}

	// When the guest doesn't fit the pane, name its viewport in the head so the
	// missing rows and columns read as a crop rather than a broken render — the
	// same "colsxrows" describe reports (#183), and the number to size the
	// window against. Only when the viewport is the guest's: the fallback
	// geometry is the pane's own, so there is nothing to report.
	if knownViewport && (peekCols > paneInner(w) || peekRows > maxLines) {
		head += m.theme.Faint().Render(fmt.Sprintf(" · %d×%d cropped", peekCols, peekRows))
	}

	// Content assembly is height-clamped from the bottom: the summary block
	// is fixed-length, and on a short body budget (#179) lipgloss would
	// otherwise happily render it all and push the frame past m.h. The head
	// and the live tail win; the summary gives way first.
	lines := []string{head, ""}
	lines = append(lines, tailLines...)
	lines = append(lines, summary...)
	if len(lines) > h {
		lines = lines[:h]
	}
	// And width-clamped: a wrapped line is a hidden extra row (see viewList).
	inner := paneInner(w)
	for i, ln := range lines {
		if lipgloss.Width(ln) > inner {
			lines[i] = ansi.Truncate(ln, inner, "")
		}
	}
	return m.theme.Box().Width(w).Height(h + paneBorderRows).Render(strings.Join(lines, "\n"))
}

// viewFooter is the key bar (SPEC-0001: `?` expands to full help).
func (m *Model) viewFooter() string {
	bar := m.help.ShortHelpView(m.keys.ShortHelp())
	// Hard clamp, matching the attached status bar below. Bubbles v2's help
	// stops emitting items only when its ellipsis ALSO fits in the columns
	// left; when it doesn't, it appends the overflowing item anyway (v1 broke
	// out unconditionally). That leaves the bar wider than the window, where
	// it wraps to a second physical row and scrolls the alt screen — so bound
	// it here rather than trusting the widget's own truncation.
	return ansi.Truncate(bar, m.w, "")
}

// --- attached -------------------------------------------------------------

// viewAttached renders the embedded terminal filling the window with a
// 1-line status bar held back at the bottom (SPEC-0001 REQ "Attached Mode":
// full-attention live terminal; the bar carries identity + key bindings).
//
// Layout: the terminal body is rendered first (sized to m.h-1 rows via
// attachViewport), then the status bar is appended below. Total output is
// exactly m.h lines so Bubble Tea doesn't scroll.
func (m *Model) viewAttached() string {
	if m.att == nil {
		return m.viewDashboard()
	}
	var body string
	if m.att.substate == substateScrollback {
		body = m.viewScrollback()
	} else {
		body = m.att.view.render()
	}
	bar := m.viewStatusBar()
	return body + "\n" + bar
}

// viewStatusBar renders the 1-line bottom bar: logo chip · harness identity +
// state · read-only badge on the left; the compact attached keymap (hop,
// scrollback, detach, help) on the right. Built on the Bubbles help registry
// so the bindings never drift from the `?` full-help view (SPEC-0001 REQ
// "Keybinding Registry").
func (m *Model) viewStatusBar() string {
	// Left: logo chip + "attached: <name> <state>" [+ ro badge].
	logo := m.theme.LogoChip()
	v := m.visible()
	pos := selectByName(v, m.att.name)
	posText := ""
	if pos >= 0 {
		posText = fmt.Sprintf(" · %d of %d", pos+1, len(v))
	}
	stateText := ""
	if h := m.harnessByName(m.att.name); h != nil {
		stateText = " " + m.theme.RenderState(core.State(h.State))
	}
	// The hop flash reverses the identity segment briefly.
	identStyle := m.theme.Ribbon()
	if m.att.flash > 0 {
		identStyle = identStyle.Reverse(true)
	}
	ident := identStyle.Render(fmt.Sprintf(" attached: %s%s ", m.att.name, posText))
	badge := ""
	if m.att.readOnly() {
		badge = " " + m.theme.ReadOnlyBadge()
	}
	// Visual feedback when the Ctrl-b prefix is armed: a highlighted "^b"
	// prompt tells the user the next key will be a harness command (not
	// forwarded to the agent).
	prefixHint := ""
	if m.att.prefixArmed {
		prefixHint = " " + m.theme.Header().Render("^b")
	}
	left := logo + " " + ident + stateText + badge + prefixHint
	lw := lipgloss.Width(left)

	// Right: compact attached-mode help (hop / scrollback / detach / ?). Budget
	// it to whatever space remains after the identity segment so the Bubbles
	// help view self-truncates (with its "…" ellipsis) instead of overflowing.
	// This is the crux of the full-window fix: an unbudgeted bar renders wider
	// than the terminal on a narrow window, wraps to a second physical row, and
	// that extra row scrolls the alt-screen — shoving the embedded terminal up
	// so it no longer fills the window. A width-bounded bar keeps the output at
	// exactly m.h lines of m.w columns. We copy m.help (a value) so setting
	// Width here doesn't leak into the dashboard's full-width footer help.
	avail := m.w - lw - 2 // reserve the 2-space lead-in the help gets below
	if avail < 0 {
		avail = 0
	}
	hp := m.help
	hp.SetWidth(avail)
	right := m.theme.Faint().Render("  ") + hp.ShortHelpView(m.keys.AttachedShortHelp())
	rw := lipgloss.Width(right)

	// Pad the left segment so the right help hugs the right edge and the bar
	// spans the full terminal width (a true status bar, not inline text).
	gap := m.w - lw - rw
	if gap < 0 {
		gap = 0
	}
	bar := left + strings.Repeat(" ", gap) + right
	// Final hard clamp: on a pathologically narrow terminal the identity
	// segment alone can exceed m.w. Truncate (ANSI-aware) so the bar is never
	// wider than the window and can't wrap.
	return ansi.Truncate(bar, m.w, "")
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
		// A line wider than the window wraps onto a second physical row, which
		// scrolls the alt-screen and breaks the "exactly m.h lines" invariant
		// viewAttached depends on — the same failure the full-window status bar
		// fix addressed, arriving from the content side instead.
		ln := ansi.Truncate(sb.lines[i], m.w, "")
		if i == sb.currentMatchLine() {
			// Strip the line's own escapes before highlighting: embedded SGR
			// runs (dense in appended frame lines) would otherwise interrupt
			// the Selected style mid-line, leaving the current match patchy or
			// invisible. A solid highlight beats preserved colors here.
			ln = m.theme.Selected().Render(ansi.Strip(ln))
		}
		lines = append(lines, ln)
	}
	status := m.theme.Faint().Render(fmt.Sprintf("-- SCROLLBACK %d-%d/%d --", sb.top, end, len(sb.lines)))
	if m.att.searchOn {
		status = m.att.search.View()
	} else if sb.term != "" {
		status = m.theme.Faint().Render(fmt.Sprintf("/%s  %d matches (n/N)", sb.term, len(sb.matches)))
	}
	// The status row must obey the same width clamp as the content lines: a
	// long search term (or match echo) would wrap onto a second physical row
	// and scroll the alt-screen.
	return strings.Join(lines, "\n") + "\n" + ansi.Truncate(status, m.w, "")
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
	style := m.theme.Box().Padding(0, 1)
	// Never let an overlay exceed the terminal width: a box wider than m.w
	// wraps its right edge to a second physical row and scrolls everything. If
	// the natural content is too wide, constrain the content width (border 2 +
	// padding 2 = 4) so lipgloss wraps inside the box; a final MaxWidth clamps
	// against any rounding. Only when the width is known (m.w > 0) — before the
	// first WindowSizeMsg it's 0, and clamping to that would erase the overlay.
	if m.w > 0 {
		if maxW := m.w - 4; maxW > 0 && lipgloss.Width(inner) > maxW {
			style = style.Width(maxW)
		}
		style = style.MaxWidth(m.w)
	}
	// Nor may it exceed the terminal HEIGHT (#179): the box renders title +
	// blank + body + borders + padding, and lipgloss pads up but never
	// truncates down, so a long body (the palette's ten rows, the full help)
	// scrolls the alt screen in a short window. Give the body a line budget —
	// title, blank, and the 4 rows of border+padding come off the top — and
	// drop body lines from the bottom to fit. A too-short window keeps the
	// title and as much body as fits, which is all it can honestly show.
	if m.h > 0 {
		if budget := m.h - 4; budget >= 0 {
			lines := strings.Split(body, "\n")
			if len(lines) > budget {
				lines = lines[:budget]
				if budget > 0 {
					lines[budget-1] += " …"
				}
			}
			inner = m.theme.Header().Render(title) + "\n\n" + strings.Join(lines, "\n")
		}
	}
	return style.Render(inner)
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
