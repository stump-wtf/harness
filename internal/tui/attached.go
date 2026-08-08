package tui

// Governing: SPEC-0001 REQ "Attached Mode" (embedded x/vt terminal, thin status
// ribbon, rebindable detach chord + Esc-Esc, read-only badge ignores input),
// REQ "Scrollback Substate" (freeze + navigate + `/` search), and REQ "Harness
// Hop" (`[`/`]` prev/next directly from attached mode — instant and physical:
// harmonica spring slide + ribbon flash). ADR-0003 (embedded terminal),
// ADR-0007 (scrollback), ADR-0008 (read-only attach).

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/harmonica"
	"github.com/charmbracelet/x/ansi"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// attachSubstate is the mode within Attached: driving the live PTY, or frozen in
// scrollback.
type attachSubstate int

const (
	substateInteractive attachSubstate = iota
	substateScrollback
)

// hopFlashTicks is how many animation ticks the ribbon-flash + slide lasts after
// a hop before settling (the "physical" feel; degrades to instant if the model
// isn't ticking).
const hopFlashTicks = 6

// attachState is the live-terminal session state. The vtView is the client-side
// x/vt emulator fed ATTACH_DATA; the spring animates the hop slide.
type attachState struct {
	name      string
	mode      protocol.AttachMode
	sessionID uint32
	view      *vtView

	substate attachSubstate
	scroll   *scrollback
	search   textinput.Model
	searchOn bool

	// Hop animation (harmonica spring): slideX eases back to 0 after an impulse
	// so the swap feels like a physical slide rather than a linear cut; flash is
	// the ribbon-flash countdown.
	spring   harmonica.Spring
	slideX   float64
	slideVel float64
	flash    int

	// prefixArmed is set when the user has pressed the harness prefix
	// (Ctrl-b). The next keystroke is intercepted as a harness command
	// (detach / hop / start / etc.) rather than forwarded to the PTY. Any
	// key that doesn't match a known chord disarms the prefix and — for
	// non-printable keys — is dropped; for a regular printable key we'd
	// ideally forward it, but the common case (user mistyped the chord) is
	// better served by a clean cancel than a phantom letter reaching the
	// agent. This is exactly how tmux handles its prefix.
	prefixArmed bool
}

// newAttachState builds attach state for a harness at the given viewport size.
func newAttachState(name string, mode protocol.AttachMode, sessionID uint32, cols, rows int) *attachState {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.CharLimit = 128
	return &attachState{
		name:      name,
		mode:      mode,
		sessionID: sessionID,
		view:      newVTView(cols, rows),
		substate:  substateInteractive,
		search:    ti,
		// ~60fps spring, moderately stiff, slightly underdamped for a lively feel.
		spring: harmonica.NewSpring(harmonica.FPS(60), 8.0, 0.4),
	}
}

// readOnly reports whether input should be ignored (ADR-0008 read-only attach).
func (a *attachState) readOnly() bool { return a.mode == protocol.AttachRO }

// impulseHop kicks the slide spring so the next few ticks animate a slide, and
// starts the ribbon flash (SPEC-0001 REQ "Harness Hop": slide + ribbon flash).
// direction is -1 for prev, +1 for next.
func (a *attachState) impulseHop(direction int) {
	a.slideX = float64(direction) * 8 // start offset in cells; springs back to 0
	a.slideVel = 0
	a.flash = hopFlashTicks
}

// animate advances the hop spring one tick, easing slideX back to rest and
// decrementing the flash. Returns true while animation is still in progress (the
// model keeps ticking until it settles, then stops for a still screen).
func (a *attachState) animate() bool {
	if a.flash > 0 {
		a.flash--
	}
	a.slideX, a.slideVel = a.spring.Update(a.slideX, a.slideVel, 0)
	settled := absf(a.slideX) < 0.5 && absf(a.slideVel) < 0.5 && a.flash == 0
	if settled {
		a.slideX, a.slideVel = 0, 0
	}
	return !settled
}

// enterScrollback freezes the current screen into a scrollback view over the
// supplied daemon-owned lines (ADR-0007). Falls back to the live screen's lines
// when no separate scrollback is available.
//
// The lines are raw PTY output, so they are made inert at entry (inertText):
// the live view can render escapes because it parses them through an x/vt
// emulator, but this frozen view prints them straight at the user's terminal,
// where they act rather than display. Doing it once at entry also means search
// matches visible text rather than escape noise.
//
// The vtView's rendered output — a faithful reconstruction of the current
// screen with colors and layout intact (#50) — is appended after the inert
// historical lines so scrolling to the bottom shows the actual screen rather
// than garbled cursor-addressed repaint traffic. The frame is derived here
// from a.view (not passed in) so no entry point can forget it; it is rendered
// without the painted guest cursor (a frozen view has no live cursor to
// show), run through inertLines so the only-graphemes-and-SGR guarantee is
// enforced structurally rather than inherited from render's implementation,
// and stripped of trailing blank rows so a mostly-empty screen doesn't bury
// the history under a page of padding. The historical lines above it still
// include the raw bytes that painted the screen, so screen content appears
// twice — once flattened, once faithful; that duplication is the price of
// keeping the full history searchable until daemon-side snapshot scrollback
// (ADR-0007) replaces this client-side interim.
func (a *attachState) enterScrollback(lines []string, height int) {
	a.substate = substateScrollback
	inert := inertLines(lines)
	if a.view != nil {
		frame := trimBlankTail(splitLines(a.view.renderNoCursor()))
		inert = append(inert, inertLines(frame)...)
	}
	a.scroll = newScrollback(inert, height)
	a.searchOn = false
	// A wheel-up can enter scrollback while the Ctrl-b prefix is armed (mouse
	// events don't pass through onAttachedKey); disarm it so the first key
	// typed after exiting scrollback isn't swallowed as a chord.
	a.prefixArmed = false
}

// trimBlankTail drops trailing lines with no visible content (spaces and
// zero-width escapes only), so an appended frame contributes exactly the rows
// the guest has drawn.
func trimBlankTail(lines []string) []string {
	n := len(lines)
	for n > 0 && strings.TrimSpace(ansi.Strip(lines[n-1])) == "" {
		n--
	}
	return lines[:n]
}

// exitScrollback returns to the live view (q/Esc).
func (a *attachState) exitScrollback() {
	a.substate = substateInteractive
	a.scroll = nil
	a.searchOn = false
	a.search.Blur()
	a.search.SetValue("")
}

// absf is a float abs without importing math for one call.
func absf(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
