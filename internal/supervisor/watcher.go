package supervisor

// Governing: issue #98 — watch harness.toml for changes and auto-reload. The
// watcher watches the config DIRECTORY (not the inode) because chezmoi writes
// via temp file + rename, which changes the inode. A debounce coalesces the
// burst of writes a rendering tool makes.
//
// Per-harness env files are NOT watched yet, though the issue mentions them.
// The directory is already watched, so extending to them is a matter of
// widening the basename filter — but an env-file change is only picked up on
// respawn (the supervisor reads it at spawn), so reloading on one would need
// a decision about whether to bounce running harnesses. That belongs in its
// own change rather than riding along here.

import (
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/fsnotify/fsnotify"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// ConfigWatcher watches the config file's directory for changes that imply
// the config (or a referenced env file) was rewritten, debounces the burst,
// and triggers a reload on the Manager.
type ConfigWatcher struct {
	watcher    *fsnotify.Watcher
	manager    *Manager
	configDir  string
	configBase string
	done       chan struct{}
	wg         sync.WaitGroup
}

// NewConfigWatcher creates a watcher for configPath. Call Start to begin
// watching; call Close to stop. The watcher adds the config file's directory
// to the fsnotify watcher (not the file itself) so temp-file+rename writes
// (the chezmoi pattern) are caught.
func NewConfigWatcher(mgr *Manager, configPath string) (*ConfigWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(configPath)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return nil, err
	}
	return &ConfigWatcher{
		watcher:    w,
		manager:    mgr,
		configDir:  dir,
		configBase: filepath.Base(configPath),
		done:       make(chan struct{}),
	}, nil
}

// Start begins watching in a background goroutine.
func (cw *ConfigWatcher) Start() {
	cw.wg.Add(1)
	go cw.loop()
}

// Close stops the watcher and waits for the goroutine to exit.
func (cw *ConfigWatcher) Close() {
	close(cw.done)
	_ = cw.watcher.Close()
	cw.wg.Wait()
}

// debounce is how long the watcher waits after the last change event before
// triggering a reload. A rendering tool (chezmoi, czu) can write several
// times in quick succession.
const debounce = 500 * time.Millisecond

func (cw *ConfigWatcher) loop() {
	defer cw.wg.Done()
	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-cw.done:
			if timer != nil {
				timer.Stop()
			}
			return

		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}
			// Only react to events on the config file itself (create, write,
			// or rename — chezmoi writes via temp + rename, so a CREATE with
			// the target name is the most common signal).
			if filepath.Base(event.Name) != cw.configBase {
				continue
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
				continue
			}
			// Debounce: reset the timer on each event.
			if timer == nil {
				timer = time.NewTimer(debounce)
				timerC = timer.C
			} else {
				timer.Reset(debounce)
			}

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			log.Warn("config watcher error", "err", err)

		case <-timerC:
			timer = nil
			timerC = nil
			cw.reload()
		}
	}
}

// reload re-reads the config and applies it via the Manager. A parse failure
// keeps the last-good config and logs loudly (issue #98: never fall back to
// an empty harness set). The Manager's ReloadFromFile already implements the
// last-good guarantee.
func (cw *ConfigWatcher) reload() {
	configPath := filepath.Join(cw.configDir, cw.configBase)
	before := cw.manager.Config()

	if err := cw.manager.ReloadFromFile(configPath); err != nil {
		log.Warn("config auto-reload failed (keeping last-good config)",
			"config", configPath,
			"err", err)
		return
	}

	after := cw.manager.Config()
	changes := diffConfig(before, after)
	if len(changes) > 0 {
		log.Info("config auto-reloaded", "changes", changes)
	} else {
		log.Debug("config reload triggered but no effective changes")
	}
}

// diffConfig returns a human-readable summary of what changed between two
// configs. Used for logging so the operator can see what the watcher did.
func diffConfig(oldCfg, newCfg *core.Config) []string {
	var changes []string

	// Added harnesses.
	for _, name := range newCfg.HarnessOrder {
		if _, ok := oldCfg.Harnesses[name]; !ok {
			changes = append(changes, "added harness "+name)
		}
	}
	// Removed harnesses.
	for _, name := range oldCfg.HarnessOrder {
		if _, ok := newCfg.Harnesses[name]; !ok {
			changes = append(changes, "removed harness "+name)
		}
	}
	// Changed harnesses. Reuses harnessDefEqual — the comparator the reload
	// path itself uses — rather than a second hand-written field list. A
	// separate list drifts: this one already omitted args, model, auto_accept,
	// quiet and max_turns, so the most common edits reloaded silently, which
	// is the exact failure this feature exists to end.
	for _, name := range newCfg.HarnessOrder {
		oldH, ok := oldCfg.Harnesses[name]
		if !ok {
			continue
		}
		if !harnessDefEqual(oldH, newCfg.Harnesses[name]) {
			changes = append(changes, "changed harness "+name)
		}
	}
	// Profile changes, including membership: a czu delivery that only moves a
	// harness between profiles changes what autostarts, so logging nothing
	// would be the same silence in a different place.
	for _, name := range newCfg.ProfileOrder {
		oldP, ok := oldCfg.Profiles[name]
		if !ok {
			changes = append(changes, "added profile "+name)
			continue
		}
		newP := newCfg.Profiles[name]
		if !slices.Equal(oldP.Harnesses, newP.Harnesses) || oldP.Autostart != newP.Autostart {
			changes = append(changes, "changed profile "+name)
		}
	}
	for _, name := range oldCfg.ProfileOrder {
		if _, ok := newCfg.Profiles[name]; !ok {
			changes = append(changes, "removed profile "+name)
		}
	}

	if len(changes) == 0 {
		return nil
	}
	return changes
}
