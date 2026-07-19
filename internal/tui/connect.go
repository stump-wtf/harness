package tui

// Governing: ADR-0002 (thin client; two independent connections keep the control
// plane off the event/attach read loop — see readloop.go's CRITICAL note),
// ADR-0004 (Unix socket), SPEC-0002 REQ "Handshake And Versioning". This file
// holds the production dialer and the serialization wrapper that keeps the
// non-concurrent Client safe under Bubble Tea's concurrent Cmds.

import (
	"sync"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// attachConn is the events+attach side of the wire: the read loop owns its
// Conn() for reading, while the model writes attach frames through it. Satisfied
// by *client.Client.
type attachConn interface {
	AttachOpen(sessionID uint32, name string, cols, rows int, mode protocol.AttachMode) error
	AttachInput(sessionID uint32, data []byte) error
	AttachResize(sessionID uint32, cols, rows int) error
	AttachClose(sessionID uint32) error
	Conn() *protocol.Conn
	Close() error
}

// dialer opens the two connections the TUI needs: a control connection (typed
// ops) and an events+attach connection (the single read loop). Injectable so
// tests can supply fakes.
type dialer func(socket, version string) (Controller, attachConn, error)

// realDial dials both connections against a live daemon.
func realDial(socket, version string) (Controller, attachConn, error) {
	ctrl, err := client.Dial(socket, version, nil)
	if err != nil {
		return nil, nil, err
	}
	evt, err := client.Dial(socket, version, []string{"events"})
	if err != nil {
		_ = ctrl.Close()
		return nil, nil, err
	}
	return newMutexController(ctrl), evt, nil
}

// mutexController serializes control calls so concurrent Bubble Tea Cmds never
// touch the non-concurrent Client at the same time (client.go: "not safe for
// concurrent control Calls"). It embeds nothing so every method is explicit.
type mutexController struct {
	mu sync.Mutex
	c  Controller
}

func newMutexController(c Controller) *mutexController { return &mutexController{c: c} }

func (m *mutexController) List() ([]protocol.HarnessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.List()
}
func (m *mutexController) Describe(name string) (protocol.HarnessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Describe(name)
}
func (m *mutexController) Start(name string) (protocol.HarnessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Start(name)
}
func (m *mutexController) Stop(name string) (protocol.HarnessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Stop(name)
}
func (m *mutexController) Restart(name string) (protocol.HarnessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Restart(name)
}
func (m *mutexController) Logs(name string, lines int) (protocol.LogsData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Logs(name, lines)
}
func (m *mutexController) Profiles() ([]protocol.ProfileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Profiles()
}
func (m *mutexController) UseProfile(name string) ([]protocol.ProfileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.UseProfile(name)
}
func (m *mutexController) Reload() ([]protocol.HarnessInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Reload()
}
func (m *mutexController) DaemonInfo() (protocol.DaemonInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.DaemonInfo()
}
func (m *mutexController) DaemonVersion() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.DaemonVersion()
}
func (m *mutexController) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Close()
}
