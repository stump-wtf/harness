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
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
	"github.com/charmbracelet/x/ansi"
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
	tail.HarnessCodex:      {Harness: tail.HarnessCodex, Username: "@codex"},
	tail.HarnessCrush:      {Harness: tail.HarnessCrush, Username: "@crush-signal"},
	tail.HarnessOpenCode:   {Harness: tail.HarnessOpenCode, Username: "@opencode"},
	tail.HarnessPi:         {Harness: tail.HarnessPi, Username: "@pi"},
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

// oneLine flattens s onto a single line, collapsing every run of whitespace
// (newlines included) to one space.
//
// Chatroom layout is line-based: each event contributes a known number of rows,
// and the view clips to that count. Summaries carry the raw command, so a
// heredoc or any multi-line invocation smuggles newlines into what the renderer
// counted as one row — the frame overflows and the stream interleaves.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateShort flattens s and clips it to n runes, appending an ellipsis when
// it had to cut. Runes, not bytes: transcript summaries routinely carry
// non-ASCII (paths, quoted output, box drawing), and a byte slice lands
// mid-rune and renders as a replacement character.
func truncateShort(s string, n int) string {
	s = oneLine(s)
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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

	// lines caches what RenderLines produced. An event is immutable once
	// buffered and the styles are fixed for the session, so its rows only ever
	// need rendering once. Without the cache both View and the scroll anchor
	// re-render the entire buffer on every arriving event, which measured at
	// ~92ms of CPU per event at the 10k-event cap — the update loop, and with
	// it the whole TUI, falls permanently behind a live stream.
	lines []string
}

// Lines returns the event's rendered rows, rendering them on first use.
func (re *RenderableEvent) Lines(s *Styles) []string {
	if re.lines == nil {
		re.lines = re.RenderLines(s)
	}
	return re.lines
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

func AllHarnesses() FilterSet              { return 0xFF }
func (f FilterSet) Has(i int) bool         { return f&(1<<i) != 0 }
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
		harnessIdentities[tail.HarnessCodex],
		harnessIdentities[tail.HarnessCrush],
		harnessIdentities[tail.HarnessOpenCode],
		harnessIdentities[tail.HarnessPi],
	}
}

// EventBuffer is the chatroom's ring of events, sorted by timestamp, alongside
// the flattened lines the visible subset of them renders to.
//
// The line slice is the point. Without it, View walked every buffered event to
// build every line and then threw away all but the ~40 on screen, and
// scrollToBottom walked them all again to find the anchor — twice per arriving
// event, growing with the buffer. Keeping the flattened form incrementally
// makes both O(1) in the buffer's depth, at the cost of the bookkeeping below.
type EventBuffer struct {
	events  []RenderableEvent
	maxSize int
	filter  FilterSet
	paused  bool

	// lines is the rendered form of the events the current filter admits.
	// Valid only while dirty is false; every mutation either extends it or
	// gives up and sets dirty, and Lines rebuilds on demand.
	lines []string
	dirty bool
}

func NewEventBuffer(maxSize int) *EventBuffer {
	return &EventBuffer{
		events:  make([]RenderableEvent, 0, maxSize),
		maxSize: maxSize,
		filter:  AllHarnesses(),
		dirty:   true,
	}
}

// Insert files re into the buffer in timestamp order and keeps the line index
// in step.
//
// The fast path is the only one that matters: events arrive in order, land at
// the end, and their lines append to the index. Anything else — an event that
// sorts into the middle, an eviction of something the filter was showing —
// invalidates the index and Lines rebuilds it once, lazily. A chatroom nobody
// has open never rebuilds at all, so buffering while closed costs no rendering.
func (b *EventBuffer) Insert(re RenderableEvent, s *Styles) {
	ts := re.Event.Classified.Timestamp
	if ts == "" {
		ts = re.Event.ReceivedAt.Format(time.RFC3339)
	}
	i := len(b.events)
	for i > 0 && b.events[i-1].Event.Classified.Timestamp > ts {
		i--
	}
	appended := i >= len(b.events)
	if appended {
		b.events = append(b.events, re)
	} else {
		b.events = append(b.events, RenderableEvent{})
		copy(b.events[i+1:], b.events[i:])
		b.events[i] = re
		b.dirty = true
	}
	if !b.dirty && appended && b.admits(re) {
		b.lines = append(b.lines, b.events[len(b.events)-1].Lines(s)...)
	}

	if len(b.events) > b.maxSize {
		evicted := b.events[:len(b.events)-b.maxSize]
		if !b.dirty {
			// Trim the index by exactly what left the front. A reslice, not a
			// rebuild: at the cap every insert evicts, and rebuilding there
			// would undo the whole point of keeping the index.
			drop := 0
			for j := range evicted {
				if b.admits(evicted[j]) {
					drop += len(evicted[j].Lines(s))
				}
			}
			if drop > len(b.lines) {
				b.dirty = true
			} else {
				b.lines = b.lines[drop:]
			}
		}
		b.events = b.events[len(b.events)-b.maxSize:]
	}
}

// Lines returns the rendered lines of every event the filter admits, rebuilding
// the index if a mutation invalidated it.
func (b *EventBuffer) Lines(s *Styles) []string {
	if !b.dirty {
		return b.lines
	}
	b.lines = b.lines[:0]
	for i := range b.events {
		if b.admits(b.events[i]) {
			b.lines = append(b.lines, b.events[i].Lines(s)...)
		}
	}
	b.dirty = false
	return b.lines
}

// admits reports whether the current filter shows re.
func (b *EventBuffer) admits(re RenderableEvent) bool {
	if b.filter == AllHarnesses() {
		return true
	}
	for i, id := range AllIdentities() {
		if id.Harness == re.Identity.Harness {
			return b.filter.Has(i)
		}
	}
	return false
}

// Visible returns the events the current filter admits, in buffer order.
//
// Rendering does not go through here — View reads the flattened line index,
// which is what keeps a frame independent of the buffer's depth. This is the
// inspection accessor: it answers questions about events (ordering, eviction,
// what the filter kept) that a flat list of strings cannot.
func (b *EventBuffer) Visible() []RenderableEvent {
	if b.filter == AllHarnesses() {
		return b.events
	}
	out := make([]RenderableEvent, 0, len(b.events))
	for i := range b.events {
		if b.admits(b.events[i]) {
			out = append(out, b.events[i])
		}
	}
	return out
}

func (b *EventBuffer) Len() int          { return len(b.events) }
func (b *EventBuffer) Filter() FilterSet { return b.filter }
func (b *EventBuffer) Paused() bool      { return b.paused }
func (b *EventBuffer) TogglePause()      { b.paused = !b.paused }

// SetFilter changes which harnesses the buffer admits, invalidating the line
// index — a different filter is a different set of lines.
func (b *EventBuffer) SetFilter(f FilterSet) {
	if f == b.filter {
		return
	}
	b.filter = f
	b.dirty = true
}

// Model is the chatroom view state — a single full-screen scrolling stream.
//
// It does not own a watcher. The TUI runs exactly one tail.Watcher for the
// whole program (internal/tui/watcher.go) and feeds it here; the chatroom is a
// view over a buffer, alive for the session rather than for the time the view
// happens to be on screen. Owning one meant a second full scan of every
// transcript on the machine every time the view was opened, concurrent with the
// dashboard's own, and an empty buffer to show while it ran.
type Model struct {
	theme  *theme.Theme
	styles *Styles
	buffer *EventBuffer
	logger *slog.Logger

	width  int
	height int
	top    int // scroll position (line index)

	errMsg string
}

func New(t *theme.Theme, logger *slog.Logger) *Model {
	return &Model{
		theme:  t,
		styles: NewStyles(t),
		buffer: NewEventBuffer(10000),
		logger: logger,
	}
}

// Add files one event into the buffer, re-anchoring the scroll unless the
// stream is paused. Cheap enough to call for every event whether or not the
// view is on screen: rendering is deferred to Lines, which nobody calls while
// the chatroom is closed.
func (m *Model) Add(ev tail.Event) {
	m.buffer.Insert(MakeRenderable(ev), m.styles)
}

// Settle re-anchors the scroll after a batch of Adds. Separate from Add so a
// batch of 500 events costs one anchor recomputation rather than 500.
func (m *Model) Settle() {
	if !m.buffer.Paused() {
		m.scrollToBottom()
	}
}

// SetError records a watcher failure for the status bar.
func (m *Model) SetError(err error) {
	if err == nil {
		return
	}
	if m.logger != nil {
		m.logger.Error("chatroom watcher error", "error", err)
	}
	m.errMsg = err.Error()
}

func (m *Model) Update(msg tea.Msg) (*Model, tea.Cmd) {
	if msg, ok := msg.(tea.WindowSizeMsg); ok {
		m.SetSize(msg.Width, msg.Height)
	}
	return m, nil
}

// View renders the full-screen chat stream + status bar.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}

	bodyHeight := m.height - 1 // 1 row for status bar
	window := clipLines(m.buffer.Lines(m.styles), m.top, bodyHeight)

	// Copy before touching a row. clipLines returns a window ONTO the buffer's
	// cached line index, so truncating in place would overwrite the cache with
	// the width it happened to be rendered at — and a widened terminal would
	// keep redrawing the narrow version.
	body := make([]string, bodyHeight)
	for i := range body {
		if i < len(window) {
			// Clip to the pane: lipgloss wraps rather than truncates, and a
			// wrapped row costs a display line the height budget never counted,
			// pushing the status bar off the bottom of the screen.
			body[i] = ansi.Truncate(window[i], m.width, "…")
		}
	}

	// Padded by hand rather than through lipgloss. ChatViewport is an empty
	// style, so Width(m.width).Height(bodyHeight).Render was padding every row
	// out to the full width to no visual effect — no background, no border —
	// at ~350us a frame, which is the frame rate of the whole TUI while the
	// chatroom is open. Only the row COUNT has to be made up, so the status bar
	// lands on the last line.
	statusLine := m.styles.StatusBar.Width(m.width).Render(m.renderStatusBar())
	return strings.Join(body, "\n") + "\n" + statusLine
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

func (m *Model) renderedLineCount() int { return len(m.buffer.Lines(m.styles)) }

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

// SetFilter changes the visible-harness filter and re-anchors the scroll.
//
// Filtering shrinks the rendered stream, so an offset taken against the
// unfiltered line count lands past the end of it and the pane renders blank —
// which reads as "the filter matched nothing" even when it matched plenty.
func (m *Model) SetFilter(f FilterSet) {
	m.buffer.SetFilter(f)
	if m.buffer.Paused() {
		m.Scroll(0) // clamp the existing offset into the new range
		return
	}
	m.scrollToBottom()
}

// SetSize seeds the chatroom's geometry.
//
// View renders nothing until it has one, and the chatroom is created on
// keypress — long after the tea.WindowSizeMsg that sized the parent model. Left
// to wait for the next resize, the view is a blank screen that repaints on
// every event, so entering the chatroom must hand it the geometry the dashboard
// already knows.
func (m *Model) SetSize(w, h int) {
	m.width, m.height = w, h
	if !m.buffer.Paused() {
		m.scrollToBottom()
	}
}

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
