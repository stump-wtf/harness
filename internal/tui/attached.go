package tui

// Governing: SPEC-0001 REQ "Attached Mode" (embedded x/vt terminal, thin status
// ribbon, rebindable detach chord + Esc-Esc, read-only badge ignores input),
// REQ "Scrollback Substate" (freeze + navigate + `/` search), and REQ "Harness
// Hop" (`[`/`]` prev/next directly from attached mode — instant and physical:
// harmonica spring slide + ribbon flash). ADR-0003 (embedded terminal),
// ADR-0007 (scrollback), ADR-0008 (read-only attach).

import (
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/harmonica"

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

	// pendingEsc arms the Esc-Esc detach: the first Esc sets it, a second Esc
	// detaches (SPEC-0001 REQ "Attached Mode": Esc-Esc returns to Dashboard).
	pendingEsc bool
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
func (a *attachState) enterScrollback(lines []string, height int) {
	a.substate = substateScrollback
	a.scroll = newScrollback(lines, height)
	a.searchOn = false
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
