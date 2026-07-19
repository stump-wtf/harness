package main

// Governing: SPEC-0001 (the cockpit TUI is the no-args entry point of the
// harness client) and ADR-0001 (Bubble Tea). runTUI wires the model to a full
// alt-screen Bubble Tea program.

import (
	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui"
)

// runTUI launches the interactive cockpit against the daemon socket.
func runTUI(socket, configPath string) error {
	m := tui.New(tui.Options{
		Socket:     socket,
		ConfigPath: configPath,
		Version:    buildinfo.Version,
	})
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
