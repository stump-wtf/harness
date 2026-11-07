package supervisor

// Per-Run Work Directories
//
// TL;DR: `workdir` may be a template, so every run of a one-shot can be born
// in a directory of its own instead of sharing one with every run before it.
// This file renders that template, creates the directory (and the per-run
// agent data directory beside it), and enforces `workdir_retain` by removing
// older run directories.
//
// Why the daemon creates the directory at all: the failure this exists for is
// a run reading state an earlier run left behind (plan files, another run's
// uncommitted edits), and a run pointed at a directory that does not exist
// yet either fails the spawn or, worse, the agent quietly makes one wherever
// it landed. So the daemon owns creating it, mode 0700 like every other
// directory it writes (ADR-0008), and owns removing the ones the operator
// asked it to keep a cap on.
//
// Retention deletes only what it can prove is a run directory: an entry under
// the parent the template renders into, matching the template's own literal
// shape, that is a real directory and not a symlink, carries this harness's
// own mark, and is not the directory of the run in flight. That proof is why config requires a retained template
// to vary only in its last element: `~/src/{{harness.name}}` plus a retain
// would otherwise point pruning at directories that were there before
// harness ever ran.
//
// Governing: ADR-0051 (proposed), ADR-0008; stump.wtf/harness#980.

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/tmpl"
)

// runDataDirName is the directory under a run's workdir that holds what the
// agent itself leaves behind (scratch files, its own session state). It is a
// dotfile so it never looks like project content to the agent, to a `git
// status`, or to whoever opens the directory afterwards.
const runDataDirName = ".harness-run"

// workdirPathNames enumerates every path a templated workdir may name, so a
// render with no run in flight can answer them all as absent rather than
// unresolved.
var workdirPathNames = []string{
	core.PathHarnessName, core.PathModel, core.PathPackageDir,
	core.PathRunID, core.PathRunTrigger, core.PathRunSource,
	core.PathRunStartedAt, core.PathRunDate,
}

// workdirValues is the template context a workdir renders against: the
// operator tier plus whatever the run actually has. A path the run lacks is
// absent, never empty, so a required reference fails the render instead of
// creating a directory named `runs/` and putting the run in it.
func workdirValues(h core.Harness, run RunEnv, now time.Time) map[string]string {
	v := map[string]string{core.PathHarnessName: h.Name}
	if model := h.CommandModel(); model != "" {
		v[core.PathModel] = model
	}
	if dir, ok := agentpkg.InstalledPackageDir(h.PackageSource); ok {
		v[core.PathPackageDir] = dir
	}
	started := run.StartedAt
	if started.IsZero() {
		started = now
	}
	v[core.PathRunStartedAt] = started.UTC().Format(time.RFC3339)
	v[core.PathRunDate] = started.In(scheduleZone(h.Schedule)).Format(time.DateOnly)
	if run.RunID > 0 {
		v[core.PathRunID] = strconv.Itoa(run.RunID)
	}
	if run.Trigger != "" {
		v[core.PathRunTrigger] = string(run.Trigger)
	}
	if run.Source != "" {
		v[core.PathRunSource] = run.Source
	}
	return v
}

// renderWorkdir renders a configured workdir for one run, home-expanded. An
// absent required path returns an error that unwraps to tmpl.ErrUnresolved,
// so the caller can tell "this run cannot have a directory" from a broken
// template.
func renderWorkdir(h core.Harness, run RunEnv, now time.Time) (string, error) {
	return renderWorkdirWith(h, run, now, false)
}

// WorkdirOf is the config-level answer for a templated workdir with no run in
// flight: the directory the run directories live in, with the varying element
// dropped. It is what `harness describe` and the skill-root resolution show,
// and it must never fail; a surface that names a harness's directory is not
// allowed to be the thing that breaks. A run's own directory comes from
// renderWorkdir.
func WorkdirOf(h core.Harness) string {
	rendered, err := renderWorkdirWith(h, RunEnv{}, time.Now(), true)
	if err != nil {
		// Unreachable for a config that passed load: every path a template may
		// name is answered. Be exact rather than clever about a hand-built
		// Harness whose template a load check never saw.
		return expandHome(h.Workdir)
	}
	return rendered
}

// renderWorkdirWith renders a workdir, leniently when lenient: a lenient
// render answers an absent path as empty and then cleans the result, which is
// how a template becomes the directory HOLDING the run directories.
func renderWorkdirWith(h core.Harness, run RunEnv, now time.Time, lenient bool) (string, error) {
	wd := expandHome(h.Workdir)
	if !core.WorkdirHasTemplate(wd) {
		return wd, nil
	}
	t, err := tmpl.Parse(wd)
	if err != nil {
		return "", err
	}
	values := workdirValues(h, run, now)
	if lenient {
		for _, p := range workdirPathNames {
			if _, ok := values[p]; !ok {
				values[p] = ""
			}
		}
	}
	out, err := t.Render(tmpl.Map{Values: values})
	if err != nil {
		return "", err
	}
	out = expandHome(out)
	if lenient {
		out = filepath.Clean(strings.TrimRight(out, string(os.PathSeparator)))
	}
	return out, nil
}

// prepareRunDirs gives one run its directories: the rendered workdir created
// 0700, and the agent data directory inside it. The data directory is the
// run's own by name when the workdir is shared between runs, and simply the
// directory inside the run's own workdir when it is not; either way one run
// cannot read another's scratch.
//
// Retention runs last, after this run's directory exists, so the cap counts
// the run in flight and the newest kept set is the one that survives.
func prepareRunDirs(h core.Harness, run RunEnv, now time.Time) (workdir, dataDir string, pruned int, err error) {
	templated := core.WorkdirHasTemplate(h.Workdir)
	workdir, err = renderWorkdir(h, run, now)
	if err != nil {
		return "", "", 0, err
	}
	if workdir == "" {
		// No directory was configured at all: the process inherits the daemon's
		// working directory and so does a hook. Nothing is created, so nothing
		// is ever pruned.
		return "", "", 0, nil
	}
	if err = os.MkdirAll(workdir, 0o700); err != nil {
		return "", "", 0, fmt.Errorf("create run workdir: %w", err)
	}
	if templated {
		// The marker is what makes pruning safe: it is the daemon's own claim
		// that this directory is one of this harness's run directories, so a
		// cap can never delete a directory that was there before it.
		if err = markRunDir(workdir, h.Name); err != nil {
			return workdir, "", 0, err
		}
	}
	dataDir = filepath.Join(workdir, runDataDirName)
	if !templated && run.RunID > 0 {
		// A shared workdir needs the run in the name, or two runs of the same
		// harness would share one data directory.
		dataDir = filepath.Join(dataDir, strconv.Itoa(run.RunID))
	}
	if err = os.MkdirAll(dataDir, 0o700); err != nil {
		return workdir, "", 0, fmt.Errorf("create run data dir: %w", err)
	}
	if h.WorkdirRetain > 0 && templated {
		parent, glob, ok := core.RunDirGlob(expandHome(h.Workdir), tmpl.Map{Values: workdirValues(h, run, now)})
		if ok {
			pruned = pruneRunDirs(parent, glob, h.Name, h.WorkdirRetain, filepath.Base(workdir))
		}
	}
	if !templated && run.RunID > 0 && h.KeepRuns > 0 {
		// The data directories under a shared workdir are bounded by the
		// harness's own run-record retention: they are named by run id, so the
		// two caps cannot disagree about how much history is kept.
		pruned += pruneNumberedDirs(filepath.Join(workdir, runDataDirName), h.KeepRuns, run.RunID)
	}
	return workdir, dataDir, pruned, nil
}

// runDirMarker is the file that marks a directory as one of this harness's run
// directories. Its name is a dotfile so it never looks like project content.
const runDirMarker = ".harness-run-dir"

// markRunDir claims a directory as a run directory of harnessName.
func markRunDir(dir, harnessName string) error {
	if err := os.WriteFile(filepath.Join(dir, runDirMarker), []byte(harnessName+"\n"), 0o600); err != nil {
		return fmt.Errorf("mark run workdir: %w", err)
	}
	return nil
}

// isMarkedRunDir reports whether dir carries the daemon's run-directory mark
// for harnessName. The mark names the harness that claimed the directory, so
// two harnesses that share a workdir parent cannot prune each other's run
// directories: a mark by anyone else is not this harness's to spend.
func isMarkedRunDir(dir, harnessName string) bool {
	b, err := os.ReadFile(filepath.Join(dir, runDirMarker))
	return err == nil && strings.TrimSpace(string(b)) == harnessName
}

// pruneRunDirs removes all but the newest keep run directories in parent
// matching glob, never keepingDir. An entry is a run directory only when it is
// a real directory (not a symlink), the template's own shape matches its name,
// and it carries this harness's own mark; anything else is left alone no
// matter how old, another harness's run directories included. It reports how
// many it removed.
func pruneRunDirs(parent, glob, harnessName string, keep int, keepingDir string) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	type candidate struct {
		name string
		mod  time.Time
	}
	var found []candidate
	for _, e := range entries {
		if e.Name() == keepingDir {
			continue
		}
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue // a file, a device, or a symlink is not a run directory
		}
		if glob != "*" {
			if ok, err := path.Match(glob, e.Name()); err != nil || !ok {
				continue
			}
		}
		if !isMarkedRunDir(filepath.Join(parent, e.Name()), harnessName) {
			continue // not a directory this harness created for a run
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		found = append(found, candidate{name: e.Name(), mod: info.ModTime()})
	}
	if len(found) <= keep-1 {
		return 0
	}
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	removed := 0
	for _, c := range found[keep-1:] {
		if err := os.RemoveAll(filepath.Join(parent, c.name)); err == nil {
			removed++
		}
	}
	return removed
}

// pruneNumberedDirs keeps the newest keep numeric entries under root, always
// including keepID. Only entries whose names are run ids are touched: whatever
// else lives in that directory belongs to someone else.
func pruneNumberedDirs(root string, keep, keepID int) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	type candidate struct {
		name string
		id   int
		mod  time.Time
	}
	var found []candidate
	for _, e := range entries {
		id, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if id == keepID {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		found = append(found, candidate{name: e.Name(), id: id, mod: info.ModTime()})
	}
	if len(found) <= keep-1 {
		return 0
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].mod.Equal(found[j].mod) {
			return found[i].mod.After(found[j].mod)
		}
		return found[i].id > found[j].id
	})
	removed := 0
	for _, c := range found[keep-1:] {
		if err := os.RemoveAll(filepath.Join(root, c.name)); err == nil {
			removed++
		}
	}
	return removed
}
