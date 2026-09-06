// Package chatroom renders a unified, read-only "chatroom" view of live agent
// activity across every supported harness (Claude Code, Codex, Crush,
// OpenCode, Pi), each appearing as a distinct chat user.
//
// Governing: ADR-0015 (chatroom TUI view), ADR-0007 as amended 2026-09-06
// (agent-trace is the readable output source for every session type),
// SPEC-0015. Events come from agent-trace's tail.Watcher over the session
// stores each tool already keeps (crush.db, ~/.claude/projects JSONL, …) —
// never from the daemon's raw PTY tees, which carry full-screen TUI repaints
// rather than append-only lines.
package chatroom

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/trajectory"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
	"github.com/stump-wtf/agent-trace/tail"
)

// maxEvents caps the chat buffer (SPEC-0015 design: EventBuffer, 10k).
const maxEvents = 10000

// chatWidthPct is the main chat panel's share of the width; the activity
// feed takes the rest.
const chatWidthPct = 0.7

// filterable lists the harnesses in filter-slot order (keys 1-5).
var filterable = []tail.Harness{
	tail.HarnessClaudeCode,
	tail.HarnessCodex,
	tail.HarnessCrush,
	tail.HarnessOpenCode,
	tail.HarnessPi,
}

// harnessUser maps a tail.Harness to its chatroom username. The map covers
// every adapter agent-trace ships; an unknown harness string still renders
// with its own name rather than being dropped.
func harnessUser(h tail.Harness) string {
	if h == "" {
		return "@unknown"
	}
	return "@" + string(h)
}

// entry is one renderable chat line: a tool call (with its result folded in)
// or a mark (user message, compaction, subagent launch).
type entry struct {
	at      time.Time
	harness tail.Harness
	// markType is non-empty for mark entries ("user", "compaction",
	// "subagent"); mark entries carry note instead of tool/summary.
	markType string
	note     string

	tool    string
	action  string
	summary string
	targets []string

	isError     bool
	resultBytes int

	// dedupe identifies the entry across watcher re-scans: session key plus
	// sequence. Two adapters can discover the same session (a crush registry
	// under ~/.local/share/crush-signal/ points at databases the default
	// registry also lists), and the watcher re-emits unchanged sessions on
	// every poll — dedupe keeps the stream readable either way.
	dedupe string
}

// parseTS parses an event timestamp (RFC 3339), reporting ok=false when
// missing or unparseable so the caller falls back to ReceivedAt.
func parseTS(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// eventTime resolves an event's timestamp, falling back to ReceivedAt
// (SPEC-0015 REQ "Chronological Event Ordering").
func eventTime(ts string, recv time.Time) time.Time {
	if t, ok := parseTS(ts); ok {
		return t
	}
	return recv
}

// Model is the chatroom view. It owns its watcher lifecycle: Init starts it,
// exit (q/esc) stops it. The embedder routes messages to Update while the
// view is active and calls View to render.
type Model struct {
	theme  *theme.Theme
	styles styles

	w, h int

	watcher *tail.Watcher
	cancel  context.CancelFunc
	stopped bool

	// mu guards entries: the watcher goroutine only touches its own channel,
	// but Update can be re-entered from the embedder's loop, and tests poke
	// the buffer directly. Cheap for the sizes involved.
	mu      sync.Mutex
	entries []entry
	seen    map[string]struct{}

	filter string // "" shows all
	paused bool
	focus  focus
	chatY  int // scroll offset into the rendered chat lines
	actSel int // activity feed selection
	status string
}

type focus int

const (
	focusChat focus = iota
	focusActivity
)

// EventsMsg delivers a batch of watcher events to Update.
type EventsMsg struct{ Events []tail.Event }

// stoppedMsg is delivered once the watcher channel has closed.
// StoppedMsg is delivered once the watcher channel has closed.
type StoppedMsg struct{}

// New builds a chatroom Model. Adapters are DefaultAdapters plus one extra
// CrushAdapter per alternate crush registry found beside the default data
// dir (~/.local/share/crush-*/projects.json) — a harness that repoints
// CRUSH_GLOBAL_DATA (crush-signal) registers its sessions only there, and
// without the extra adapter its turns are invisible in the chatroom.
func New(th *theme.Theme) *Model {
	m := &Model{
		theme: th,
		seen:  make(map[string]struct{}),
	}
	m.styles = newStyles(th)
	return m
}

func (m *Model) adapters() []tail.Adapter {
	return trajectory.LiveAdapters()
}

// Init starts the watcher and the event-pump command.
func (m *Model) Init() tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	cfg := tail.DefaultWatchConfig()
	m.watcher = tail.NewWatcherWithConfig(cfg, m.adapters())
	m.watcher.Start(ctx)
	return m.pump()
}

// Stop tears the watcher down. Safe to call more than once.
func (m *Model) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.watcher != nil {
		m.watcher.Stop()
	}
	m.stopped = true
}

// pump returns a tea.Cmd that blocks on the next watcher event.
func (m *Model) pump() tea.Cmd {
	ch := m.watcher.Events()
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return StoppedMsg{}
		}
		return EventsMsg{Events: []tail.Event{ev}}
	}
}

// Update advances the chatroom. The second return value reports that the user
// requested exit; the embedder then switches back to its main view.
func (m *Model) Update(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case EventsMsg:
		m.add(msg.Events)
		// Drain anything already queued so one Cmd per batch, not one per
		// event, round-trips the tea loop under bursts.
		var batch []tail.Event
		for {
			select {
			case ev, ok := <-m.watcher.Events():
				if !ok {
					if len(batch) > 0 {
						m.add(batch)
					}
					return nil, false
				}
				batch = append(batch, ev)
				if len(batch) >= 256 {
					m.add(batch)
					batch = nil
				}
			default:
				if len(batch) > 0 {
					m.add(batch)
				}
				return m.pump(), false
			}
		}
	case StoppedMsg:
		return nil, false
	case tea.WindowSizeMsg:
		m.Resize(msg.Width, msg.Height)
		return nil, false
	case tea.KeyPressMsg:
		return m.onKey(msg)
	}
	return nil, false
}

// add merges events into the buffer in timestamp order, deduping re-emissions
// and capping the buffer at maxEvents.
func (m *Model) add(events []tail.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range events {
		h := ev.Session.Harness
		for _, mk := range ev.Marks {
			d := ev.Session.Key + ":m:" + itoa(mk.Seq)
			if _, dup := m.seen[d]; dup {
				continue
			}
			m.seen[d] = struct{}{}
			m.insert(entry{
				at:       eventTime(mk.Timestamp, ev.ReceivedAt),
				harness:  h,
				markType: mk.Type,
				note:     mk.Note,
				dedupe:   d,
			})
		}
		c := ev.Classified
		if c.Tool == "" && c.Summary == "" {
			continue
		}
		d := ev.Session.Key + ":e:" + itoa(c.Seq)
		if _, dup := m.seen[d]; dup {
			continue
		}
		m.seen[d] = struct{}{}
		targets := make([]string, 0, len(c.Targets))
		for _, t := range c.Targets {
			targets = append(targets, t.Path)
		}
		m.insert(entry{
			at:          eventTime(c.Timestamp, ev.ReceivedAt),
			harness:     h,
			tool:        c.Tool,
			action:      c.Action,
			summary:     c.Summary,
			targets:     targets,
			isError:     c.IsError,
			resultBytes: c.ResultBytes,
			dedupe:      d,
		})
	}
	if len(m.entries) > maxEvents {
		drop := m.entries[:len(m.entries)-maxEvents]
		for _, e := range drop {
			delete(m.seen, e.dedupe)
		}
		m.entries = append([]entry(nil), m.entries[len(m.entries)-maxEvents:]...)
	}
}

func (m *Model) insert(e entry) {
	i := sort.Search(len(m.entries), func(i int) bool {
		prev := m.entries[i]
		if prev.at.Equal(e.at) {
			return string(prev.harness) >= string(e.harness)
		}
		return prev.at.After(e.at)
	})
	m.entries = append(m.entries, entry{})
	copy(m.entries[i+1:], m.entries[i:])
	m.entries[i] = e
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// visible returns the entries passing the current filter.
func (m *Model) visible() []entry {
	if m.filter == "" {
		return m.entries
	}
	out := m.entries[:0:0]
	for _, e := range m.entries {
		if string(e.harness) == m.filter {
			out = append(out, e)
		}
	}
	return out
}

// Resize records the terminal geometry, preserving the chat's distance from
// the bottom (SPEC-0015 REQ "Terminal Resize Handling").
func (m *Model) Resize(w, h int) {
	fromBottom := m.chatHeight() - m.chatY
	m.w, m.h = w, h
	m.clampScroll(fromBottom)
}

func (m *Model) chatHeight() int {
	h := m.h - 2 // header + status bar
	if h < 1 {
		return 1
	}
	return h
}

func (m *Model) clampScroll(fromBottom int) {
	lines := len(m.chatLines())
	if lines <= m.chatHeight() {
		m.chatY = 0
		return
	}
	max := lines - m.chatHeight()
	// fromBottom <= 0 (or paused auto-scroll) pins to the newest lines.
	m.chatY = lines - fromBottom
	if m.chatY > max {
		m.chatY = max
	}
	if m.chatY < 0 {
		m.chatY = 0
	}
}

// onKey handles the chatroom keymap (SPEC-0015 REQ "Keyboard Navigation").
func (m *Model) onKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	s := msg.String()
	switch s {
	case "q", "esc":
		m.Stop()
		return nil, true
	case "j", "down":
		if m.focus == focusActivity {
			if m.actSel < len(m.visible())-1 {
				m.actSel++
				m.scrollTo(m.actSel)
			}
		} else {
			m.clampScroll(m.chatHeight() - m.chatY - 1)
		}
	case "k", "up":
		if m.focus == focusActivity {
			if m.actSel > 0 {
				m.actSel--
				m.scrollTo(m.actSel)
			}
		} else {
			m.clampScroll(m.chatHeight() - m.chatY + 1)
		}
	case "ctrl+d", "pgdown":
		m.clampScroll(m.chatHeight() - m.chatY - m.chatHeight()/2) // half page
	case "ctrl+u", "pgup":
		m.clampScroll(m.chatHeight() - m.chatY + m.chatHeight()/2)
	case "g", "home":
		m.chatY = 0
	case "G", "end":
		m.clampScroll(0)
	case " ", "p":
		m.paused = !m.paused
	case "tab":
		if m.focus == focusChat {
			m.focus = focusActivity
		} else {
			m.focus = focusChat
		}
	case "0", "a":
		m.filter = ""
		m.status = "filter: all"
	case "1", "2", "3", "4", "5":
		idx := int(s[0] - '1')
		if idx < len(filterable) {
			m.filter = string(filterable[idx])
			m.status = "filter: " + m.filter
		}
	}
	return nil, false
}

// scrollTo jumps the chat viewport so the activity-feed selection is visible.
func (m *Model) scrollTo(visIdx int) {
	// Scrolling from the feed implies manual browsing: pause auto-follow.
	m.paused = true
	m.chatY = visIdx // one rendered block per entry, approximately
	if max := len(m.chatLines()) - m.chatHeight(); m.chatY > max {
		m.chatY = max
	}
	if m.chatY < 0 {
		m.chatY = 0
	}
}

// chatLines renders the visible entries as styled chat blocks, one string per
// logical block (wrapped by lipgloss at the panel width).
func (m *Model) chatLines() []string {
	w := m.chatPanelWidth()
	out := make([]string, 0, len(m.entries))
	for _, e := range m.visible() {
		out = append(out, m.renderEntry(e, w))
	}
	return out
}

func (m *Model) chatPanelWidth() int {
	w := int(float64(m.w)*chatWidthPct) - 4
	if w < 20 {
		w = 20
	}
	return w
}

func (m *Model) activityPanelWidth() int {
	w := m.w - int(float64(m.w)*chatWidthPct) - 3
	if w < 12 {
		w = 12
	}
	return w
}

// renderEntry formats one chat message: username, badge, tool, summary,
// targets, and (for tool calls) status.
func (m *Model) renderEntry(e entry, w int) string {
	ts := e.at.Format("15:04:05")
	user := m.styles.user(e.harness).Render(harnessUser(e.harness))
	var badge, body string
	if e.markType != "" {
		badge = m.styles.markBadge(e.markType).Render(markBadgeLabel(e.markType))
		body = truncate(e.note, 200)
	} else {
		badge = m.styles.actionBadge(e.action).Render(actionBadgeLabel(e.action))
		tool := e.tool
		if tool == "" {
			tool = "?"
		}
		body = tool + " " + truncate(e.summary, 80)
		if e.isError {
			body += " " + m.styles.errBadge.Render("[ERROR]")
		} else if e.resultBytes > 0 {
			body += " " + m.styles.okBadge.Render("[OK]")
		}
	}
	line := m.styles.faint.Render(ts) + " " + user + " " + badge + " " + m.styles.fg.Render(body)
	for _, t := range e.targets {
		line += "\n" + strings.Repeat(" ", 10) + m.styles.faint.Render("· "+t)
	}
	return m.styles.fg.MaxWidth(w).Render(line)
}

// activityLine renders one condensed feed line: HH:MM:SS @harness tool summary.
func (m *Model) activityLine(e entry, w int) string {
	ts := e.at.Format("15:04:05")
	name := harnessUser(e.harness)
	what := e.tool
	if e.markType != "" {
		what = e.markType
	}
	txt := ts + " " + name + " " + what + " " + e.summary
	if len(txt) > w {
		txt = txt[:w-1] + "…"
	}
	st := m.styles.user(e.harness)
	if e.isError {
		st = m.styles.errBadge
	}
	return st.MaxWidth(w).Render(txt)
}

// View renders the chatroom: header, chat panel (left, 70%), activity feed
// (right, 30%), status bar.
func (m *Model) View() string {
	if m.w == 0 {
		return "chatroom — waiting for terminal size…"
	}
	vis := m.visible()

	chatLines := m.chatLines()
	if !m.paused {
		m.clampScroll(0) // follow the newest lines
	}
	start, end := m.chatY, m.chatY+m.chatHeight()
	if start > len(chatLines) {
		start = len(chatLines)
	}
	if end > len(chatLines) {
		end = len(chatLines)
	}
	var chat strings.Builder
	for _, l := range chatLines[start:end] {
		chat.WriteString(l)
		chat.WriteString("\n")
	}

	feedW := m.activityPanelWidth()
	feedH := m.chatHeight()
	var feed strings.Builder
	for i := len(vis) - feedH; i < len(vis); i++ {
		if i < 0 {
			continue
		}
		line := m.activityLine(vis[i], feedW)
		if m.focus == focusActivity && i == m.actSel {
			line = m.styles.selected(feedW).Render(line)
		}
		feed.WriteString(line)
		feed.WriteString("\n")
	}

	header := m.styles.header(m.w).Render(
		"chatroom — live agent activity" + m.filterSuffix() + m.pauseSuffix())
	left := m.styles.box.Render(chat.String())
	if m.focus == focusChat {
		left = m.styles.focusedBox.Render(chat.String())
	}
	right := m.styles.box.Render(feed.String())
	if m.focus == focusActivity {
		right = m.styles.focusedBox.Render(feed.String())
	}
	status := m.styles.statusBar(m.w).Render(m.statusText(len(vis)))
	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, left, right),
		status,
	)
}

func (m *Model) filterSuffix() string {
	if m.filter == "" {
		return ""
	}
	return " · filter " + m.filter
}

func (m *Model) pauseSuffix() string {
	if m.paused {
		return " · paused"
	}
	return ""
}

func (m *Model) statusText(n int) string {
	s := m.status
	if s == "" {
		s = keyHelp
	}
	return s + "  ·  " + itoa(n) + " events"
}

const keyHelp = "j/k scroll · 1-5 filter · 0 all · space pause · tab panel · q back"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func actionBadgeLabel(action string) string {
	switch action {
	case "search":
		return "[SEARCH]"
	case "read":
		return "[READ]"
	case "edit":
		return "[EDIT]"
	case "exec":
		return "[EXEC]"
	case "verify":
		return "[VERIFY]"
	}
	return "[OTHER]"
}

func markBadgeLabel(t string) string {
	switch t {
	case "user":
		return "[USER]"
	case "compaction":
		return "[COMPACTION]"
	case "subagent":
		return "[SUBAGENT]"
	}
	return "[" + strings.ToUpper(t) + "]"
}
