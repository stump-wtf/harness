package daemon

// Governing: SPEC-0002 REQ "Control Operations" — the daemon mirrors the CLI/TUI
// verbs 1:1 (list/describe/start/stop/restart/logs/profiles/use_profile/reload/
// daemon_info), idempotent where sensible (double-start is a no-op), with
// structured ERROR frames carrying a machine code + human message. ADR-0002
// (control is the same set of verbs the CLI and TUI expose). ADR-0006 (reload
// keeps last-good config on a parse error).
//
// @joestump-agent 09/27/2026 - Added the skills_synced and skills_status ops
// (SPEC-0007 REQ "Default-Branch Gate") and the skills refresh after a
// successful reload.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/skillmerge"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// handleControl decodes and services one CONTROL_REQ, replying with a
// CONTROL_RESP or a structured ERROR.
func (c *conn) handleControl(payload []byte) {
	var req protocol.ControlReq
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "malformed control request: %v", err)
		return
	}
	switch req.Op {
	case protocol.OpList:
		c.respond(req, c.opList())
	case protocol.OpDescribe:
		c.opDescribe(req)
	case protocol.OpStart, protocol.OpStop, protocol.OpRestart:
		c.opLifecycle(req)
	case protocol.OpEnable, protocol.OpDisable:
		c.opEnableDisable(req)
	case protocol.OpLogs:
		c.opLogs(req)
	case protocol.OpProfiles:
		c.respond(req, c.opProfiles())
	case protocol.OpUseProfile:
		c.opUseProfile(req)
	case protocol.OpReload:
		c.opReload(req)
	case protocol.OpDaemonInfo:
		c.respond(req, c.opDaemonInfo())
	case protocol.OpProjectUp:
		c.opProjectUp(req)
	case protocol.OpProjectDown:
		c.opProjectDown(req)
	case protocol.OpRemove:
		c.opRemove(req)
	case protocol.OpScratchRun:
		c.opScratchRun(req)
	case protocol.OpJobs:
		c.respond(req, c.opJobs())
	case protocol.OpTrigger:
		c.opTrigger(req)
	case protocol.OpRuns:
		c.opRuns(req)
	case protocol.OpNotifyTest:
		c.opNotifyTest(req)
	case protocol.OpTriggers:
		c.respond(req, c.opTriggers())
	case protocol.OpSkillsSynced:
		c.opSkillsSynced(req)
	case protocol.OpSkillsStatus:
		c.respond(req, c.opSkillsStatus())
	case protocol.OpCapture:
		c.opCapture(req)
	default:
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownOp, "unknown op %q", req.Op)
	}
}

// respond marshals data and writes a CONTROL_RESP; a marshal failure becomes an
// internal ERROR.
func (c *conn) respond(req protocol.ControlReq, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		_ = c.pc.WriteError(req.ID, protocol.ErrInternal, "encode response: %v", err)
		return
	}
	_ = c.pc.WriteJSON(protocol.TypeControlResp, &protocol.ControlResp{ID: req.ID, Op: req.Op, Data: raw})
}

// infoFor projects a snapshot + config record onto the wire HarnessInfo.
func (c *conn) infoFor(snap supervisor.Snapshot) protocol.HarnessInfo {
	info := protocol.HarnessInfo{
		Name:               snap.Name,
		State:              string(snap.State),
		Enabled:            snap.Enabled,
		ScheduleSuppressed: snap.OperatorStopped && snap.Scheduled,
		RestartCount:       snap.RestartCount,
		LastExitCode:       snap.LastExitCode,
		Flapping:           snap.Flapping,
		NextRetryInMs:      snap.NextRetryIn.Milliseconds(),
		ConfigChanged:      snap.ConfigChanged,
		PID:                snap.PID,
		SessionStalled:     snap.SessionStalled,
		SessionRotations:   snap.SessionRotations,
	}
	if !snap.LastStarted.IsZero() {
		info.LastStarted = snap.LastStarted.Format(time.RFC3339Nano)
	}
	if !snap.LastExitAt.IsZero() {
		info.LastExitAt = snap.LastExitAt.Format(time.RFC3339Nano)
	}
	// The last intent change (issue #835) rides the wire so describe can
	// answer who flipped the enabled intent and when, across a restart.
	if !snap.LastIntent.At.IsZero() {
		info.LastIntentAt = snap.LastIntent.At.Format(time.RFC3339Nano)
		info.LastIntentSource = snap.LastIntent.Source
		info.LastIntentPeer = snap.LastIntent.Peer
	}
	// HarnessRecord resolves the definition and provenance together under one
	// manager lock hold — so a list of N harnesses costs N+1 lock round-trips
	// instead of 2N+1, and Cmd/Backend and Project can never come from two
	// different registry states mid-project_down (SPEC-0004 REQ "Project
	// Naming And Namespacing"; ADR-0009).
	h, project, ok := c.srv.mgr.HarnessRecord(snap.Name)
	if ok {
		info.Adapter = h.Adapter
		info.Workdir = supervisor.Workdir(h)
		info.Args = h.Args
		info.Argv = h.Argv
		info.Transcripts = h.Transcripts
		info.Prompt = h.Prompt
		info.PromptFile = h.PromptFile
		info.PromptTemplate = h.PromptTemplate
		info.PromptTemplateFile = h.PromptTemplateFile
		info.Model = h.Model
		info.AutoAccept = h.AutoAccept
		info.MaxTurns = h.MaxTurns
		info.SystemPromptFile = h.SystemPromptFile
		info.MCPConfig = h.MCPConfig
		info.AllowedTools = h.AllowedTools
		info.Quiet = h.Quiet
		// The pin reference rides the wire so a TUI edit of a harness whose
		// file cannot be re-read still round-trips `source` instead of
		// silently converting the table to a bare one (SPEC-0026 REQ-7).
		info.Source = h.PackageSource
		info.PackageKeys = h.PackageKeys
		info.Skills = c.skillAttributions(h)
		info.Backend = string(h.Backend)
		info.Description = h.Description
		info.Schedule = h.Schedule
		info.OperatingHours = h.OperatingHours
		info.Triggers = c.triggerBindings(h)
	}
	info.Project = project
	// Next-run comes from the live cron, not the config snapshot: the spec
	// alone can't answer "when", and the daemon is the only party that knows
	// the resolved phase (ADR-0013).
	if info.Schedule != "" && c.srv.sched != nil {
		if next, ok := c.srv.sched.NextFire(snap.Name); ok {
			info.NextRun = next.Format(time.RFC3339)
		}
	}
	// Hold reasons (ADR-0027, SPEC-0021 REQ-14, REQ-16): every reason the
	// harness is held for, gated or not — a park or a spent budget holds an
	// ungated harness too. Omitted when it is not held; there is no `held`.
	info.HoldReasons = snap.Holds.Strings()
	// Operating-hours projection (ADR-0019, SPEC-0012 REQ "Operating Hours
	// Visibility"). Gated is the snapshot's own record of "has operating_hours"
	// (set on the actor loop, so it can never disagree with Holds/Closing);
	// info.OperatingHours is the wire discriminator clients read against.
	if snap.Gated {
		info.HoursShutdown = string(h.HoursShutdown)
		if c.srv.sched != nil {
			if in, next, hasNext, ok := c.srv.sched.HoursStatus(snap.Name); ok {
				info.InHours = in
				if hasNext {
					info.HoursNext = next.Format(time.RFC3339)
				}
			}
		}
		if snap.Closing && !snap.CloseAt.IsZero() {
			timeout := h.HoursShutdownTimeout
			if timeout <= 0 {
				timeout = core.DefaultHoursShutdownTimeout
			}
			info.ClosingUntil = snap.CloseAt.Add(timeout).Format(time.RFC3339)
		}
		if until, hasLease := c.srv.mgr.Lease(snap.Name); hasLease {
			info.LeaseUntil = until.Format(time.RFC3339)
		}
	}
	// Stuck-at-prompt projection (issue #735; ADR-0040). Computed here, on
	// both list and describe, so the table column and the FIELD row answer
	// identically; the screen walk it costs is a cell-grid read per running
	// harness, not an attach session.
	if hit, idle, waiting := c.waitingFor(snap, time.Now()); waiting {
		info.Waiting = true
		info.WaitingFor = hit.Label
		info.IdleMs = idle.Milliseconds()
	}
	return info
}

// opList returns every harness in config order (SPEC-0002 "list"; SPEC-0003
// glyphs are derived client-side from State).
func (c *conn) opList() []protocol.HarnessInfo {
	snaps := c.srv.mgr.Snapshots()
	out := make([]protocol.HarnessInfo, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, c.infoFor(s))
	}
	return out
}

// skillAttributions resolves the harness's merged skill set with its shadow
// map for describe (SPEC-0006 REQ "Ordered Merge and Shadowing": shadowed
// copies MUST remain enumerable). Pure local reads — the same roots
// spawn's projection draws from, never a fetch. Nil when the harness has
// no skill ground at all.
func (c *conn) skillAttributions(h core.Harness) []protocol.SkillAttribution {
	reg := adapter.NewRegistryWithDefaults()
	a := reg.Resolve(h)
	target := a.SkillTarget()
	if target == "" && len(h.SkillPaths) == 0 {
		return nil
	}
	// Global-file skill_paths are stored raw (the workdir convention) and
	// spawn expands them; describe expands the same way so the two surfaces
	// name the same directories.
	expand := func(d string) string {
		if strings.HasPrefix(d, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, d[1:])
			}
		}
		return d
	}
	var roots []skillmerge.Root
	// The package bundle tier, below the adapter defaults (SPEC-0026
	// REQ-10), attributed and shadowed like every other root.
	if dir, ok := agentpkg.BundleSkillsDir(h.PackageSource); ok {
		roots = append(roots, skillmerge.Root{Dir: dir, Tier: 0, Source: "package bundle"})
	}
	if h.UseDefaultSkillPaths {
		for _, d := range a.SkillRoots(supervisor.Workdir(h)) {
			roots = append(roots, skillmerge.Root{Dir: expand(d), Tier: 1, Source: "adapter default"})
		}
	}
	for _, d := range h.SkillPaths {
		roots = append(roots, skillmerge.Root{Dir: expand(d), Tier: 2, Source: "skill_paths"})
	}
	// The serving-clone store is out of bounds for roots, exactly as the
	// spawn-time projection excludes it — including a root equal to the
	// store directory itself.
	serving := filepath.Join(supervisor.StateHome(), "skills")
	servingPrefix := serving + string(filepath.Separator)
	kept := roots[:0]
	for _, r := range roots {
		if r.Dir == serving || strings.HasPrefix(r.Dir, servingPrefix) {
			continue
		}
		kept = append(kept, r)
	}
	set, _ := skillmerge.Resolve(kept)
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]protocol.SkillAttribution, 0, len(names))
	for _, name := range names {
		sk := set[name]
		sa := protocol.SkillAttribution{Name: name, Winner: sk.Winner, WinnerSource: sk.WinSrc}
		for _, sh := range sk.Shadowed {
			sa.Shadowed = append(sa.Shadowed, sh.Dir)
		}
		out = append(out, sa)
	}
	return out
}

// opDescribe returns one harness, or an unknown-harness ERROR.
func (c *conn) opDescribe(req protocol.ControlReq) {
	snap, ok := c.srv.mgr.Snapshot(req.Name)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	info := c.infoFor(snap)
	c.attachInfoFor(&info)
	c.respond(req, info)
}

// attachInfoFor decorates a describe payload with the harness's attach plane:
// the authoritative viewport and every live session, with the session(s)
// setting the smallest-attached-wins minimum flagged (#183). Describe only —
// list would pay a registry round-trip per harness for data nobody asked for.
// A harness with no Mux (never attached, no output teed) gets neither field.
func (c *conn) attachInfoFor(info *protocol.HarnessInfo) {
	ms, ok := c.srv.reg.SnapshotFor(info.Name)
	if !ok {
		return
	}
	// A harness served by a line mux (a structured one-shot) has sessions but
	// no terminal, so no viewport (internal/attach, lines.go).
	if ms.Cols > 0 && ms.Rows > 0 {
		info.AttachViewport = fmt.Sprintf("%dx%d", ms.Cols, ms.Rows)
	}
	sessions := make([]protocol.AttachSessionInfo, 0, len(ms.Sessions))
	for _, s := range ms.Sessions {
		sessions = append(sessions, protocol.AttachSessionInfo{
			ID:        s.ID,
			Mode:      string(s.Mode),
			Cols:      s.Cols,
			Rows:      s.Rows,
			CreatedAt: s.CreatedAt.Format(time.RFC3339),
			SetsMin:   s.SetsMin,
		})
	}
	info.AttachSessions = sessions
}

// opLifecycle handles start/stop/restart. Each is idempotent (SPEC-0002:
// double-start is a no-op success); an unknown harness is a structured ERROR.
//
// start carries the after-hours lease path (SPEC-0012 REQ "After-Hours
// Lease"): For set is an explicit lease length — validated, then applied
// through Manager.StartFor, which persists the lease synchronously before
// starting and rejects a harness to which no lease applies (ungated, or gated
// and in hours). For unset on a gated, out-of-hours harness applies the
// one-hour default lease through the same path; anything else starts as
// before.
func (c *conn) opLifecycle(req protocol.ControlReq) {
	var ok bool
	switch req.Op {
	case protocol.OpStart:
		forDur := time.Duration(0)
		if req.For != "" {
			d, err := time.ParseDuration(req.For)
			if err != nil || d <= 0 {
				_ = c.pc.WriteError(req.ID, protocol.ErrBadRequest,
					"for must be a positive duration (e.g. 2h30m), got %q", req.For)
				return
			}
			forDur = d
		} else if c.srv.mgr.LeaseApplies(req.Name) {
			// A plain start of a gated, out-of-hours harness leases the
			// default length (SPEC-0012 REQ "After-Hours Lease", scenario
			// "Late-night start": held at 21:00, not at the next tick).
			forDur = supervisor.DefaultLease
		}
		if forDur > 0 {
			if err := c.srv.mgr.StartFor(req.Name, forDur); err != nil {
				switch {
				case errors.Is(err, supervisor.ErrUnknownHarness):
					_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
				case errors.Is(err, supervisor.ErrNoLease):
					_ = c.pc.WriteError(req.ID, protocol.ErrBadRequest, "%v", err)
				default:
					_ = c.pc.WriteError(req.ID, protocol.ErrInternal, "leased start: %v", err)
				}
				return
			}
			// SPEC-0012 REQ "Operating Hours Visibility": a durable-log line
			// for the lease start, stating its end — the "next transition" a
			// gated harness's other lifecycle lines already carry. Read back
			// through Lease rather than the (durable but only in-memory
			// until now) forDur, so the line always reports what actually
			// landed, including an existing lease StartFor just replaced.
			if until, had := c.srv.mgr.Lease(req.Name); had {
				c.srv.mgr.LogLifecycle(req.Name, "lease start", "reason", "operating_hours", "until", until.Format(time.RFC3339))
			}
			ok = true
			break
		}
		ok = c.srv.mgr.StartPeer(req.Name, c.peer)
	case protocol.OpStop:
		ok = c.srv.mgr.StopPeer(req.Name, c.peer)
	case protocol.OpRestart:
		ok = c.srv.mgr.RestartPeer(req.Name, c.peer)
	}
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	// Reply with the fresh snapshot so the client can render the new state.
	snap, _ := c.srv.mgr.Snapshot(req.Name)
	c.respond(req, c.infoFor(snap))
}

// opEnableDisable handles enable/disable. Enable sets intent + starts; disable
// clears intent + stops. Both are idempotent; an unknown harness is a structured
// ERROR.
func (c *conn) opEnableDisable(req protocol.ControlReq) {
	var ok bool
	switch req.Op {
	case protocol.OpEnable:
		ok = c.srv.mgr.EnablePeer(req.Name, c.peer)
	case protocol.OpDisable:
		ok = c.srv.mgr.DisablePeer(req.Name, c.peer)
	}
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	snap, _ := c.srv.mgr.Snapshot(req.Name)
	c.respond(req, c.infoFor(snap))
}

// opLogs returns a tail of the harness's on-disk log (ADR-0007). Works for a
// live or crashed harness alike.
//
// The reply also carries the harness's authoritative viewport when one exists,
// because the log tail is raw PTY output: it only reconstructs into a screen at
// the geometry it was drawn at. Sourced from the same Mux the attach plane
// resizes, so the peek pane and an attach session agree on the guest's size
// instead of each guessing (ADR-0003 smallest-attached-wins).
//
// With Events set the reply is instead the structured activity view of one run
// (activity.go; SPEC-0006 REQ "Run Correlation", #302). Events is opt-in so
// every existing reader of the raw tail — the peek pane above all — is
// untouched.
func (c *conn) opLogs(req protocol.ControlReq) {
	snap, ok := c.srv.mgr.Snapshot(req.Name)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	lines := req.Lines
	if lines <= 0 {
		lines = 200
	}
	if req.Run > 0 {
		// One run of a scheduled harness (jobs.go; #120).
		c.opLogsRun(req, snap, lines)
		return
	}
	if req.Events {
		data, err := c.activity(req, snap, lines)
		if err != nil {
			_ = c.pc.WriteError(req.ID, protocol.ErrBadRequest, "logs %q: %v", req.Name, err)
			return
		}
		c.respond(req, data)
		return
	}
	text := readLogTail(c.srv.mgr.LogDir(), req.Name, lines)
	data := protocol.LogsData{Name: req.Name, Text: text}
	// A triggered structured one-shot writes its stdout to each run's stream
	// file, not to this log (ADR-0033; streamlog.go).
	if h, _, ok := c.srv.mgr.HarnessRecord(req.Name); ok && h.Triggered() && supervisor.RunsOnPipes(h) {
		data.Notices = append(data.Notices, fmt.Sprintf("%s runs on pipes: this log holds its lifecycle and stderr lines; each run's stdout is in its stream file (harness logs %s --run N --raw)", req.Name, req.Name))
	}
	// SnapshotFor never materializes a Mux (#183), so a harness nobody has
	// attached to and that has teed no output simply reports no viewport.
	if ms, ok := c.srv.reg.SnapshotFor(req.Name); ok {
		data.Cols, data.Rows = ms.Cols, ms.Rows
	}
	c.respond(req, data)
}

// opProfiles returns every profile, flagging the active one.
func (c *conn) opProfiles() []protocol.ProfileInfo {
	cfg := c.srv.mgr.Config()
	active := c.srv.mgr.ActiveProfile()
	out := make([]protocol.ProfileInfo, 0, len(cfg.ProfileOrder))
	for _, p := range cfg.OrderedProfiles() {
		out = append(out, protocol.ProfileInfo{
			Name:        p.Name,
			Description: p.Description,
			Harnesses:   p.Harnesses,
			Autostart:   p.Autostart,
			Active:      p.Name == active,
		})
	}
	return out
}

// opUseProfile activates a profile and broadcasts profile_changed.
func (c *conn) opUseProfile(req protocol.ControlReq) {
	if !c.srv.mgr.UseProfile(req.Profile) {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownProfile, "unknown profile %q", req.Profile)
		return
	}
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvProfileChange, Profile: req.Profile})
	c.respond(req, c.opProfiles())
}

// opReload re-parses the config file and applies it, keeping the last-good
// config on a parse error (ADR-0006). On success it broadcasts config_reloaded.
func (c *conn) opReload(req protocol.ControlReq) {
	if err := c.srv.mgr.ReloadFromFile(c.srv.configPath); err != nil {
		// Surface the location-carrying config error verbatim (SPEC-0001 reload
		// banner uses it).
		msg := err.Error()
		var cerr *config.Error
		if errors.As(err, &cerr) {
			msg = cerr.Error()
		}
		_ = c.pc.WriteError(req.ID, protocol.ErrReload, "%s", msg)
		return
	}
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvConfigReload})
	// Skill repos and their serving settings live in the same config of
	// record; refresh the serving manager so the index tracks the reload
	// (SPEC-0007 REQ "Skill Repos"). Best-effort: a refresh failure keeps the
	// previous index.
	go c.srv.SyncSkillsManager()
	c.respond(req, c.opList())
}

// opDaemonInfo returns daemon metadata.
func (c *conn) opDaemonInfo() protocol.DaemonInfo {
	resolved := c.srv.mgr.ProfileResolved()
	res := protocol.DaemonInfo{
		Version:       c.srv.version,
		ProtoVersion:  protocol.ProtoVersion,
		PID:           os.Getpid(),
		UptimeSeconds: timeSince(c.srv.started),
		Socket:        c.srv.socketPath,
		// The registered count — globals plus project harnesses — so the
		// number agrees with what list returns (SPEC-0004; ADR-0009).
		Harnesses:        c.srv.mgr.HarnessCount(),
		ActiveProfile:    c.srv.mgr.ActiveProfile(),
		ProfileResolved:  &resolved,
		DormantAutostart: c.srv.mgr.DormantAutostart(),
	}
	// Remote SSH (ADR-0004): report the live listener only when it actually
	// started, so clients can distinguish "off" from "enabled but refused".
	if addr, keys := c.srv.Remote(); addr != "" {
		res.SshAddr = addr
		res.SshKeys = keys
	}
	res.Notify = c.srv.notifyInfo()
	// The webhook listener, likewise only when it actually bound (SPEC-0014
	// REQ "Webhook Listener"), so doctor judges the live bind.
	res.WebhookAddr, res.WebhookTLS = c.srv.webhook()
	return res
}

// timeSince returns whole seconds elapsed since t.
func timeSince(t time.Time) int64 { return int64(time.Since(t).Seconds()) }
