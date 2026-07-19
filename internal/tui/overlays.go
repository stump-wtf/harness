package tui

// Governing: SPEC-0001 REQ "Command Palette", "Profile Switcher", "Harness
// Form", "Confirmation Guards", "Keybinding Registry" (help overlay). The
// overlay layer sits above both primary modes; each overlay owns its own key
// handling and closes back to whatever mode was underneath.

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// closeOverlay dismisses any overlay and returns to the primary mode.
func (m *Model) closeOverlay() {
	m.overlay = overlayNone
	m.pal.input.Blur()
	m.search.Blur()
	m.form = nil
}

// onOverlayKey routes keys to the active overlay.
func (m *Model) onOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.overlay {
	case overlayPalette:
		return m.onPaletteKey(msg)
	case overlaySearch:
		return m.onSearchKey(msg)
	case overlayProfile:
		return m.onProfileKey(msg)
	case overlayForm:
		return m.updateForm(msg)
	case overlayConfirm:
		return m.onConfirmKey(msg)
	case overlayHelp:
		// Any of back/help/q closes the help overlay.
		if key.Matches(msg, m.keys.Back) || key.Matches(msg, m.keys.Help) || msg.String() == "q" {
			m.overlay = overlayNone
		}
		return m, nil
	}
	return m, nil
}

// --- command palette ------------------------------------------------------

// openPalette builds the command list and focuses the input (SPEC-0001 REQ
// "Command Palette").
func (m *Model) openPalette() (tea.Model, tea.Cmd) {
	m.pal.all = BuildCommands(CLIVerbs(), m.harnesses, m.profiles)
	m.pal.filtered = m.pal.all
	m.pal.sel = 0
	m.pal.input.SetValue("")
	m.pal.input.Focus()
	m.overlay = overlayPalette
	return m, textinputBlink()
}

// onPaletteKey handles palette navigation, filtering, and execution.
func (m *Model) onPaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Back):
		m.closeOverlay()
		return m, nil
	case msg.Type == tea.KeyEnter:
		return m.executePalette()
	case msg.Type == tea.KeyUp:
		m.pal.sel = clamp(m.pal.sel-1, 0, maxIndex(len(m.pal.filtered)))
		return m, nil
	case msg.Type == tea.KeyDown:
		m.pal.sel = clamp(m.pal.sel+1, 0, maxIndex(len(m.pal.filtered)))
		return m, nil
	}
	var cmd tea.Cmd
	m.pal.input, cmd = m.pal.input.Update(msg)
	m.pal.filtered = FilterCommands(m.pal.all, m.pal.input.Value())
	m.pal.sel = clamp(m.pal.sel, 0, maxIndex(len(m.pal.filtered)))
	return m, cmd
}

// executePalette runs the selected command (SPEC-0001 scenario "Verb plus
// target": Enter on "restart reduit-agent" executes the restart).
func (m *Model) executePalette() (tea.Model, tea.Cmd) {
	if m.pal.sel < 0 || m.pal.sel >= len(m.pal.filtered) {
		m.closeOverlay()
		return m, nil
	}
	cmd := m.pal.filtered[m.pal.sel]
	m.closeOverlay()
	return m.runVerb(cmd.Verb, cmd.Target)
}

// runVerb dispatches a palette/CLI verb by name — the single place the palette
// maps to actions, mirroring the CLI's run() (SPEC-0001 REQ "Command Palette":
// mirrors the CLI 1:1). Palette execution is explicit, so it bypasses the
// confirm guard.
func (m *Model) runVerb(verb, target string) (tea.Model, tea.Cmd) {
	switch verb {
	case "attach":
		if h := m.harnessByName(target); h != nil {
			return m, m.attachTo(*h, 0)
		}
	case "start":
		return m, m.performAction(ActionStart, target)
	case "stop":
		return m, m.performAction(ActionStop, target)
	case "restart":
		return m, m.performAction(ActionRestart, target)
	case "delete":
		return m, m.performAction(ActionDelete, target)
	case "edit":
		if i := selectByName(m.visible(), target); i >= 0 {
			m.sel = i
		}
		return m.openForm(true)
	case "new":
		return m.openForm(false)
	case "profile":
		if target != "" && m.ctrl != nil {
			return m, doSwitchProfile(m.ctrl, target, false, m.harnesses)
		}
	case "describe", "logs":
		if i := selectByName(m.visible(), target); i >= 0 {
			m.sel = i
		}
		return m, m.peekCmd()
	case "reload":
		if m.ctrl != nil {
			return m, doReload(m.ctrl)
		}
	case "list", "profiles", "daemon-info":
		if m.ctrl != nil {
			return m, fetchState(m.ctrl)
		}
	}
	return m, nil
}

// --- search ---------------------------------------------------------------

// openSearch focuses the dashboard filter input (SPEC-0001 `/` search).
func (m *Model) openSearch() (tea.Model, tea.Cmd) {
	m.search.SetValue(m.searchQuery)
	m.search.Focus()
	m.overlay = overlaySearch
	return m, textinputBlink()
}

// onSearchKey live-filters the dashboard list; Enter commits, Esc clears.
func (m *Model) onSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEscape:
		m.searchQuery = ""
		m.closeOverlay()
		m.clampSel()
		return m, nil
	case tea.KeyEnter:
		m.closeOverlay()
		m.clampSel()
		return m, m.peekCmd()
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	m.searchQuery = m.search.Value()
	m.sel = 0
	return m, cmd
}

// --- profile switcher -----------------------------------------------------

// openProfileSwitcher opens the profile picker (SPEC-0001 REQ "Profile
// Switcher").
func (m *Model) openProfileSwitcher() (tea.Model, tea.Cmd) {
	m.prof = profState{}
	if p := activeProfile(m.profiles); p != nil {
		m.prof.sel = selectProfileIndex(m.profiles, p.Name)
	}
	m.overlay = overlayProfile
	return m, nil
}

// onProfileKey drives the two-step switcher: pick a profile, then answer the
// non-destructive "start stopped members?" prompt (SPEC-0001 scenario
// "Non-destructive switch").
func (m *Model) onProfileKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.prof.askStart {
		switch {
		case key.Matches(msg, m.keys.Confirm), msg.String() == "y":
			name := m.prof.pending
			m.closeOverlay()
			return m, doSwitchProfile(m.ctrl, name, true, m.harnesses)
		case msg.String() == "n":
			name := m.prof.pending
			m.closeOverlay()
			return m, doSwitchProfile(m.ctrl, name, false, m.harnesses)
		case key.Matches(msg, m.keys.Back):
			m.closeOverlay()
			return m, nil
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Back):
		m.closeOverlay()
	case key.Matches(msg, m.keys.Up):
		m.prof.sel = clamp(m.prof.sel-1, 0, maxIndex(len(m.profiles)))
	case key.Matches(msg, m.keys.Down):
		m.prof.sel = clamp(m.prof.sel+1, 0, maxIndex(len(m.profiles)))
	case msg.Type == tea.KeyEnter:
		if m.prof.sel >= 0 && m.prof.sel < len(m.profiles) {
			m.prof.pending = m.profiles[m.prof.sel].Name
			m.prof.askStart = true
		}
	}
	return m, nil
}

// --- harness form (Huh) ---------------------------------------------------

// openForm opens the n/e Huh form. When editing, it pre-fills from the selected
// harness (SPEC-0001 REQ "Harness Form"; `e` pre-fills).
func (m *Model) openForm(editing bool) (tea.Model, tea.Cmd) {
	m.editing = editing
	m.fInputs = formInputs{backend: string(core.BackendNative)}
	if editing {
		if sel, ok := m.selectedHarness(); ok {
			// Pre-fill the WHOLE schema from the config file (file-is-truth,
			// ADR-0006) so the edit round-trip never drops args/workdir/env_file/
			// restart_delay, which HarnessInfo omits (see editInputsFor).
			m.fInputs = editInputsFor(m.opts.ConfigPath, sel)
		}
	}
	m.form = buildHarnessForm(&m.fInputs)
	m.overlay = overlayForm
	return m, m.form.Init()
}

// updateForm routes a message to the Huh form and, on completion, writes the
// harness to harnessd.toml and reloads the daemon (SPEC-0001 scenario "Create
// without leaving the TUI").
func (m *Model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.form == nil {
		m.overlay = overlayNone
		return m, nil
	}
	// Esc aborts the form without writing.
	if km, ok := msg.(tea.KeyMsg); ok && km.Type == tea.KeyEscape {
		m.closeOverlay()
		return m, nil
	}
	fm, cmd := m.form.Update(msg)
	m.form = fm
	if f, ok := fm.(*huh.Form); ok && f.State == huh.StateCompleted {
		form := m.fInputs.toForm()
		m.closeOverlay()
		if err := form.Validate(); err != nil {
			m.status = "form: " + err.Error()
			return m, nil
		}
		return m, m.saveHarnessCmd(form)
	}
	return m, cmd
}

// saveHarnessCmd serializes the form to TOML and writes+reloads. A new harness
// is appended; an edited one replaces its table.
func (m *Model) saveHarnessCmd(form HarnessForm) tea.Cmd {
	path := m.opts.ConfigPath
	ctrl := m.ctrl
	editing := m.editing
	return func() tea.Msg {
		existing, _ := readFileOrEmpty(path)
		var body []byte
		if editing {
			body = []byte(removeHarnessTOML(string(existing), form.Name))
			body = AppendHarness(body, form)
		} else {
			body = AppendHarness(existing, form)
		}
		if err := writeFile(path, body); err != nil {
			return reloadResultMsg{err: err}
		}
		if ctrl == nil {
			return reloadResultMsg{harnesses: nil}
		}
		hs, err := ctrl.Reload()
		return reloadResultMsg{harnesses: hs, err: err}
	}
}

// --- confirm dialog -------------------------------------------------------

// onConfirmKey resolves a destructive-action confirm dialog (SPEC-0001 REQ
// "Confirmation Guards").
func (m *Model) onConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Matches(msg, m.keys.Confirm) {
		c := m.confirm
		m.closeOverlay()
		return m, m.performAction(c.action, c.target)
	}
	// Anything else (n / esc) cancels.
	m.closeOverlay()
	return m, nil
}

// --- helpers --------------------------------------------------------------

// buildHarnessForm constructs the Huh form bound to fi (SPEC-0001 REQ "Harness
// Form" schema: cmd/args/workdir/env_file/restart_delay/backend/description).
func buildHarnessForm(fi *formInputs) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("name").Value(&fi.name),
			huh.NewInput().Title("cmd").Value(&fi.cmd),
			huh.NewInput().Title("args (space-separated)").Value(&fi.args),
			huh.NewInput().Title("workdir").Value(&fi.workdir),
			huh.NewInput().Title("env_file").Value(&fi.envFile),
			huh.NewInput().Title("restart_delay (seconds)").Value(&fi.delay),
			huh.NewSelect[string]().Title("backend").
				Options(huh.NewOption("native", "native"), huh.NewOption("tmux", "tmux")).
				Value(&fi.backend),
			huh.NewInput().Title("description").Value(&fi.description),
			huh.NewConfirm().Title("enabled (autostart)").Value(&fi.enabled),
		),
	).WithShowHelp(false)
}

// harnessByName returns a pointer to the harness named n, or nil.
func (m *Model) harnessByName(n string) *protocol.HarnessInfo {
	for i := range m.harnesses {
		if m.harnesses[i].Name == n {
			return &m.harnesses[i]
		}
	}
	return nil
}

// selectProfileIndex returns the index of the named profile, or 0.
func selectProfileIndex(profiles []protocol.ProfileInfo, name string) int {
	for i := range profiles {
		if profiles[i].Name == name {
			return i
		}
	}
	return 0
}

// maxIndex returns n-1 clamped at 0 (the largest valid index of an n-length
// slice, or 0 when empty).
func maxIndex(n int) int {
	if n <= 1 {
		return 0
	}
	return n - 1
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
