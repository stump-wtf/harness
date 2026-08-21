// Package chatroom implements the unified chatroom TUI view (ADR-0015, SPEC-0015).
//
// A full-screen chronological stream of all agent activity across every harness
// (Claude Code, Codex, Crush, OpenCode, Pi). Each event renders as a chat-style
// line: timestamp, harness username (colored), action badge, tool name, summary.
// Tool results, user marks, and file targets render as follow-up lines.
//
// The view consumes events from agent-trace's tail.Watcher, which discovers and
// parses native session transcripts from all five harness formats.
package chatroom

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	tea "charm.land/bubbletea/v2"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

// HarnessIdentity maps a tail.Harness to its chatroom display identity.
type HarnessIdentity struct {
	Harness  tail.Harness
	Username string // e.g. "@claude-code"
}

var harnessIdentities = map[tail.Harness]HarnessIdentity{
	tail.HarnessClaudeCode: {Harness: tail.HarnessClaudeCode, Username: "@claude-code"},
	"codex":                {Harness: "codex", Username: "@codex"},
	tail.HarnessCrush:      {Harness: tail.HarnessCrush, Username: "@crush-signal"},
	tail.HarnessOpenCode:   {Harness: tail.HarnessOpenCode, Username: "@opencode"},
	"pi":                   {Harness: "pi", Username: "@pi"},
}

func IdentityFor(h tail.Harness) HarnessIdentity {
	if id, ok := harnessIdentities[h]; ok {
		return id
	}
	return HarnessIdentity{Harness: h, Username: "@" + string(h)}
}

func ActionBadge(action string) string {
	switch action {
	case classify.ActionSearch:
		return "[SEARCH]"
	case classify.ActionRead:
		return "[READ]"
	case classify.ActionEdit:
		return "[EDIT]"
	case classify.ActionExec:
		return "[EXEC]"
	case classify.ActionVerify:
		return "[VERIFY]"
	default:
		return "[OTHER]"
	}
}

func MarkBadge(markType string) string {
	switch markType {
	case "user":
		return "[USER]"
	case "compaction":
		return "[COMPACTION]"
	case "subagent":
		return "[SUBAGENT]"
	default:
		return "[" + strings.ToUpper(markType) + "]"
	}
}

func truncateShort(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// FormatTime parses an ISO timestamp string and renders HH:MM:SS.
func FormatTime(ts string) string {
	if ts == "" {
		return "--:--:--"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Format("15:04:05")
}

// RenderableEvent wraps a tail.Event with pre-computed render data.
type RenderableEvent struct {
	Event    tail.Event
	Identity HarnessIdentity
	Time     string
	Badge    string
	Tool     string
	Summary  string
	IsError  bool
	HasMarks bool
	Marks    []classify.Mark
}

func MakeRenderable(ev tail.Event) RenderableEvent {
	id := IdentityFor(ev.Session.Harness)
	re := RenderableEvent{
		Event:    ev,
		Identity: id,
		Time:     FormatTime(ev.Classified.Timestamp),
		IsError:  ev.Classified.IsError,
		Summary:  truncateShort(ev.Classified.Summary, 80),
		Tool:     ev.Classified.Tool,
	}
	if len(ev.Marks) > 0 {
		re.HasMarks = true
		re.Marks = ev.Marks
	}
	if ev.Classified.Tool != "" {
		re.Badge = ActionBadge(ev.Classified.Action)
	}
	return re
}

// RenderLines produces the chat-style lines for a single event.
func (re RenderableEvent) RenderLines(s *Styles) []string {
	var lines []string

	if re.HasMarks {
		for _, mark := range re.Marks {
			badge := s.BadgeUser.Render(MarkBadge(mark.Type))
			username := s.Username[string(re.Identity.Harness)].Render(re.Identity.Username)
			ts := s.Timestamp.Render(FormatTime(mark.Timestamp))
			note := truncateShort(mark.Note, 200)
			lines = append(lines, fmt.Sprintf("%s %s %s %s %s", ts, username, badge, s.Dim.Render("—"), note))
		}
	}

	if re.Tool != "" {
		username := s.Username[string(re.Identity.Harness)].Render(re.Identity.Username)
		ts := s.Timestamp.Render(re.Time)
		badge := s.BadgeStyle(re.Event.Classified.Action).Render(re.Badge)
		tool := s.Tool.Render(re.Tool)
		lines = append(lines, fmt.Sprintf("%s %s %s %s %s", ts, username, badge, tool, re.Summary))

		for _, t := range re.Event.Classified.Targets {
			rank := "•"
			switch t.Touch {
			case "edit":
				rank = "✎"
			case "read":
				rank = "👁"
			}
			lines = append(lines, fmt.Sprintf("    %s %s", rank, s.Target.Render(t.Path)))
		}

		if re.IsError {
			lines = append(lines, fmt.Sprintf("    %s %s", s.BadgeError.Render("[ERROR]"), truncateShort(re.Event.Classified.Summary, 120)))
		}
	}

	return lines
}

// LastAction returns a compact one-line summary of the most recent event for
// a harness, for use in the dashboard's live activity field.
func LastAction(ev tail.Event) string {
	if ev.Classified.Tool != "" {
		return fmt.Sprintf("%s %s", ActionBadge(ev.Classified.Action), ev.Classified.Tool)
	}
	if len(ev.Marks) > 0 {
		return MarkBadge(ev.Marks[0].Type)
	}
	return ""
}

// FilterSet is a bitmask of visible harnesses.
type FilterSet uint32

func AllHarnesses() FilterSet      { return 0xFF }
func (f FilterSet) Has(i int) bool { return f&(1<<i) != 0 }
func (f FilterSet) Toggle(i int) FilterSet { return f ^ (1 << i) }
func (f FilterSet) Count() int {
	c := 0
	for i := 0; i < 5; i++ {
		if f.Has(i) {
			c++
		}
	}
	return c
}
func (f FilterSet) String() string {
	if f == AllHarnesses() {
		return "all"
	}
	ids := AllIdentities()
	names := make([]string, 0, 5)
	for i, id := range ids {
		if f.Has(i) {
			names = append(names, id.Username)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, " ")
}

func AllIdentities() []HarnessIdentity {
	return []HarnessIdentity{
		harnessIdentities[tail.HarnessClaudeCode],
		harnessIdentities["codex"],
		harnessIdentities[tail.HarnessCrush],
		harnessIdentities[tail.HarnessOpenCode],
		harnessIdentities["pi"],
	}
}

// EventBuffer is the chatroom's ring of events, sorted by timestamp.
type EventBuffer struct {
	events  []RenderableEvent
	maxSize int
	filter  FilterSet
	paused  bool
}

func NewEventBuffer(maxSize int) *EventBuffer {
	return &EventBuffer{
		events:  make([]RenderableEvent, 0, maxSize),
		maxSize: maxSize,
		filter:  AllHarnesses(),
	}
}

func (b *EventBuffer) Insert(re RenderableEvent) {
	ts := re.Event.Classified.Timestamp
	if ts == "" {
		ts = re.Event.ReceivedAt.Format(time.RFC3339)
	}
	i := len(b.events)
	for i > 0 && b.events[i-1].Event.Classified.Timestamp > ts {
		i--
	}
	if i >= len(b.events) {
		b.events = append(b.events, re)
	} else {
		b.events = append(b.events, RenderableEvent{})
		copy(b.events[i+1:], b.events[i:])
		b.events[i] = re
	}
	if len(b.events) > b.maxSize {
		b.events = b.events[len(b.events)-b.maxSize:]
	}
}

func (b *EventBuffer) Visible() []RenderableEvent {
	if b.filter == AllHarnesses() {
		return b.events
	}
	out := make([]RenderableEvent, 0, len(b.events))
	ids := AllIdentities()
	for _, ev := range b.events {
		for i, id := range ids {
			if id.Harness == ev.Identity.Harness && b.filter.Has(i) {
				out = append(out, ev)
				break
			}
		}
	}
	return out
}

func (b *EventBuffer) Len() int          { return len(b.events) }
func (b *EventBuffer) Filter() FilterSet { return b.filter }
func (b *EventBuffer) SetFilter(f FilterSet) { b.filter = f }
func (b *EventBuffer) Paused() bool      { return b.paused }
func (b *EventBuffer) TogglePause()       { b.paused = !b.paused }

// LastForHarness returns the most recent event for a given harness, or nil.
func (b *EventBuffer) LastForHarness(h tail.Harness) *RenderableEvent {
	for i := len(b.events) - 1; i >= 0; i-- {
		if b.events[i].Identity.Harness == h {
			return &b.events[i]
		}
	}
	return nil
}

// Model is the chatroom view state — a single full-screen scrolling stream.
type Model struct {
	theme   *theme.Theme
	styles  *Styles
	watcher *tail.Watcher
	buffer  *EventBuffer
	logger  *slog.Logger

	width  int
	height int
	top    int // scroll position (line index)

	running bool
	errMsg  string
}

func New(t *theme.Theme, logger *slog.Logger) *Model {
	return &Model{
		theme:  t,
		styles: NewStyles(t),
		buffer: NewEventBuffer(10000),
		logger: logger,
	}
}

// Init starts the watcher and returns a tea.Cmd that reads events.
func (m *Model) Init() tea.Cmd {
	adapters := tail.DefaultAdapters()
	cfg := tail.DefaultWatchConfig()
	m.watcher = tail.NewWatcherWithConfig(cfg, adapters)
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel
	m.watcher.Start(ctx)
	m.running = true
	return waitForEvents(m.watcher, m.logger)
}

// MsgEvent is a bubbletea message carrying a tail.Event from the watcher.
type MsgEvent struct{ Event tail.Event }

// MsgWatcherError is a bubbletea message carrying a watcher error.
type MsgWatcherError struct{ Err error }

// WaitForEvents returns a tea.Cmd that blocks on the watcher's event channel
// and forwards each event as a MsgEvent. Exported so the main TUI model can
// re-arm the command after forwarding the event through Update.
func WaitForEvents(m *Model) tea.Cmd {
	if m == nil || m.watcher == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-m.watcher.Events()
		if !ok {
			return nil
		}
		return MsgEvent{Event: ev}
	}
}

func waitForEvents(w *tail.Watcher, logger *slog.Logger) tea.Cmd {
	if w == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-w.Events()
		if !ok {
			return nil
		}
		return MsgEvent{Event: ev}
	}
}

func (m *Model) Stop() {
	if m.watcher != nil {
		m.watcher.Stop()
		m.watcher = nil
	}
	m.running = false
}

func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case MsgEvent:
		re := MakeRenderable(msg.Event)
		m.buffer.Insert(re)
		if !m.buffer.Paused() {
			m.scrollToBottom()
		}
		return m, waitForEvents(m.watcher, m.logger)

	case MsgWatcherError:
		if m.logger != nil {
			m.logger.Error("chatroom watcher error", "error", msg.Err)
		}
		m.errMsg = msg.Err.Error()
		return m, nil
	}
	return m, nil
}

// View renders the full-screen chat stream + status bar.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	events := m.buffer.Visible()
	var lines []string
	for _, re := range events {
		lines = append(lines, re.RenderLines(m.styles)...)
	}

	bodyHeight := m.height - 1 // 1 row for status bar
	visible := clipLines(lines, m.top, bodyHeight)
	chatContent := strings.Join(visible, "\n")

	chatPane := m.styles.ChatViewport.Width(m.width).Height(bodyHeight).Render(chatContent)
	statusLine := m.styles.StatusBar.Width(m.width).Render(m.renderStatusBar())

	return lipgloss.JoinVertical(lipgloss.Top, chatPane, statusLine)
}

func (m *Model) renderStatusBar() string {
	var parts []string
	if m.buffer.Paused() {
		parts = append(parts, m.styles.PauseInd.Render("⏸ PAUSED"))
	}
	parts = append(parts, m.styles.FilterInd.Render("filter: "+m.buffer.Filter().String()))
	parts = append(parts, m.styles.Dim.Render(fmt.Sprintf("%d events", m.buffer.Len())))
	if m.errMsg != "" {
		parts = append(parts, m.styles.BadgeError.Render("⚠ "+truncateShort(m.errMsg, 40)))
	}
	return strings.Join(parts, "  ")
}

func (m *Model) scrollToBottom() {
	lines := m.renderedLineCount()
	bodyHeight := m.height - 1
	if lines > bodyHeight {
		m.top = lines - bodyHeight
	} else {
		m.top = 0
	}
}

func (m *Model) renderedLineCount() int {
	events := m.buffer.Visible()
	c := 0
	for _, re := range events {
		c += len(re.RenderLines(m.styles))
	}
	return c
}

func (m *Model) Scroll(delta int) {
	lines := m.renderedLineCount()
	bodyHeight := m.height - 1
	m.top += delta
	if m.top < 0 {
		m.top = 0
	}
	maxTop := lines - bodyHeight
	if maxTop < 0 {
		maxTop = 0
	}
	if m.top > maxTop {
		m.top = maxTop
	}
}

func (m *Model) Buffer() *EventBuffer { return m.buffer }

func clipLines(lines []string, top, height int) []string {
	if top >= len(lines) {
		return nil
	}
	end := top + height
	if end > len(lines) {
		end = len(lines)
	}
	return lines[top:end]
}
