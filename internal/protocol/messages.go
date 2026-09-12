package protocol

// Governing: SPEC-0002 REQ "Handshake And Versioning" (HELLO with proto/client/
// daemon versions; same major REQUIRED), REQ "Control Operations" (the JSON
// request/response verbs and structured ERROR), REQ "Event Subscription" (the
// EVENT payloads), and REQ "Attach Session" (ATTACH_OPEN/DATA/RESIZE/CLOSE with
// a session id). ADR-0002 (control mirrors the CLI/TUI verbs 1:1).

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// ProtoMajor / ProtoMinor are this build's protocol version. The MAJOR must
// match between client and daemon (SPEC-0002 REQ "Handshake And Versioning");
// MINOR is informational (additive changes only).
const (
	ProtoMajor = 1
	// ProtoMinor 1 added the project compose ops (project_up/project_down,
	// SPEC-0004) — additive only.
	// ProtoMinor 2 added the prompt field on ProjectHarness and HarnessInfo
	// (agent one-shot harnesses, ADR-0011) — additive only.
	// ProtoMinor 3 added the model field on ProjectHarness and HarnessInfo
	// (agent model selection, issue #57) — additive only.
	// ProtoMinor 4 added the auto_accept field on ProjectHarness and
	// HarnessInfo (agent unattended mode, issue #58) — additive only.
	// ProtoMinor 5 added AttachViewport and AttachSessions on HarnessInfo
	// (describe-only attach session visibility, issue #183) — additive only.
	// ProtoMinor 6 added Cols/Rows on LogsData so the peek pane replays the
	// tail at the guest's authoritative viewport instead of the pane's — additive
	// only.
	// ProtoMinor 7 added the structured activity view of logs (Events, Since,
	// Until and IncludeAmbiguous on ControlReq; Source, Run, Entries, Excluded
	// and Notices on LogsData) and LastStarted/LastExitAt on HarnessInfo
	// (issues #302, #89) — additive only. A daemon older than 7 ignores Events
	// and answers with the raw Text, which a newer client prints as before.
	// ProtoMinor 8 added scheduled-run visibility (issue #120): the jobs,
	// trigger and runs ops; Run and Limit on ControlReq (Run selects one run
	// of a scheduled harness for logs); JobInfo, RunInfo, RunsData and
	// TriggerData; the not_scheduled and unknown_run error codes; and the
	// job_run_started, job_run_finished and job_schedule_changed events with
	// RunID, Trigger, Outcome, ExitCode, DurationMs and NextRunAt on EventMsg
	// — additive only. A daemon older than 8 answers the new ops with
	// unknown_op, and ignores Run, so it returns the harness-wide log.
	//
	// It also records fields that reached the wire earlier without a bump of
	// their own, so this list is complete rather than silently short. On
	// HarnessInfo: Adapter, Workdir, PromptFile, MaxTurns, Quiet, Project,
	// Schedule and NextRun. On DaemonInfo: ProfileResolved, DormantAutostart,
	// SshAddr and SshKeys. All additive and omitempty, so no reader broke; they
	// simply shipped under whatever minor was current.
	//
	// ProtoMinor 9 added Args on HarnessInfo (issue #330): the harness's
	// configured argv, which carries the flag naming a crush instance's
	// session store (--data-dir). A client correlating sessions to harnesses
	// needs it to tell apart several harnesses sharing one working directory,
	// where the store is the only thing that differs — additive only. A daemon
	// older than 9 omits it, and correlation infers the store from the workdir
	// as it did before.
	ProtoMinor = 9
)

// ProtoVersion is the "major.minor" string carried in HELLO.
var ProtoVersion = fmt.Sprintf("%d.%d", ProtoMajor, ProtoMinor)

// ---- HELLO (SPEC-0002 REQ "Handshake And Versioning") --------------------

// Hello is the handshake payload, sent by both sides. A client fills
// ClientVersion + Wants; the daemon replies with DaemonVersion + Capabilities.
type Hello struct {
	ProtoVersion  string   `json:"proto_version"`
	ClientVersion string   `json:"client_version,omitempty"`
	DaemonVersion string   `json:"daemon_version,omitempty"`
	Wants         []string `json:"wants,omitempty"`        // e.g. ["control","events"]
	Capabilities  []string `json:"capabilities,omitempty"` // daemon → client
}

// Major parses the MAJOR component of a "major.minor" proto version string.
func Major(version string) (int, error) {
	var maj, min int
	if _, err := fmt.Sscanf(version, "%d.%d", &maj, &min); err != nil {
		// Tolerate a bare major.
		if _, err2 := fmt.Sscanf(version, "%d", &maj); err2 != nil {
			return 0, fmt.Errorf("protocol: malformed proto version %q", version)
		}
	}
	return maj, nil
}

// ---- Control plane (SPEC-0002 REQ "Control Operations") ------------------

// Op is a control verb. The set mirrors the CLI verbs and the TUI 1:1
// (ADR-0002).
type Op string

const (
	OpList       Op = "list"
	OpDescribe   Op = "describe"
	OpStart      Op = "start"
	OpStop       Op = "stop"
	OpRestart    Op = "restart"
	OpEnable     Op = "enable"
	OpDisable    Op = "disable"
	OpLogs       Op = "logs"
	OpProfiles   Op = "profiles"
	OpUseProfile Op = "use_profile"
	OpReload     Op = "reload"
	OpDaemonInfo Op = "daemon_info"

	// Project compose ops. Governing: ADR-0009 (project-scoped compose),
	// SPEC-0004 REQ "Project Control Operations" and REQ "Remove".
	// project_up registers and starts (or reconciles) a project's harnesses
	// under the <project>/<name> namespace; project_down stops and
	// deregisters them; remove stops and deregisters ONE registered harness
	// (the single-member tear-down `harness rm` maps to).
	OpProjectUp   Op = "project_up"
	OpProjectDown Op = "project_down"
	OpRemove      Op = "remove"

	// Scratchpad op. Governing: ADR-0017 (ephemeral scratchpads), SPEC-0011
	// REQ "Control Operation": scratch_run registers and starts ONE ad-hoc
	// harness under a daemon-minted random name with scratch provenance; it
	// is never persisted and dies with the daemon.
	OpScratchRun Op = "scratch_run"

	// Scheduled-run ops. Governing: ADR-0013, SPEC-0008 REQ "Protocol
	// Operations", REQ "Manual Trigger". jobs lists every scheduled harness
	// with its next window and latest run; trigger starts a manual run through
	// the same path a schedule firing takes (so on_overlap applies); runs
	// returns one harness's run history, newest first.
	OpJobs    Op = "jobs"
	OpTrigger Op = "trigger"
	OpRuns    Op = "runs"
)

// ControlReq is a control-plane request. ID correlates the response; Name
// targets a harness (start/stop/restart/describe/logs) or names the project
// (project_up/project_down); Profile targets use_profile; Lines/Follow tune
// logs; Harnesses carries the project definitions for project_up.
type ControlReq struct {
	ID        uint64           `json:"id"`
	Op        Op               `json:"op"`
	Name      string           `json:"name,omitempty"`
	Profile   string           `json:"profile,omitempty"`
	Lines     int              `json:"lines,omitempty"`
	Follow    bool             `json:"follow,omitempty"`
	Harnesses []ProjectHarness `json:"harnesses,omitempty"`

	// Events asks logs for the structured activity view of one run — agent
	// events attributed to the harness, interleaved with lifecycle lines —
	// instead of the raw durable-log tail. Governing: SPEC-0002 REQ "Control
	// Operations", SPEC-0006 REQ "Run Correlation" (#302).
	Events bool `json:"events,omitempty"`
	// Since/Until (RFC 3339) pin the run window Events describes. Unset, the
	// daemon describes the harness's latest run. This is the seam a run
	// selector (`logs --run N`, #120) resolves a run record into.
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// IncludeAmbiguous adds sessions more than one harness could have
	// written, flagged as ambiguous. Off by default: correlation fails closed.
	IncludeAmbiguous bool `json:"include_ambiguous,omitempty"`

	// Run selects one run of a scheduled harness by run id (issue #120). On
	// logs it scopes the reply to that run: the raw view reads the run's own
	// log, and the events view uses the run record's exact window, overriding
	// Since/Until. Zero means the harness-wide behavior.
	Run int `json:"run,omitempty"`
	// Limit caps the records runs returns, newest first. Zero means 20.
	Limit int `json:"limit,omitempty"`
}

// ProjectHarness is one project-local harness definition carried by a
// project_up request. Fields mirror the [harness.*] schema (SPEC-0004 REQ
// "Project File Schema"); Name is the project-local name — the daemon
// namespaces it to <project>/<name> at registration (SPEC-0004 REQ "Project
// Naming And Namespacing"). Governing: ADR-0009 (project-scoped compose).
type ProjectHarness struct {
	Name string `json:"name"`
	// Harness is the harness-kind enum ("crush", "claude-code", "codex",
	// "generic"); empty means the default, "crush". It selects the adapter
	// and the executable (ADR-0011).
	Harness string   `json:"harness,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Prompt mirrors the schema's agent one-shot `prompt`: exactly one of
	// harness/prompt defines the argv, and a prompt harness carries empty
	// args — the daemon synthesizes its argv at spawn time (ADR-0011).
	Prompt string `json:"prompt,omitempty"`
	// Model mirrors the schema's agent `model` selection: set only alongside
	// prompt (parse validation enforces it); the daemon folds it into the
	// synthesized argv at spawn (ADR-0011, issue #57).
	Model string `json:"model,omitempty"`
	// AutoAccept mirrors the schema's agent `auto_accept` unattended mode: set
	// only alongside prompt (parse validation enforces it); the daemon folds
	// the vendor's yolo flag into the synthesized argv at spawn (ADR-0011,
	// issue #58).
	AutoAccept bool `json:"auto_accept,omitempty"`
	// MaxTurns mirrors the schema's agent `max_turns` budget: set only
	// alongside prompt (parse validation enforces it); the daemon folds
	// --max-turns into the synthesized argv at spawn (ADR-0011, issue #59).
	// 0 means unset/unlimited.
	MaxTurns int `json:"max_turns,omitempty"`
	// Quiet mirrors the schema's agent `quiet` headless switch: a *bool so an
	// omitted key (nil = the headless one-shot default) is distinguishable from
	// an explicit false (stream output to an attach). Set only alongside prompt
	// (parse validation enforces it); the daemon folds --quiet into (or out of)
	// the synthesized argv at spawn (ADR-0011, issue #60).
	Quiet *bool `json:"quiet,omitempty"`
	// PromptFile mirrors the schema's `prompt_file`: the PATH to the file
	// holding the instruction, never its contents (ADR-0018). Clients show
	// and round-trip the path; the daemon reads the file at spawn.
	PromptFile     string `json:"prompt_file,omitempty"`
	Workdir        string `json:"workdir,omitempty"`
	EnvFile        string `json:"env_file,omitempty"`
	RestartDelayMs int64  `json:"restart_delay_ms,omitempty"`
	// Restart mirrors the schema's `restart` policy (core.RestartPolicy);
	// empty means the always-restart default, matching an omitted key.
	Restart     string `json:"restart,omitempty"`
	Backend     string `json:"backend,omitempty"`
	Description string `json:"description,omitempty"`
	TmuxSocket  string `json:"tmux_socket,omitempty"`
	// Enabled mirrors the schema's `enabled`: a disabled harness is registered
	// (visible to list/ps) but not started by project_up — the same way the
	// global config's `enabled` gates autostart (SPEC-0004 REQ "Project File
	// Schema": identical field meanings).
	Enabled bool `json:"enabled,omitempty"`
}

// ControlResp is a successful control-plane response. Data holds the op-specific
// JSON payload (a HarnessInfo list for list, etc.).
type ControlResp struct {
	ID   uint64          `json:"id"`
	Op   Op              `json:"op"`
	Data json.RawMessage `json:"data,omitempty"`
}

// HarnessInfo is one harness's state for list/describe (SPEC-0003 fields; the
// glyph is derived client-side from State). It is the JSON projection of a
// supervisor.Snapshot plus the config-derived Cmd/Backend/Description.
type HarnessInfo struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	Enabled       bool   `json:"enabled"`
	RestartCount  int    `json:"restart_count"`
	LastExitCode  int    `json:"last_exit_code"`
	Flapping      bool   `json:"flapping"`
	NextRetryInMs int64  `json:"next_retry_in_ms,omitempty"`
	ConfigChanged bool   `json:"config_changed,omitempty"`
	PID           int    `json:"pid,omitempty"`
	// Adapter is the harness-kind enum the harness selected ("crush" default).
	Adapter string `json:"adapter,omitempty"`
	// Workdir is the resolved process working directory the supervisor spawns
	// into — the same value it assigns to cmd.Dir, with a leading ~ already
	// expanded. Empty when the harness configured none (it then inherits the
	// daemon's cwd, which identifies nothing).
	//
	// It is on the wire so a client can correlate an agent session transcript
	// back to the harness that produced it: the transcript records the cwd the
	// agent ran in and nothing else that names a harness (ADR-0015 dashboard
	// activity).
	Workdir string `json:"workdir,omitempty"`
	// Args is the harness's configured argv, as spawned. It is on the wire
	// because it carries the flag that names a crush instance's session store
	// (--data-dir): a working directory shared by several harnesses cannot
	// tell them apart, and the store can, so without this a client attributes
	// nothing on a host where the agents all work in one tree (SPEC-0006 REQ
	// "Run Correlation"; issue #330).
	Args []string `json:"args,omitempty"`
	// LastStarted / LastExitAt (RFC 3339) bound the harness's latest run, so a
	// client can attribute a session to the harness whose run covers it, not
	// merely to one sharing its workdir (SPEC-0006 REQ "Run Correlation";
	// ADR-0015 chatroom identity). Empty when never started / never exited.
	LastStarted string `json:"last_started,omitempty"`
	LastExitAt  string `json:"last_exit_at,omitempty"`
	// Prompt is the agent one-shot instruction for a prompt harness; the
	// argv is synthesized at spawn from the same adapter (ADR-0011).
	Prompt string `json:"prompt,omitempty"`
	// PromptFile is the PATH to a file holding the instruction, the
	// alternative to an inline Prompt and mutually exclusive with it. The
	// daemon reads the file at spawn, so the contents never travel on the
	// wire — a client shows and round-trips the path (ADR-0018).
	PromptFile string `json:"prompt_file,omitempty"`
	// Model is the agent model selection for a prompt harness, folded into the
	// synthesized argv at spawn (issue #57). Empty for cmd harnesses.
	Model string `json:"model,omitempty"`
	// AutoAccept is the agent unattended/yolo mode for a prompt harness,
	// folded into the synthesized argv at spawn (issue #58). Always false for
	// cmd harnesses.
	AutoAccept bool `json:"auto_accept,omitempty"`
	// MaxTurns is the agent turn budget for a prompt harness, folded into the
	// synthesized argv at spawn (issue #59). Always 0 for cmd harnesses.
	MaxTurns int `json:"max_turns,omitempty"`
	// Quiet is the agent headless switch for a prompt harness, folded into the
	// synthesized argv at spawn (issue #60). Always true for prompt harnesses
	// unless the config set quiet = false.
	Quiet       bool   `json:"quiet,omitempty"`
	Backend     string `json:"backend,omitempty"`
	Description string `json:"description,omitempty"`
	// Project is the harness's provenance: the owning project's name for a
	// project-registered harness (its Name is then "<project>/<local>"), empty
	// for a global-config harness. Lets `down`/`ps` scope correctly (SPEC-0004
	// REQ "Project Naming And Namespacing"; ADR-0009).
	Project string `json:"project,omitempty"`
	// Schedule is the cron expression from config for a daemon-scheduled
	// one-shot harness (ADR-0013; SPEC-0008 REQ "Schedule Key"). Empty for an
	// always-on or purely manual harness.
	Schedule string `json:"schedule,omitempty"`
	// NextRun is when Schedule next fires, RFC 3339 local time. Empty when
	// there is no schedule or the daemon has not resolved a firing time yet.
	NextRun string `json:"next_run,omitempty"`
	// AttachViewport is the authoritative (smallest-attached-wins) viewport the
	// guest PTY is sized to, "colsxrows" ("80x24"). Present when a Mux exists
	// — i.e. someone has attached, or the supervisor teed output — so "why is
	// my guest 80 columns wide?" is answerable from describe alone (#183).
	// Describe only; list omits it.
	AttachViewport string `json:"attach_viewport,omitempty"`
	// AttachSessions lists every live attach session with the one(s) setting
	// the minimum flagged, so a stale client clamping the PTY for everyone
	// else is visible instead of only discoverable with lsof (#183).
	// Describe only; list omits it.
	AttachSessions []AttachSessionInfo `json:"attach_sessions,omitempty"`
}

// AttachSessionInfo is one live attach session on a harness (#183).
type AttachSessionInfo struct {
	ID uint32 `json:"id"`
	// Mode is "rw" or "ro" (ADR-0008).
	Mode string `json:"mode"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	// CreatedAt is when the session opened, RFC3339 — the client renders age.
	CreatedAt string `json:"created_at"`
	// SetsMin marks a session whose viewport defines the current
	// smallest-attached-wins minimum on at least one axis.
	SetsMin bool `json:"sets_minimum,omitempty"`
}

// ProfileInfo is one profile for the profiles op.
type ProfileInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Harnesses   []string `json:"harnesses"`
	Autostart   bool     `json:"autostart"`
	Active      bool     `json:"active"`
}

// LogsData is the logs op response payload.
type LogsData struct {
	Name string `json:"name"`
	Text string `json:"text"`
	// Cols/Rows are the harness's authoritative (smallest-attached-wins)
	// viewport — the geometry Text was drawn at. A client that replays the tail
	// through a terminal emulator (the TUI's peek pane) MUST size that emulator
	// to these dimensions and crop, not to its own pane: replaying a 156-column
	// guest into a 90-column emulator wraps every line and lands cursor-
	// addressed content in the wrong cells, which is the same "not 100%x100%"
	// class of bug the attach path fixed by negotiating a size (ADR-0003).
	// Absent (0) when no Mux exists for the harness — nothing has ever teed
	// output for it — or when the daemon predates ProtoMinor 6; the client then
	// falls back to its own geometry, the historical behaviour.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`

	// Source is LogSourceAgentTrace when the reply is the structured activity
	// view (Run/Entries). Empty means Text is the rendering — a raw request, a
	// daemon older than ProtoMinor 7, or a harness whose adapter records no
	// native trajectory (generic), for which the durable log is the record
	// (ADR-0007).
	Source string `json:"source,omitempty"`
	// Run is the run window the entries describe.
	Run *LogRun `json:"run,omitempty"`
	// Entries is the run's lifecycle and agent activity, oldest first, trimmed
	// to the requested line count.
	Entries []LogEntry `json:"entries,omitempty"`
	// Excluded lists sessions inside the window that more than one harness
	// could have written. They are never attributed (SPEC-0006 REQ "Run
	// Correlation"); listing them keeps the exclusion observable.
	Excluded []LogExclusion `json:"excluded,omitempty"`
	// Notices explain a degraded view: no attributable session, an unreadable
	// store, a run window recovered from the log. With no agent activity the
	// reply also carries the durable-log tail in Text.
	Notices []string `json:"notices,omitempty"`
}

// LogSourceAgentTrace marks a logs reply rendered from agent-trace sessions.
const LogSourceAgentTrace = "agent-trace"

// LogRun is the run window a structured logs reply describes. Times are
// RFC 3339.
type LogRun struct {
	Start string `json:"start"`
	// End is empty while the run is in flight.
	End string `json:"end,omitempty"`
	// ExitCode is set when the run ended with a recorded exit.
	ExitCode *int   `json:"exit_code,omitempty"`
	Adapter  string `json:"adapter,omitempty"`
	Workdir  string `json:"workdir,omitempty"`
}

// Kinds of LogEntry.
const (
	LogEntryLifecycle = "lifecycle"
	LogEntrySession   = "session"
	LogEntryTool      = "tool"
	LogEntryMark      = "mark"
)

// LogEntry is one line of a run's activity.
type LogEntry struct {
	// ID is stable across repeated requests, so a follower prints each entry
	// once.
	ID   string `json:"id"`
	Time string `json:"time"`
	// Kind is one of the LogEntry* constants.
	Kind string `json:"kind"`
	// Action is the lifecycle message ("state", "exited", "flapping"), the
	// classified tool action ("read", "edit", "exec", "search", "verify",
	// "other"), the mark type ("user-message", "error", …) or "session".
	Action  string `json:"action"`
	Tool    string `json:"tool,omitempty"`
	Target  string `json:"target,omitempty"`
	Summary string `json:"summary,omitempty"`
	Session string `json:"session,omitempty"`
	Error   bool   `json:"error,omitempty"`
	// Ambiguous marks an entry from an excluded session, present only on an
	// IncludeAmbiguous request.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

// LogExclusion is a session correlation refused to attribute.
type LogExclusion struct {
	Session   string   `json:"session"`
	StartedAt string   `json:"started_at"`
	Claimants []string `json:"claimants"`
}

// ProjectUpData is the project_up response payload: the project's harnesses
// (fully-qualified names) and their fresh states, so the CLI can print its
// one-shot status table (SPEC-0004 REQ "Bring Up"). Governing: ADR-0009.
type ProjectUpData struct {
	Project   string        `json:"project"`
	Harnesses []HarnessInfo `json:"harnesses"`
}

// ProjectDownData is the project_down response payload: the fully-qualified
// harness names that were stopped and deregistered (SPEC-0004 REQ "Tear
// Down"). Governing: ADR-0009.
type ProjectDownData struct {
	Project string   `json:"project"`
	Removed []string `json:"removed"`
}

// RemoveData is the remove response payload: the fully-qualified harness
// name that was stopped and deregistered, and the owning project (SPEC-0004
// REQ "Remove"). Governing: ADR-0009.
type RemoveData struct {
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
}

// ScratchRunData is the scratch_run response payload: the daemon-minted name
// and the scratchpad's fresh state (SPEC-0011 REQ "Control Operation").
// Governing: ADR-0017.
type ScratchRunData struct {
	Name string      `json:"name"`
	Info HarnessInfo `json:"harness"`
}

// RunInfo is one run record of a scheduled harness (SPEC-0008 REQ "Run
// History"). Times are RFC 3339. It carries outcomes, times and exit codes
// only — never environment, prompt or output (ADR-0008).
type RunInfo struct {
	RunID int `json:"run_id"`
	// Trigger is "schedule", "manual" or "catch_up".
	Trigger string `json:"trigger"`
	// Outcome is "running", "success", "failed", "timed_out", "skipped",
	// "replaced", "missed", "cancelled" or "interrupted".
	Outcome   string `json:"outcome"`
	StartedAt string `json:"started_at"`
	// EndedAt is empty while running, and for a run a crashed daemon left
	// behind, whose real end is unknown.
	EndedAt string `json:"ended_at,omitempty"`
	// DurationMs is set once the run has ended.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// ExitCode is set only when a process was reaped (-1 if signalled).
	ExitCode *int `json:"exit_code,omitempty"`
	// Window is the schedule window the record honors; FirstWindow and
	// Windows describe what a missed record or a catch-up run covers.
	Window      string `json:"window,omitempty"`
	FirstWindow string `json:"first_window,omitempty"`
	Windows     int    `json:"windows,omitempty"`
	// HasLog reports whether the run has a log to read with logs --run.
	HasLog bool `json:"has_log,omitempty"`
}

// JobInfo is one scheduled harness for the jobs op (SPEC-0008 REQ "Protocol
// Operations"). NextRun is computed daemon-side from the live scheduler, so a
// client renders "in 6h" without doing cron math.
type JobInfo struct {
	Name        string `json:"name"`
	Schedule    string `json:"schedule"`
	Description string `json:"description,omitempty"`
	// State is the harness state, as on HarnessInfo.
	State string `json:"state"`
	// NextRun is RFC 3339; empty when the schedule never fires again.
	NextRun string `json:"next_run,omitempty"`
	CatchUp bool   `json:"catch_up,omitempty"`
	// TimeoutMs bounds each run; 0 means no limit.
	TimeoutMs int64  `json:"timeout_ms"`
	OnOverlap string `json:"on_overlap"`
	KeepRuns  int    `json:"keep_runs"`
	// Running is the run in flight, if any.
	Running *RunInfo `json:"running,omitempty"`
	// LastRun is the newest finished record, if any — including a decision
	// that started no process (skipped, missed).
	LastRun *RunInfo `json:"last_run,omitempty"`
	// ConsecutiveFailures counts the newest runs that failed or timed out,
	// back to the latest success. Records that pass no verdict on the harness
	// (skipped, missed, replaced, cancelled, interrupted) neither count nor
	// reset it.
	ConsecutiveFailures int `json:"consecutive_failures"`
}

// RunsData is the runs op response payload.
type RunsData struct {
	Name string `json:"name"`
	// Runs is newest first, capped at the request's Limit.
	Runs []RunInfo `json:"runs"`
}

// Trigger decisions.
const (
	// TriggerStarted: the run started a process (on_overlap = "replace" may
	// have stopped a run in flight first).
	TriggerStarted = "started"
	// TriggerQueued: a run is in flight and on_overlap = "queue" is holding
	// this one; it starts when that run ends, under a run id not yet assigned.
	TriggerQueued = "queued"
	// TriggerSkipped: a run is in flight (or the harness is stopping) and the
	// trigger was recorded skipped.
	TriggerSkipped = "skipped"
)

// TriggerData is the trigger op response payload.
type TriggerData struct {
	Name string `json:"name"`
	// Decision is one of the Trigger* constants.
	Decision string `json:"decision"`
	// Run is the started run, or the skipped record; nil when queued.
	Run *RunInfo `json:"run,omitempty"`
}

// DaemonInfo is the daemon_info response payload.
type DaemonInfo struct {
	Version         string `json:"version"`
	ProtoVersion    string `json:"proto_version"`
	PID             int    `json:"pid"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	Socket          string `json:"socket"`
	Harnesses       int    `json:"harnesses"`
	ActiveProfile   string `json:"active_profile,omitempty"`
	ProfileResolved *bool  `json:"profile_resolved,omitempty"` // #99: false when persisted profile is missing from config
	// DormantAutostart lists harnesses an autostart profile asks for that
	// state.json restored disabled, so the daemon left them down. Empty/absent
	// is healthy. Clients surface it so `autostart = true` next to a harness
	// that never starts is not silent.
	DormantAutostart []string `json:"dormant_autostart,omitempty"`
	// SshAddr is the bind address of the running remote Wish SSH server
	// (ADR-0004), or empty when it is not running. SshKeys is the size of
	// its resolved public-key allowlist. Set by the daemon only when the
	// server actually started (config enabled or --ssh), so a client can
	// distinguish "off" from "enabled but refused to start" (empty
	// allowlist, ADR-0008).
	SshAddr string `json:"ssh_addr,omitempty"`
	SshKeys int    `json:"ssh_keys,omitempty"`
}

// ---- Structured errors (SPEC-0002 REQ "Control Operations") --------------

// ErrCode is a machine-readable error code the client can branch on; the
// human Message is safe to surface verbatim (SPEC-0002 REQ "Structured
// failure").
type ErrCode string

const (
	// ErrUnknownHarness: a control/attach request named a harness that does
	// not exist.
	ErrUnknownHarness ErrCode = "unknown_harness"
	// ErrUnknownProfile: use_profile named a profile that does not exist.
	ErrUnknownProfile ErrCode = "unknown_profile"
	// ErrVersionMismatch: HELLO proto major differed (REQ "Handshake And
	// Versioning").
	ErrVersionMismatch ErrCode = "version_mismatch"
	// ErrBadRequest: a malformed frame/payload.
	ErrBadRequest ErrCode = "bad_request"
	// ErrUnknownOp: an unrecognized control verb.
	ErrUnknownOp ErrCode = "unknown_op"
	// ErrInternal: the daemon failed to service an otherwise valid request.
	ErrInternal ErrCode = "internal"
	// ErrReload: a reload failed (config parse/validation); the daemon keeps
	// its last-good config (ADR-0006).
	ErrReload ErrCode = "reload_failed"
	// ErrNoSession: an attach frame referenced an unknown session id.
	ErrNoSession ErrCode = "no_session"

	// Project compose errors (SPEC-0004 REQ "Project Control Operations";
	// ADR-0009).

	// ErrProjectCollision: project_up named a project that would shadow an
	// existing bare global harness name; nothing was registered (SPEC-0004 REQ
	// "Project Naming And Namespacing").
	ErrProjectCollision ErrCode = "project_collision"
	// ErrUnknownProject: project_down named a project the daemon has no record
	// of; no state changed.
	ErrUnknownProject ErrCode = "unknown_project"
	// ErrInvalidProject: project_up carried an invalid project name or harness
	// definition; nothing was registered.
	ErrInvalidProject ErrCode = "invalid_project"
	// ErrNotRemovable: remove named a harness the daemon does not own outright
	// (a global-config harness, authored in harness.toml) or an unknown name;
	// no state changed (SPEC-0004 REQ "Remove").
	ErrNotRemovable ErrCode = "not_removable"

	// Scheduled-run errors (SPEC-0008 REQ "Protocol Operations").

	// ErrNotScheduled: trigger named a harness that exists but has no
	// schedule. Distinct from unknown_harness so a script can tell a typo from
	// pointing a job verb at a resident harness.
	ErrNotScheduled ErrCode = "not_scheduled"
	// ErrUnknownRun: a run selector named a run id the harness's history does
	// not hold — never run, or pruned past keep_runs.
	ErrUnknownRun ErrCode = "unknown_run"
)

// ErrorMsg is a structured error frame body. ID echoes the request it answers
// (0 for connection-level errors like a version mismatch).
type ErrorMsg struct {
	ID      uint64  `json:"id,omitempty"`
	Code    ErrCode `json:"code"`
	Message string  `json:"message"`
}

// Error implements error so daemon/client code can pass an ErrorMsg around.
func (e *ErrorMsg) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// ---- Events (SPEC-0002 REQ "Event Subscription") -------------------------

// EventKind names a pushed event. The first three map 1:1 to the supervisor's
// lifecycle events; config_reloaded and profile_changed are daemon-level.
type EventKind string

const (
	EvStateChanged  EventKind = "harness_state_changed"
	EvExited        EventKind = "harness_exited"
	EvFlapping      EventKind = "harness_flapping"
	EvConfigReload  EventKind = "config_reloaded"
	EvProfileChange EventKind = "profile_changed"

	// Scheduled-run events (SPEC-0008 REQ "Lifecycle Events"). A run that
	// starts a process emits job_run_started; every record that becomes final
	// emits job_run_finished, including a decision that started no process
	// (skipped, missed). job_schedule_changed carries a harness's new next
	// window whenever it moves: armed, re-armed by a reload, advanced past a
	// firing, or disarmed (empty NextRunAt).
	EvJobRunStarted      EventKind = "job_run_started"
	EvJobRunFinished     EventKind = "job_run_finished"
	EvJobScheduleChanged EventKind = "job_schedule_changed"
)

// EventMsg is a pushed EVENT frame body. Only the fields relevant to Kind are
// populated (mirrors supervisor.Event across the wire).
type EventMsg struct {
	Kind          EventKind `json:"kind"`
	Name          string    `json:"name,omitempty"`
	From          string    `json:"from,omitempty"`
	To            string    `json:"to,omitempty"`
	Code          int       `json:"code,omitempty"`
	Restarts      int       `json:"restarts,omitempty"`
	NextRetryInMs int64     `json:"next_retry_in_ms,omitempty"`
	Profile       string    `json:"profile,omitempty"`

	// Job run fields (job_run_started, job_run_finished). ExitCode is a
	// pointer so a clean exit 0 is distinguishable from "no process exited".
	RunID      int    `json:"run_id,omitempty"`
	Trigger    string `json:"trigger,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	// NextRunAt (RFC 3339) is job_schedule_changed's new next window; empty
	// when the harness no longer has one.
	NextRunAt string `json:"next_run_at,omitempty"`
}

// ---- Attach data plane (SPEC-0002 REQ "Attach Session") ------------------

// AttachMode is the attach access mode (ADR-0008 read-only attach).
type AttachMode string

const (
	// AttachRW is a read-write attach: keystrokes reach the PTY.
	AttachRW AttachMode = "rw"
	// AttachRO is a read-only attach: input is discarded, the PTY never sees
	// it (ADR-0008).
	AttachRO AttachMode = "ro"
)

// AttachOpen is the JSON body of an ATTACH_OPEN frame's payload (after the
// 4-byte session id prefix). The client picks the session id so it can run
// several attaches over one connection.
type AttachOpen struct {
	Name string     `json:"name"`
	Cols int        `json:"cols"`
	Rows int        `json:"rows"`
	Mode AttachMode `json:"mode"`
}

// AttachResize is the JSON body of an ATTACH_RESIZE frame's payload (after the
// session id prefix).
type AttachResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// --- attach payload framing helpers ---
//
// Attach frames are `session_id (uint32 BE) || rest`. For ATTACH_DATA the rest
// is raw terminal bytes; for ATTACH_OPEN/ATTACH_RESIZE it is JSON; for
// ATTACH_CLOSE it is empty.

// EncodeAttach prefixes rest with the 4-byte big-endian session id.
func EncodeAttach(sessionID uint32, rest []byte) []byte {
	out := make([]byte, 4+len(rest))
	binary.BigEndian.PutUint32(out[:4], sessionID)
	copy(out[4:], rest)
	return out
}

// DecodeAttach splits an attach payload into its session id and remaining
// bytes. The returned slice aliases payload.
func DecodeAttach(payload []byte) (sessionID uint32, rest []byte, err error) {
	if len(payload) < 4 {
		return 0, nil, fmt.Errorf("protocol: attach payload %d bytes, need >=4 for session id", len(payload))
	}
	return binary.BigEndian.Uint32(payload[:4]), payload[4:], nil
}

// ---- JSON convenience ----------------------------------------------------

// marshal is a panic-free JSON encode used by the typed writers below.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }

// WriteJSON marshals v and writes it as a single frame of type t.
func (c *Conn) WriteJSON(t Type, v any) error {
	b, err := marshal(v)
	if err != nil {
		return err
	}
	return c.WriteFrame(t, b)
}

// WriteError writes a structured ERROR frame.
func (c *Conn) WriteError(id uint64, code ErrCode, format string, args ...any) error {
	return c.WriteJSON(TypeError, &ErrorMsg{ID: id, Code: code, Message: fmt.Sprintf(format, args...)})
}
