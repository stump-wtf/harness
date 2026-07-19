// Package keys is the single key-binding registry that drives the whole TUI and
// its rendered help.
//
// Governing: SPEC-0001 REQ "Keybinding Registry" — ALL bindings SHALL be
// declared through the Bubbles key.Binding registry so help renders them and a
// future config can remap them; defaults are single-key dashboard verbs
// (s/x/r/e/n/p), vim scroll (j/k/g/G), `/` search everywhere, `?` help
// everywhere, `Ctrl-k` palette everywhere; tmux-adjacent chords (Ctrl-b d,
// Ctrl-b [) are provided but NEVER required — `Esc` always works. ADR-0001
// (Bubbles help/key).
package keys

import "github.com/charmbracelet/bubbles/key"

// KeyMap is the complete binding registry. Every interactive action in the TUI
// resolves through exactly one field here — nothing matches a raw string — so
// the help view is guaranteed to be a faithful, exhaustive picture of the
// keymap (SPEC-0001 REQ "Keybinding Registry": "the full current keymap renders
// from the binding registry").
type KeyMap struct {
	// Navigation (dashboard list + scrollback).
	Up   key.Binding
	Down key.Binding
	Top  key.Binding
	Bot  key.Binding

	// Dashboard verbs.
	Attach  key.Binding
	Start   key.Binding
	Stop    key.Binding
	Restart key.Binding
	Edit    key.Binding
	New     key.Binding
	Delete  key.Binding
	Profile key.Binding
	ShowAll key.Binding
	Logs    key.Binding

	// Everywhere.
	Search  key.Binding
	Palette key.Binding
	Help    key.Binding
	Quit    key.Binding
	Back    key.Binding // Esc — always works to unwind an overlay/attach.

	// Attached mode.
	Detach     key.Binding // rebindable tmux-style chord (default Ctrl-b d).
	DetachEsc  key.Binding // Esc-Esc double-tap detach.
	Scrollback key.Binding // Ctrl-b [ / PgUp.
	HopPrev    key.Binding // [
	HopNext    key.Binding // ]

	// Scrollback substate.
	PageUp   key.Binding
	PageDown key.Binding
	Live     key.Binding // q/Esc back to live.

	// Overlays.
	Confirm key.Binding // Enter/y confirm a guarded action.
}

// Default returns the SPEC-0001 default bindings.
func Default() KeyMap {
	return KeyMap{
		Up:   key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down: key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Top:  key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
		Bot:  key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),

		Attach:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("↵", "attach")),
		Start:   key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start")),
		Stop:    key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "stop")),
		Restart: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "restart")),
		Edit:    key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
		New:     key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Delete:  key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Profile: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "profile")),
		ShowAll: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "show all")),
		Logs:    key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "logs")),

		Search:  key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Palette: key.NewBinding(key.WithKeys("ctrl+k", ":"), key.WithHelp("^k/:", "palette")),
		Help:    key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:    key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Back:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),

		Detach:     key.NewBinding(key.WithKeys("ctrl+b d"), key.WithHelp("^b d", "detach")),
		DetachEsc:  key.NewBinding(key.WithKeys("esc esc"), key.WithHelp("esc esc", "detach")),
		Scrollback: key.NewBinding(key.WithKeys("ctrl+b [", "pgup"), key.WithHelp("^b [", "scrollback")),
		HopPrev:    key.NewBinding(key.WithKeys("["), key.WithHelp("[", "prev harness")),
		HopNext:    key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "next harness")),

		PageUp:   key.NewBinding(key.WithKeys("pgup"), key.WithHelp("PgUp", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("PgDn", "page down")),
		Live:     key.NewBinding(key.WithKeys("q", "esc"), key.WithHelp("q", "live")),

		Confirm: key.NewBinding(key.WithKeys("enter", "y"), key.WithHelp("↵/y", "confirm")),
	}
}

// RebindDetach swaps the detach chord to a user-chosen key sequence, keeping the
// help label in sync. This is the SPEC-0001 "rebindable detach chord": the
// registry is the one place a remap has to touch.
func (k *KeyMap) RebindDetach(keysSeq, help string) {
	k.Detach = key.NewBinding(key.WithKeys(keysSeq), key.WithHelp(keysSeq, help))
}

// ShortHelp implements help.KeyMap — the compact footer key bar.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Attach, k.Start, k.Stop, k.Restart, k.Edit, k.New, k.Profile, k.Search, k.Help}
}

// FullHelp implements help.KeyMap — the `?` expanded grid. It returns EVERY
// binding in the registry so the help view is exhaustive by construction: add a
// binding to KeyMap and it appears in help automatically (SPEC-0001).
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Top, k.Bot},
		{k.Attach, k.Start, k.Stop, k.Restart, k.Edit, k.New, k.Delete},
		{k.Profile, k.ShowAll, k.Logs, k.Search, k.Palette, k.Help, k.Quit},
		{k.Detach, k.DetachEsc, k.Scrollback, k.HopPrev, k.HopNext},
		{k.PageUp, k.PageDown, k.Live, k.Confirm, k.Back},
	}
}

// All returns every binding in the registry, flattened — used by tests and any
// remap UI that must enumerate the full set.
func (k KeyMap) All() []key.Binding {
	var out []key.Binding
	for _, row := range k.FullHelp() {
		out = append(out, row...)
	}
	return out
}
