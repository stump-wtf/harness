package protocol

// Governing: ADR-0004 (local transport is the Unix domain socket at
// $XDG_RUNTIME_DIR/harness.sock); ADR-0008 (socket mode 0600, or a 0700 dir
// fallback under $XDG_STATE_HOME/harness/ on systems without a per-user
// runtime dir — filesystem permissions are the local access control).

import (
	"os"
	"path/filepath"
)

// SocketMode is the required permission on the control/data socket (ADR-0008).
const SocketMode = 0o600

// fallbackDirMode is the permission on the fallback directory that holds the
// socket when there is no $XDG_RUNTIME_DIR (ADR-0008: 0700).
const fallbackDirMode = 0o700

// DefaultSocketPath returns the conventional socket location. It prefers
// $XDG_RUNTIME_DIR/harness.sock (ADR-0004); when that env var is unset it
// falls back to $XDG_STATE_HOME/harness/harness.sock (ADR-0008), matching the
// state-home the supervisor already uses.
func DefaultSocketPath() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "harness.sock")
	}
	return filepath.Join(stateHome(), "harness.sock")
}

// stateHome mirrors supervisor.StateHome without importing it (avoids a
// dependency cycle for a one-liner): $XDG_STATE_HOME/harness, falling back to
// ~/.local/state/harness.
func stateHome() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".", "harness")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "harness")
}

// EnsureSocketDir creates the parent directory for path if needed. When path is
// in a fallback location (no $XDG_RUNTIME_DIR) the directory is created 0700 so
// only the owning user can reach the socket inside it (ADR-0008).
func EnsureSocketDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, fallbackDirMode)
}
