package supervisor

// Governing: ADR-0006 (config is the source of truth; the daemon hot-reloads
// harness.toml, and a parse error keeps the last-good config and surfaces the
// error rather than crashing); SPEC-0003 REQ "Config Change Application".

import (
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
)

// ReloadFromFile re-parses the config at path and applies it. On a parse or
// validation error the current (last-good) config is retained untouched and the
// error is returned for the caller to surface (ADR-0006). On success the new
// config is applied via Reload (running harnesses keep running; changes stage
// for next restart).
func (m *Manager) ReloadFromFile(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err // keep last-good config; surface the location-carrying error
	}
	m.Reload(cfg)
	return nil
}
