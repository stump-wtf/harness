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
	ProtoMinor = 6
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
	Quiet          *bool  `json:"quiet,omitempty"`
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
	// Prompt is the agent one-shot instruction for a prompt harness; the
	// argv is synthesized at spawn from the same adapter (ADR-0011).
	Prompt string `json:"prompt,omitempty"`
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
