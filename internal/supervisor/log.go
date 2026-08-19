package supervisor

// Governing: ADR-0007 (State & scrollback ownership) — "the daemon tees raw
// PTY output to a rotating log file per harness under
// $XDG_STATE_HOME/harness/logs/<name>.log (size/age rotation)"; SPEC-0003 REQ
// "Lifecycle Events" observability. This backs `harness logs <name>` for live
// and dead harnesses alike, independent of the in-memory ring (ADR-0003).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// rotatedStampLayout is the timestamp suffix stamped onto rotated log files.
// Kept in one place so rotation and pruning agree on the exact shape and a
// pruning harness only ever matches its own backups.
const rotatedStampLayout = "20060102T150405.000"

// LogConfig tunes per-harness log rotation. Zero values fall back to defaults
// in newRotatingLog.
type LogConfig struct {
	// Dir is the directory logs live in (…/harness/logs).
	Dir string
	// MaxBytes rotates the active file once it would exceed this size.
	MaxBytes int64
	// MaxAge rotates the active file once it is older than this.
	MaxAge time.Duration
	// MaxBackups caps how many rotated files are retained per harness.
	MaxBackups int
}

const (
	defaultMaxBytes   = 8 << 20 // 8 MiB
	defaultMaxAge     = 24 * time.Hour
	defaultMaxBackups = 5
)

// rotatingLog is an io.WriteCloser that appends raw PTY bytes to
// <dir>/<name>.log, rotating by size or age. Rotated files are renamed with a
// timestamp suffix and pruned to MaxBackups. It is safe for concurrent writes
// (the PTY reader is the only writer in practice, but rotation and Close may
// race with it).
type rotatingLog struct {
	name string
	cfg  LogConfig

	mu       sync.Mutex
	f        *os.File
	size     int64
	openedAt time.Time
}

// newRotatingLog opens (creating parents) the active log file for name.
func newRotatingLog(name string, cfg LogConfig) (*rotatingLog, error) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = defaultMaxAge
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = defaultMaxBackups
	}
	rl := &rotatingLog{name: name, cfg: cfg}
	// Create the log file's own parent, not just cfg.Dir: a project harness's
	// name is namespaced ("reduit/agent"), so its log lives one level down at
	// <dir>/reduit/agent.log (ADR-0009; SPEC-0004 REQ "Project Naming And
	// Namespacing").
	if err := os.MkdirAll(filepath.Dir(rl.path()), 0o755); err != nil {
		return nil, fmt.Errorf("supervisor: create log dir: %w", err)
	}
	if err := rl.open(); err != nil {
		return nil, err
	}
	return rl, nil
}

// path is the active log file path for this harness.
func (rl *rotatingLog) path() string {
	return filepath.Join(rl.cfg.Dir, rl.name+".log")
}

// open opens the active file for appending and records its current size/age.
func (rl *rotatingLog) open() error {
	f, err := os.OpenFile(rl.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("supervisor: open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("supervisor: stat log: %w", err)
	}
	rl.f = f
	rl.size = info.Size()
	rl.openedAt = time.Now()
	return nil
}

// Write appends p, rotating first if the active file has hit its size or age
// bound. It implements io.Writer.
func (rl *rotatingLog) Write(p []byte) (int, error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.f == nil {
		return 0, os.ErrClosed
	}
	if rl.size+int64(len(p)) > rl.cfg.MaxBytes || time.Since(rl.openedAt) > rl.cfg.MaxAge {
		if err := rl.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := rl.f.Write(p)
	rl.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("supervisor: write log: %w", err)
	}
	return n, nil
}

// rotate closes the active file, renames it with a timestamp suffix, prunes old
// backups, and opens a fresh active file. Caller holds rl.mu.
func (rl *rotatingLog) rotate() error {
	if rl.f != nil {
		_ = rl.f.Close()
		rl.f = nil
	}
	// Only rename if there is content to preserve.
	if rl.size > 0 {
		stamp := time.Now().Format(rotatedStampLayout)
		rotated := filepath.Join(rl.cfg.Dir, fmt.Sprintf("%s-%s.log", rl.name, stamp))
		if err := os.Rename(rl.path(), rotated); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("supervisor: rotate log: %w", err)
		}
		rl.pruneBackups()
	}
	return rl.open()
}

// pruneBackups deletes the oldest rotated files beyond MaxBackups. Best-effort:
// prune failures never fail a write.
func (rl *rotatingLog) pruneBackups() {
	pattern := filepath.Join(rl.cfg.Dir, rl.name+"-*.log")
	globbed, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	// Filter to THIS harness's own backups. A bare glob on `<name>-*.log` also
	// matches sibling harnesses whose name shares our prefix (e.g. pruning
	// "web" would otherwise sweep up "web-api.log" and "web-api-<stamp>.log"),
	// deleting another harness's logs. isOwnBackup keeps only files whose suffix
	// parses as our rotation timestamp.
	matches := globbed[:0]
	for _, m := range globbed {
		if rl.isOwnBackup(m) {
			matches = append(matches, m)
		}
	}
	if len(matches) <= rl.cfg.MaxBackups {
		return
	}
	// Glob returns lexical order; our timestamp suffix sorts chronologically,
	// so the oldest are first.
	for _, old := range matches[:len(matches)-rl.cfg.MaxBackups] {
		_ = os.Remove(old)
	}
}

// isOwnBackup reports whether path is one of THIS harness's rotated backups —
// exactly `<name>-<timestamp>.log` — and not a sibling harness that merely
// shares our name as a prefix.
func (rl *rotatingLog) isOwnBackup(path string) bool {
	return isBackupOf(rl.name, path)
}

// isBackupOf reports whether path is a rotated backup of the named harness.
// The comparison uses the BASE of the (possibly namespaced) harness name: a
// project harness "reduit/agent" rotates to <dir>/reduit/agent-<stamp>.log, so
// after filepath.Base only "agent-<stamp>" remains to match (ADR-0009;
// SPEC-0004 REQ "Project Naming And Namespacing"). Cross-harness confusion is
// impossible because callers glob within the name's own directory and the
// suffix must parse as our exact rotation timestamp.
func isBackupOf(name, path string) bool {
	base := filepath.Base(path)
	mid := strings.TrimSuffix(base, ".log")
	if mid == base {
		return false // no .log suffix
	}
	stamp := strings.TrimPrefix(mid, filepath.Base(name)+"-")
	if stamp == mid {
		return false // not `<name>-…`
	}
	_, err := time.Parse(rotatedStampLayout, stamp)
	return err == nil
}

// removeLogArtifacts deletes a harness's on-disk log tree: the active
// <dir>/<name>.log, every rotated backup, and — for a namespaced project
// harness — the project's log subdirectory if it is now empty. The Manager
// calls this when a project harness is deregistered so torn-down projects do
// not leak unreachable log files forever (SPEC-0004 REQ "Tear Down": the
// daemon retains no record of the project afterward). Best-effort: removal
// failures are ignored, exactly like pruneBackups.
func removeLogArtifacts(dir, name string) {
	active := filepath.Join(dir, name+".log")
	_ = os.Remove(active)
	if globbed, err := filepath.Glob(filepath.Join(dir, name+"-*.log")); err == nil {
		for _, path := range globbed {
			if isBackupOf(name, path) {
				_ = os.Remove(path)
			}
		}
	}
	if filepath.Dir(name) != "." {
		// Namespaced name: drop the per-project directory when empty (os.Remove
		// refuses a non-empty directory, which is exactly what we want).
		_ = os.Remove(filepath.Dir(active))
	}
}

// Close closes the active file. Implements io.Closer.
func (rl *rotatingLog) Close() error {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.f == nil {
		return nil
	}
	err := rl.f.Close()
	rl.f = nil
	if err != nil {
		return fmt.Errorf("supervisor: close log: %w", err)
	}
	return nil
}
