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
	} else if m.mode == modeChatroom {
		base = m.viewChatroom()
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
		return m.overlayModal(base, m.viewConfirm())
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

// viewChatroom renders the chatroom view (ADR-0015, SPEC-0015). The chatroom
// model owns its own layout; this is a thin pass-through that clips to the
// terminal geometry the same way the dashboard does.
func (m *Model) viewChatroom() string {
	if m.chatroom == nil {
		return m.overlayBox("Chatroom", "Initializing chatroom…")
	}
	return m.chatroom.View()
}

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

	listW := m.listPaneWidth()
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

// minListWidth is the narrowest the harness list may be squeezed to before the
// dashboard would rather clip it than shrink it further.
const minListWidth = 24

// paneCeiling is the widest listPaneWidth will let the list pane grow: a third
// of the window, never below the floor and never wider than the window itself.
//
// Separated out because activityGutter has to answer "can this window afford
// the activity column at all?" before listPaneWidth has run, and answering it
// from a second copy of these clamps is a second thing to drift.
func (m *Model) paneCeiling() int {
	c := m.w / 3
	if c < minListWidth {
		c = minListWidth
	}
	if m.w > 0 && c > m.w {
		c = m.w
	}
	return c
}

// listPaneWidth sizes the list pane to its CONTENT rather than to a fixed
// fraction of the window (#199).
//
// The old `m.w * 2 / 5` reserved two fifths of the terminal whether the list
// needed them or not: on a wide window that is a column of dead space, and
// every one of those columns is taken from the live preview beside it — the
// one pane that can always use more. Measuring the rows instead means a
// dashboard of short names collapses to a narrow rail, while a long cmd path
// or a `project/harness` name gets the room it actually needs.
//
// Bounded on both ends. The floor keeps a nearly-empty list legible; the
// ceiling (a third of the window) keeps one pathological row — a 300-character
// prompt — from swallowing the preview, and is what metaLine's elision budget
// bottoms out against.
func (m *Model) listPaneWidth() int {
	natural := lipgloss.Width(m.listTitle())
	measure := func(s string) {
		if w := lipgloss.Width(s); w > natural {
			natural = w
		}
	}
	v := m.visible()
	gutter := m.activityGutter()
	for _, h := range v {
		measure(m.renderRow(h, true))
		what, rest := harnessMeta(h)
		// Measure the line renderMetaRow will actually draw, activity gutter
		// included. Measuring it without was the flicker: the pane was sized
		// for a line that did not exist, so every rendered meta row came back
		// over budget and metaLine elided the cmd path by however many columns
		// the current action happened to occupy.
		measure(strings.Repeat(" ", metaIndent+gutter) + metaLine(what, rest, 0))
	}
	if len(v) == 0 {
		// The zero-state is the only content the pane has; sizing to the rows
		// (there are none) would clamp it to the floor and wrap its text.
		for _, ln := range strings.Split(emptyStateText(profileName(m.profiles, m.showAll)), "\n") {
			measure(ln)
		}
	}

	w := natural + 2 // the rounded Box border, one column each side
	if ceiling := m.paneCeiling(); w > ceiling {
		w = ceiling
	}
	if w < minListWidth {
		w = minListWidth
	}
	// Never wider than the window itself: on a terminal narrower than the
	// floor the list takes what there is and the peek collapses to its own
	// 1-column minimum (viewDashboard), which is all either can honestly do.
	if w > m.w && m.w > 0 {
		w = m.w
	}
	return w
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
// listTitle is the list pane's heading — "HARNESSES", plus the active profile
// when the view is filtered to one. Split out of viewList so listPaneWidth can
// measure it without rendering the whole pane.
func (m *Model) listTitle() string {
	title := m.theme.Faint().Render(strings.ToUpper("harnesses"))
	if p := activeProfile(m.profiles); p != nil && !m.showAll {
		title += m.theme.Faint().Render(" · " + p.Name)
	}
	return title
}

func (m *Model) viewList(w, h int) string {
	v := m.visible()
	lines := []string{m.listTitle(), ""}

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

	// Count rendered lines to compare against the budget — comparing harness
	// count to row budget is wrong when a row is taller than one line (issue
	// #148 "rows vs harnesses"), which since #199 every row is.
	totalRenderedLines := 2 + (len(v)-offset)*listRowLines // title + blank

	// Determine if we need a scroll indicator before rendering, so we can
	// reserve its row in the budget.
	needsIndicator := offset > 0 || totalRenderedLines > maxContent
	if needsIndicator {
		contentBudget--
	}
	if contentBudget < 1 {
		contentBudget = 1
	}

	metaBudget := paneInner(w) - metaIndent
	var rendered int
	lastRenderedIdx := offset - 1
	for i := offset; i < len(v); i++ {
		if rendered+listRowLines > contentBudget {
			break // don't half-render a row, leaving a name with no metadata
		}
		lines = append(lines, m.renderRow(v[i], i == m.sel))
		lines = append(lines, m.renderMetaRow(v[i], metaBudget))
		rendered += listRowLines
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

// listRowLines is how many lines one harness occupies in the list: the state
// row, plus the metadata sub-line beneath it (#199). Every row is the same
// height — the degraded expansion folds into the metadata line rather than
// claiming a third — which is what keeps viewList's budget and
// scrollListToSel's rowOf() a multiplication instead of a scan.
const listRowLines = 2

// metaIndent aligns the metadata sub-line under the harness name: renderRow
// leads with a 2-cell selection marker, then a 1-cell glyph and a space.
const metaIndent = 4

// minWhatWidth is the least room worth giving the elided cmd/prompt. Below it
// the field is all ellipsis and no information, so metaLine drops it and
// spends the columns on the facts instead.
const minWhatWidth = 8

// renderRow renders one harness row. The colored glyph leads; name, state label,
// restart marker, and next-action follow — glyph + text are always present so a
// mono terminal is fully legible (SPEC-0001 REQ "State Presentation").
func (m *Model) renderRow(h protocol.HarnessInfo, selected bool) string {
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
	switch {
	case h.Schedule != "":
		// A scheduled one-shot is ALWAYS Enabled=false — config rejects
		// `schedule` alongside `enabled = true` — so the disabled label would
		// be wrong on every cron job, reading as "someone turned this off"
		// for a harness that fires on its own (ADR-0013).
		state += " (scheduled)"
	case !h.Enabled:
		state += " (disabled)"
	}
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

// renderMetaRow renders the metadata sub-line beneath a harness row — the
// block that used to sit at the bottom of the peek pane (#199). A degraded
// harness gets the whole line in the degraded color, which subsumes the
// separate expansion row it used to draw (SPEC-0001 REQ "Zero And Error
// States").
func (m *Model) renderMetaRow(h protocol.HarnessInfo, budget int) string {
	style := m.theme.Faint()
	if isDegraded(h) {
		style = m.theme.StateStyle(core.StateDegraded)
	}
	what, rest := harnessMeta(h)
	// The live activity field (ADR-0015) leads the line in its own fixed-width
	// column, and the facts fit into what is left. It is deliberately NOT one
	// of `rest`: metaLine spends its budget by dropping fields from the right
	// and eliding `what`, so folding a field that changes on every event into
	// that negotiation re-elides the cmd path every time an agent switches
	// tools. Reserving the column instead means the action changes and nothing
	// around it moves.
	gutter := m.activityGutter()
	if gutter == 0 {
		return strings.Repeat(" ", metaIndent) + style.Render(metaLine(what, rest, budget))
	}
	action := m.liveAction(h)
	if action == "" {
		action = strings.Repeat(" ", actionFieldWidth)
	}
	return strings.Repeat(" ", metaIndent) +
		style.Render(action+"  "+metaLine(what, rest, budget-gutter))
}

// activityGutter is the width renderMetaRow reserves for the live activity
// column, including its trailing separator — zero until the watcher has
// reported something, so a dashboard that never sees agent activity (no
// transcripts, watcher failed to start) does not pay for an empty column.
//
// It is all-or-nothing across the list rather than per row. Reserving it only
// on the rows that currently have an action would move every other row's text
// sideways as harnesses picked up and finished work.
func (m *Model) activityGutter() int {
	if len(m.lastActions) == 0 {
		return 0
	}
	// And zero on a window too narrow to pay for the facts beside it. This is
	// not a nicety: renderMetaRow hands metaLine `budget - gutter`, and a
	// budget <= 0 is metaLine's UNBOUNDED sentinel — so a pane that cannot
	// fund both does not crowd the line, it emits the whole natural-width line
	// into a 24-column pane. The list pane is capped at a third of the window,
	// so every terminal at or below ~113 columns landed there.
	//
	// The facts win the tie because they are per-harness truth that appears
	// nowhere else on the dashboard, where the action is also the entire
	// chatroom view. Dropping the column wholesale (rather than shrinking it)
	// keeps the all-or-nothing property above: this changes on resize, which
	// re-lays out the cockpit anyway, and never on an event.
	if paneInner(m.paneCeiling())-metaIndent-(actionFieldWidth+2) < minWhatWidth {
		return 0
	}
	return actionFieldWidth + 2
}

// metaLine joins a harness's metadata into one line of at most budget columns
// (budget <= 0 means unbounded — how listPaneWidth measures the natural width
// before there is a pane to fit).
//
// `what` (the cmd path or the prompt) is the only field with unbounded length,
// so it absorbs the shortfall alone: truncating the joined line from the right
// would push the pid and exit code — the operationally useful half — off the
// edge to keep a directory prefix nobody reads. It is elided from the LEFT so
// what survives is the executable's basename, or the tail of the prompt.
func metaLine(what string, rest []string, budget int) string {
	tail := strings.Join(rest, " · ")
	if what == "" {
		return fitFields(rest, budget)
	}
	if budget <= 0 {
		return what + " · " + tail
	}
	room := budget - lipgloss.Width(tail) - 3 // the " · " joining what to tail
	if lipgloss.Width(what) > room && room < minWhatWidth {
		// It does not fit whole, and what is left would be all ellipsis and no
		// information — spend every column on facts instead. The floor gates
		// the ELISION, not the field: a `/bin/sh` that fits in 7 columns still
		// renders, and listPaneWidth sized the pane expecting it to.
		return fitFields(rest, budget)
	}
	return elideLeft(what, room) + " · " + tail
}

// fitFields joins the metadata fields that fit in budget, dropping whole
// fields from the right rather than truncating through one.
//
// A field cut mid-token is worse than an absent field, because it still reads
// as a value: "exit 0" clipped to "exit" reports an exit code of nothing, and
// "native" clipped to "e" names a backend that does not exist. Dropping keeps
// every field that renders true and says nothing where there is no room —
// which is also what metaLine's own contract above promises, that the
// operationally useful fields are not silently mangled to keep a longer one.
//
// The last resort MARKS its cut rather than hiding it: when even the first
// field overruns the budget there is no field boundary left to drop on, and a
// silent clamp there is the same misinformation this function exists to stop.
// "model claude-opus-5" cut to "model claude-opus-" names a model that does
// not exist; "model claude-opu…" reads as truncated. That branch is reachable,
// not theoretical — the pane floors metaBudget at 18 (minListWidth 24 -
// border 2 - metaIndent 4) and an ordinary model name is wider, so a
// 60-column terminal lands there.
//
// Governing: SPEC-0008 REQ "Schedule Visibility" (the cadence this makes room
// for), SPEC-0001 REQ "Zero And Error States" (the sub-line it shares).
//
// @joestump 08/20/2026 - Added reviewing #244, which put the cadence ahead of
// backend/exit and pushed "exit 0" off the edge as "exit" below ~102 columns.
//
// @joestump-agent 08/20/2026 - The last-resort clamp still cut mid-token, so
// the fix leaked at the pane's own floor: `model claude-opus-5` rendered as
// `model claude-opus-` on a 60-column terminal. Marked the cut with an
// ellipsis and pinned the floor in a test.
func fitFields(rest []string, budget int) string {
	joined := strings.Join(rest, " · ")
	if budget <= 0 || lipgloss.Width(joined) <= budget {
		return joined
	}
	for n := len(rest) - 1; n > 0; n-- {
		if s := strings.Join(rest[:n], " · "); lipgloss.Width(s) <= budget {
			return s
		}
	}
	return ansi.Truncate(joined, budget, "…")
}

// clampWidth truncates s to at most budget columns; budget <= 0 is unbounded.
func clampWidth(s string, budget int) string {
	if budget <= 0 || lipgloss.Width(s) <= budget {
		return s
	}
	return ansi.Truncate(s, budget, "")
}

// elideLeft shortens s to w columns by dropping from the FRONT and marking the
// cut with an ellipsis, so the informative tail (a basename, the end of a
// prompt) is what survives.
//
// A cmd is usually a path, so the cut prefers a `/` boundary: "…/.local/bin/
// claude" reads as a path fragment, where the raw column cut "…nt/.local/bin/
// claude" leaves a stump of the directory above it that looks like corruption.
func elideLeft(s string, w int) string {
	full := lipgloss.Width(s)
	if full <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	// Left to right, so the first separator whose suffix fits is the LONGEST
	// fitting one — as much path as the budget allows.
	for i, r := range s {
		if r == '/' && lipgloss.Width(s[i:]) <= w-1 {
			return "…" + s[i:]
		}
	}
	// TruncateLeft removes n columns from the front; the prefix it adds back
	// costs one, so cut one extra to land on w.
	return ansi.TruncateLeft(s, full-w+1, "…")
}

// viewPeek renders the live read-only tail (SPEC-0001 REQ "Dashboard").
//
// The pane is a head line and the guest's screen, nothing else. It used to
// close with a key/value block of the harness's config summary — cmd, backend,
// exit, restarts, pid — which read as a debug dump stapled to a live terminal
// and cost the preview six rows. That metadata now renders under its own row
// in the list (#199, renderMetaRow), where per-harness facts belong.
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

	// Layout within the content height h: head(1) + blank(1) + tail(N). Every
	// row the summary block used to hold is the tail's now.
	maxLines := h - 2 // head + blank-before-tail
	if maxLines < 1 {
		maxLines = 1
	}

	// The screen comes from one of two places.
	//
	// Live (#200): the preview holds its own read-only attach session sized to
	// this pane, so the guest's PTY is this size and its ATTACH_DATA stream has
	// been rendering into peekView all along. Nothing to reconstruct and
	// nothing to crop — the emulator's grid IS the pane.
	//
	// Polled: until that session settles (peekSettleDelay), and against a
	// daemon that cannot serve one, fall back to replaying the `logs` tail
	// through peekCache. That replay MUST be sized to the guest's authoritative
	// viewport (carried on the logs reply from the same Mux the attach plane
	// resizes), not to the pane: a tail drawn for a 156-column guest replayed
	// into a 90-column emulator wraps every line and lands cursor-addressed
	// content in the wrong cells (#192). Sized correctly it reconstructs
	// faithfully and the pane simply CROPS it — honestly cut beats plausibly
	// wrong. A daemon with no viewport to report (no Mux, or predating
	// ProtoMinor 6) leaves us the pane's own geometry, the historical
	// behaviour.
	var (
		screenStr string
		cropNote  bool
	)
	if m.peekLive() {
		screenStr = m.peekView.renderNoCursor()
	} else {
		knownViewport := peekCols > 0 && peekRows > 0
		if !knownViewport {
			peekCols, peekRows = paneInner(w), maxLines+1
		}
		screenStr = m.peekCache.render(tail, peekCols, peekRows)
		cropNote = knownViewport && (peekCols > paneInner(w) || peekRows > maxLines)
	}
	tailLines := trimBlankTail(splitLines(screenStr))
	if len(tailLines) > maxLines {
		tailLines = tailLines[len(tailLines)-maxLines:]
	}

	// When the guest doesn't fit the pane, name its viewport in the head so the
	// missing rows and columns read as a crop rather than a broken render — the
	// same "colsxrows" describe reports (#183), and the number to size the
	// window against. A live session is sized to the pane by construction, so
	// it never crops and never carries the note.
	if cropNote {
		head += m.theme.Faint().Render(fmt.Sprintf(" · %d×%d cropped", peekCols, peekRows))
	}

	// Content assembly stays height-clamped: on a short body budget (#179)
	// lipgloss pads up but never truncates down, so a head plus a tail that
	// together overrun h would push the frame past m.h and scroll the alt
	// screen. maxLines already bounds the tail; this is the backstop.
	lines := []string{head, ""}
	lines = append(lines, tailLines...)
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

// overlayModal composites a dialog centered on top of the surface it
// interrupts (#239): the dashboard (or attached view) stays visible behind a
// centered confirm box, so the operator can see which harness they are about
// to stop while they answer. Built on Lip Gloss v2 Canvas + Layer — the base
// is a full-window layer, the dialog a second layer offset to the center and
// ordered above it by z-index. Without a known window (m.w or m.h still 0
// before the first WindowSizeMsg) there is nothing to center on, so the
// dialog renders alone rather than at a bogus origin.
func (m *Model) overlayModal(base, box string) string {
	if m.w <= 0 || m.h <= 0 {
		return box
	}
	canvas := lipgloss.NewCanvas(m.w, m.h)
	under := lipgloss.NewLayer(base)
	over := lipgloss.NewLayer(box).
		X(maxInt(0, (m.w-lipgloss.Width(box))/2)).
		Y(maxInt(0, (m.h-lipgloss.Height(box))/2)).
		Z(1)
	return canvas.Compose(lipgloss.NewCompositor(under, over)).Render()
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
