// Agent-trace-backed log lines for a single harness.
//
// Governing: ADR-0007 as amended 2026-09-06 — for every session type the
// readable output surface is derived from agent-trace adapters over the tool's
// own session store, not from the raw PTY tee (which carries full-screen
// repaints for interactive TUI harnesses). `harness logs <name>` uses these
// helpers when the harness resolves to a native adapter; the generic adapter
// keeps the ansifold-filtered PTY fallback (SPEC-0006 REQ "Trajectory
// Discovery").

package trajectory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/adapter"
	"github.com/stump-wtf/agent-trace/tail"
)

// maxSnapshotSessions bounds a snapshot listing; the watcher's MaxAge does the
// same job for follow mode.
const maxSnapshotSessions = 50

// Line formats one watcher event as log lines, oldest first: mark lines
// (user/compaction/subagent) then the tool line. Mirrors the chatroom's
// rendering in plain text — `harness logs` output must stay pipe-friendly.
func Line(ev tail.Event) []string {
	var out []string
	for _, mk := range ev.Marks {
		ts := markTime(mk.Timestamp, ev.ReceivedAt)
		out = append(out, fmt.Sprintf("%s %s %s %s",
			ts, ev.Session.Harness, strings.ToUpper(mk.Type), mk.Note))
	}
	c := ev.Classified
	if c.Tool == "" && c.Summary == "" {
		return out
	}
	status := ""
	if c.IsError {
		status = "[ERROR]"
	} else if c.ResultBytes > 0 {
		status = "[OK]"
	}
	line := fmt.Sprintf("%s %s %s %s %s %s",
		markTime(c.Timestamp, ev.ReceivedAt), ev.Session.Harness,
		c.Action, c.Tool, c.Summary, status)
	if len(c.Targets) > 0 {
		paths := make([]string, 0, len(c.Targets))
		for _, t := range c.Targets {
			paths = append(paths, t.Path)
		}
		line += "  (" + strings.Join(paths, ", ") + ")"
	}
	return append(out, strings.TrimSpace(line))
}

func markTime(ts string, recv time.Time) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil && !t.IsZero() {
		return t.Format("15:04:05")
	}
	return recv.Format("15:04:05")
}

// ExtraCrushRegistries returns projects.json paths under crush-*/ siblings of
// the default crush data dir. A harness that repoints CRUSH_GLOBAL_DATA
// (crush-signal) registers its sessions only in its own registry — and on
// machines where the default projects.json is unreadable (it is not written
// atomically, and two crush instances can interleave writes into a
// concatenated, unparseable file) the extra registries are the ONLY live
// source. Shared by the chatroom view and the `harness logs` tail so both see
// every crush session. Consumers must dedupe events by session key + seq,
// which both do.
func ExtraCrushRegistries() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(home, ".local", "share", "crush-*", "projects.json"))
	if err != nil {
		return nil
	}
	return matches
}

// LiveAdapters returns every adapter the live surfaces (chatroom, logs tail)
// should watch: agent-trace's defaults plus one extra CrushAdapter per
// alternate registry from ExtraCrushRegistries.
func LiveAdapters() []tail.Adapter {
	adapters := tail.DefaultAdapters()
	for _, reg := range ExtraCrushRegistries() {
		adapters = append(adapters, &tail.CrushAdapter{ProjectsPath: reg})
	}
	return adapters
}

// HarnessSnapshotLines returns the most recent agent-trace lines for a
// harness's own session store, oldest first and capped to limit. Returns
// (nil, nil) when the harness has no native adapter — callers then fall back
// to the raw PTY tail.
func HarnessSnapshotLines(ctx context.Context, reg *adapter.Registry, adapterName, workdir string, limit int) ([]string, error) {
	tads := adaptersForKind(reg, adapterName)
	if len(tads) == 0 {
		return nil, nil
	}
	filter := tail.SessionFilter{}
	if workdir != "" {
		filter.Cwd = workdir
	}
	type timed struct {
		at   time.Time
		line string
	}
	var all []timed
	seen := map[string]struct{}{}
	for _, td := range tads {
		sessions, err := tail.ListSessionsFiltered(ctx, td, filter)
		if err != nil {
			continue // one unreadable registry must not hide the others
		}
		sort.Slice(sessions, func(i, j int) bool {
			ti, oki := sessions[i].Ended()
			tj, okj := sessions[j].Ended()
			if !oki || !okj {
				return sessions[i].EndedAt > sessions[j].EndedAt
			}
			return ti.After(tj)
		})
		for _, sm := range sessions {
			if len(seen) >= maxSnapshotSessions {
				break
			}
			if _, dup := seen[sm.Key]; dup {
				continue
			}
			seen[sm.Key] = struct{}{}
			events, _, _, err := td.Parse(ctx, sm.Path)
			if err != nil {
				continue
			}
			for _, c := range events {
				ev := tail.Event{Session: sm, Classified: c, ReceivedAt: time.Now()}
				for _, line := range Line(ev) {
					all = append(all, timed{at: eventSortTime(c.Timestamp, ev.ReceivedAt), line: line})
				}
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.line)
	}
	return out, nil
}

func eventSortTime(ts string, recv time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, ts); err == nil && !t.IsZero() {
		return t
	}
	return recv
}

// FollowHarnessEvents streams formatted agent-trace lines for a harness to
// emit as they arrive, starting with the watcher's initial history scan. It
// blocks until ctx is cancelled. Returns (nil, nil) when the harness has no
// native adapter.
func FollowHarnessEvents(ctx context.Context, reg *adapter.Registry, adapterName, workdir string, emit func(string)) error {
	tads := adaptersForKind(reg, adapterName)
	if len(tads) == 0 {
		return nil
	}
	cfg := tail.DefaultWatchConfig()
	w := tail.NewWatcherWithConfig(cfg, tads)
	w.Start(ctx)
	defer w.Stop()
	seen := map[string]struct{}{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events():
			if !ok {
				return nil
			}
			d := ev.Session.Key + ":" + fmt.Sprint(ev.Classified.Seq)
			if _, dup := seen[d]; dup {
				continue
			}
			seen[d] = struct{}{}
			for _, line := range Line(ev) {
				emit(line)
			}
		}
	}
}

// adaptersForKind resolves a harness adapter name to the tail adapters to
// query for it: the harness's own adapter, plus the alternate crush
// registries when it is crush (see ExtraCrushRegistries). Empty means no
// native trajectory — use the raw PTY fallback.
func adaptersForKind(reg *adapter.Registry, adapterName string) []tail.Adapter {
	adp, err := reg.Get(adapterName)
	if err != nil {
		return nil
	}
	td := adp.TailAdapter()
	if td == nil {
		return nil
	}
	out := []tail.Adapter{td}
	if adp.Name() == "crush" {
		for _, extra := range ExtraCrushRegistries() {
			out = append(out, &tail.CrushAdapter{ProjectsPath: extra})
		}
	}
	return out
}
