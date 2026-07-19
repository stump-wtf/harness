package tui

// Governing: ADR-0002 (the TUI is a thin client; it owns nothing durable and
// speaks the control plane) and SPEC-0002 REQ "Control Operations". Controller
// is the typed control surface the model depends on — satisfied by
// *client.Client in production and by a fake in tests, so the model's decision
// logic is exercised without a live daemon.

import (
	"errors"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// errChannelClosed is the disconnect cause when the read-loop channel closes.
var errChannelClosed = errors.New("tui: event channel closed")

// Controller is the daemon control plane as the TUI uses it. Each method is a
// one-shot typed request→response; a domain error comes back as a
// *protocol.ErrorMsg. *client.Client implements this exactly.
type Controller interface {
	List() ([]protocol.HarnessInfo, error)
	Describe(name string) (protocol.HarnessInfo, error)
	Start(name string) (protocol.HarnessInfo, error)
	Stop(name string) (protocol.HarnessInfo, error)
	Restart(name string) (protocol.HarnessInfo, error)
	Logs(name string, lines int) (protocol.LogsData, error)
	Profiles() ([]protocol.ProfileInfo, error)
	UseProfile(name string) ([]protocol.ProfileInfo, error)
	Reload() ([]protocol.HarnessInfo, error)
	DaemonInfo() (protocol.DaemonInfo, error)
	DaemonVersion() string
	Close() error
}
