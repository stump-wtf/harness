package daemon

// Package environment status (issue #930): a package-sourced harness's
// [[env]] declarations, each with whether the child the daemon would spawn
// sees it set. The daemon answers because only it knows its own
// environment — a client checking its shell's environment would pass a
// variable the daemon (started by launchd or systemd) never had.
//
// Governing: ADR-0044, ADR-0038 rule 9 (names, never values, on the wire);
// SPEC-0026 REQ-12.
//
// @joestump-agent 10/09/2026 - Added for harness#930.

import (
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// packageEnvStatus projects h's declared environment onto the wire with each
// variable's presence. Nil when the package declares nothing. An unreadable
// env_file leaves its names unset here; the spawn reports the read error.
func packageEnvStatus(h core.Harness) []protocol.PackageEnvStatus {
	if len(h.PackageEnv) == 0 {
		return nil
	}
	names := make([]string, len(h.PackageEnv))
	for i, ev := range h.PackageEnv {
		names[i] = ev.Name
	}
	from, _ := supervisor.EnvPresence(h, names)
	out := make([]protocol.PackageEnvStatus, len(h.PackageEnv))
	for i, ev := range h.PackageEnv {
		out[i] = protocol.PackageEnvStatus{
			Name:        ev.Name,
			Required:    ev.Required,
			Secret:      ev.Secret,
			Description: ev.Description,
			Set:         from[ev.Name] != "",
			From:        from[ev.Name],
		}
	}
	return out
}
