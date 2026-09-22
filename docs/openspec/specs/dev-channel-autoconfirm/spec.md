---
status: approved
date: 2026-09-22
implements: [ADR-0029]
extends: [SPEC-0003, SPEC-0006]
requires: [SPEC-0002, SPEC-0013]
---

# SPEC-0023: Auto-Confirm Claude Code Development Channels

## Overview

A resident `claude-code` harness that loads a custom channel with
`--dangerously-load-development-channels` stops at a full-screen confirmation
dialog on every start. This spec lets an operator opt in, per named entry, to
Harness answering that dialog. The Claude Code adapter watches the harness's
emulated screen during startup. It sends the confirmation keystroke only when
the dialog matches a known signature and lists exactly the configured entries.
Every other case produces no keystroke, an error, and a harness marked as
needing attention. Every firing logs a WARN carrying the Claude Code version,
and `harness doctor` warns for as long as the opt-in is configured.

See ADR-0029 for the decision, and for the Team/Enterprise and ADR-0021
alternatives it prefers where they apply. This spec extends SPEC-0003 (an
attention flag beside the lifecycle state) and SPEC-0006 (an adapter-scoped
detector). It requires SPEC-0002 (protocol fields and events) and SPEC-0013
(metrics).

Requirements are numbered. Cite them as `SPEC-0023 REQ-n`.

## Requirements

### Requirement: REQ-1 — The Accept Key

A harness MAY declare `accept_dev_channels`, a non-empty array of strings. Each
entry SHALL match `^server:[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$` or
`^plugin:[A-Za-z0-9][A-Za-z0-9_.-]{0,63}@[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`, and
entries SHALL be unique. The key SHALL fail config validation, with a located
error naming the harness, when:

- the harness is not `harness = "claude-code"`;
- the harness is a one-shot (it has a prompt source, `schedule` or `triggers`);
- it appears in a project `harness.toml`, or on the project-up or scratchpad
  wire;
- its set of entries differs from the set REQ-2 extracts from `args`. The error
  SHALL name each missing or extra entry.

The key SHALL be accepted in the global `harness.toml` and in `harness_d`
drop-ins.

#### Scenario: A valid opt-in

- **WHEN** a global claude-code harness declares
  `args = ["--dangerously-load-development-channels", "server:switchboard"]`
  and `accept_dev_channels = ["server:switchboard"]`
- **THEN** the config loads

#### Scenario: An entry not in args

- **WHEN** `accept_dev_channels = ["server:switchboard"]` and `args` loads no
  development channel
- **THEN** config validation fails, naming `server:switchboard` as not loaded

#### Scenario: An args entry not accepted

- **WHEN** `args` loads `server:switchboard server:other` and
  `accept_dev_channels = ["server:switchboard"]`
- **THEN** config validation fails, naming `server:other` and stating that the
  dialog would never be confirmed

#### Scenario: Project files cannot opt in

- **WHEN** a project `harness.toml` declares `accept_dev_channels`
- **THEN** `harness up` fails, naming the key as global-only

#### Scenario: Wrong kind

- **WHEN** a `crush` or `command` harness declares `accept_dev_channels`
- **THEN** config validation fails

### Requirement: REQ-2 — Entries From Args

The development-channel entries a harness loads SHALL be extracted from its
`args` as the union of:

- every argument after an `--dangerously-load-development-channels` argument,
  up to the next argument beginning with `-` or the end of `args`;
- the value of every `--dangerously-load-development-channels=<value>`
  argument, split on whitespace.

A repeated flag SHALL add to the union. An extracted entry that fails REQ-1's
patterns SHALL fail validation when `accept_dev_channels` is set.

#### Scenario: Several entries after one flag

- **WHEN** `args = ["--dangerously-load-development-channels", "server:a",
  "plugin:b@m", "--allowedTools", "x"]`
- **THEN** the extracted set is `{server:a, plugin:b@m}`

#### Scenario: The equals form

- **WHEN** `args = ["--dangerously-load-development-channels=server:a server:b"]`
- **THEN** the extracted set is `{server:a, server:b}`

### Requirement: REQ-3 — Watch Window

For a harness with `accept_dev_channels`, the adapter's detector SHALL start at
each spawn. It SHALL stop at the first of these:

- a confirmation verified under REQ-6;
- the channel-registration notice appearing on screen and naming every
  configured entry;
- 90 seconds after spawn;
- process exit.

The detector SHALL NOT run at any other time, and SHALL NOT act after it stops.

#### Scenario: Stops after registration

- **WHEN** the registration notice naming `server:switchboard` appears
- **THEN** the detector stops, and nothing typed later by the session or the
  operator is inspected

#### Scenario: Window expires

- **WHEN** 90 seconds pass with neither a dialog nor a registration notice
- **THEN** the detector stops, and REQ-9 applies with reason
  `no_registration_notice`

### Requirement: REQ-4 — Screen Signatures

The detector SHALL read the harness's rendered screen from the daemon's
emulator (the screen `harness attach` repaints). It SHALL NOT match raw PTY
bytes. The binary SHALL carry a table of **signatures**. Each signature SHALL
declare:

| Field | Meaning |
| --- | --- |
| `id` | A stable identifier, logged on every use |
| `captured_from` | The Claude Code versions its fixture was recorded from |
| `required` | Phrases that must all appear on screen (at least the dialog title phrase, the accept option label and the exit option label) |
| `entries` | The rule that extracts the listed entries from the screen |
| `accept_selected` | How to tell that the accept option is the current selection, or `keys_select` when the key sequence selects it by an absolute choice |
| `keys` | The exact bytes to write |

A screen **matches** a signature only when every `required` phrase is present
and the `entries` rule extracts a non-empty list. Matching SHALL be exact on
phrases after whitespace runs are collapsed. It SHALL NOT be fuzzy, and SHALL
NOT be case-insensitive.

#### Scenario: The current dialog matches

- **WHEN** the recorded fixture of the current dialog, listing
  `server:switchboard`, is replayed into the emulator
- **THEN** it matches a signature, and the extracted entries are
  `[server:switchboard]`

#### Scenario: Altered wording does not match

- **WHEN** the fixture's accept option reads differently from every signature's
  label
- **THEN** no signature matches

### Requirement: REQ-5 — Confirm Conditions

The detector SHALL send the confirmation only when all of these hold at the
same screen read:

1. The screen matches a signature (REQ-4).
2. The extracted entries, as a set, equal `accept_dev_channels`.
3. The signature's `accept_selected` test passes.
4. No **excluded dialog** is on screen (REQ-7).
5. No attached client holds input capability (REQ-8).

Two consecutive screen reads at least 250 ms apart SHALL satisfy conditions 1
to 4 before sending, so that a partially drawn frame is never acted on.

#### Scenario: Exact match confirms

- **WHEN** the dialog lists exactly `server:switchboard`, the accept option is
  selected, nothing else is on screen, and no client is attached
- **THEN** the detector sends the signature's keys once

#### Scenario: An extra entry blocks

- **WHEN** the dialog lists `server:switchboard` and `server:evil`
- **THEN** nothing is sent, and REQ-9 applies with reason `entry_mismatch`

#### Scenario: A partial frame

- **WHEN** only the dialog's title has been drawn at the first read
- **THEN** nothing is sent until two complete, matching reads occur

### Requirement: REQ-6 — Sending And Verifying

The detector SHALL write the signature's `keys` exactly once per spawn, through
the supervisor's input path (the path attach keystrokes use). It SHALL then
check, within 10 seconds, that no signature matches the screen any more. If a
signature still matches, it SHALL NOT write again, and REQ-9 SHALL apply with
reason `confirm_failed`.

#### Scenario: One write only

- **WHEN** the dialog is still on screen 10 seconds after the confirmation
- **THEN** no second write occurs, and the harness is marked for attention
  with `confirm_failed`

### Requirement: REQ-7 — Excluded Dialogs

The detector SHALL never send any input while any of these is on screen, and
SHALL treat each as REQ-9 with the reason shown:

| Dialog | Reason |
| --- | --- |
| MCP server consent for a project-scoped server ("New MCP server found in this project") | `other_dialog` |
| Workspace or folder trust | `other_dialog` |
| A tool permission prompt | `other_dialog` |
| An organization-policy block of channels ("blocked by org policy", or the startup warning that channels are disabled for the organization) | `org_policy_blocked` |

The phrases for these SHALL live in the same versioned table as the signatures,
each with a fixture.

#### Scenario: MCP consent is never answered

- **WHEN** the MCP server consent dialog is on screen during the window
- **THEN** nothing is sent, and the harness is marked for attention with
  `other_dialog`

#### Scenario: Org policy is never bypassed

- **WHEN** the screen shows that channels are blocked by organization policy
- **THEN** nothing is sent, an ERROR is logged with `org_policy_blocked`, and
  the harness is marked for attention

### Requirement: REQ-8 — A Present Human Answers

If any client is attached to the harness with input capability (not read-only)
when the dialog appears, or at any point before the keys would be written, the
detector SHALL NOT send and SHALL NOT mark the harness for attention. It SHALL
keep watching until its window ends. If the registration notice then appears,
the detector stops. If the window ends with the dialog still on screen, REQ-9
applies with reason `no_registration_notice`.

#### Scenario: Attached operator

- **WHEN** an operator is attached read-write as the dialog appears, and
  answers it
- **THEN** Harness sends nothing, logs no confirmation, and sets no attention
  flag

#### Scenario: A read-only viewer does not block

- **WHEN** only a read-only client is attached
- **THEN** the detector proceeds as if none were attached

### Requirement: REQ-9 — Unrecognized Or Unsafe Prompts

When a development-channels dialog is on screen that REQ-5 does not permit
confirming, or when REQ-3, REQ-6 or REQ-7 name a reason, the detector SHALL
send nothing. It SHALL log one ERROR line naming the harness, the Claude Code
version (REQ-12) and the reason: `unrecognized_prompt`, `entry_mismatch`,
`no_registration_notice`, `other_dialog`, `org_policy_blocked` or
`confirm_failed`. It SHALL set the harness's attention flag (REQ-10). A screen
counts as a development-channels dialog without matching a signature when it
contains `--dangerously-load-development-channels` or the phrase
`development channel`, in any case. That case is reported as
`unrecognized_prompt`.

#### Scenario: A Claude Code release changes the dialog

- **WHEN** a new Claude Code version shows the dialog with a changed title
- **THEN** nothing is sent, an ERROR names the version and
  `unrecognized_prompt`, and `harness list` shows the harness needs attention

### Requirement: REQ-10 — Attention Flag

A harness SHALL carry an optional attention flag: `{kind: "dev_channels",
reason, since, claude_code_version}`. It is independent of its SPEC-0003 state.
The flag SHALL be:

- shown in the `harness list` STATE cell without adding a column;
- shown in `harness describe`, with the reason and a hint to run
  `harness attach <name>`;
- a `fail` row in `harness doctor`;
- carried on the harness projection in the protocol, with an
  `attention_changed` event, bumping the protocol minor version;
- visible in the TUI dashboard.

The flag SHALL clear when the registration notice naming every configured
entry appears, or when the harness next spawns. It SHALL NOT persist across a
daemon restart, because the next spawn re-evaluates it.

#### Scenario: Cleared by attaching

- **WHEN** a harness flagged `unrecognized_prompt` is attached, the operator
  confirms by hand, and the registration notice appears
- **THEN** the flag clears, and an `attention_changed` event is emitted

#### Scenario: Visible in list

- **WHEN** a harness is flagged
- **THEN** `harness list` still has six columns, and the STATE cell marks it as
  needing attention

### Requirement: REQ-11 — Every Firing Is Logged And Recorded

Each confirmation sent SHALL produce exactly one WARN log line in the daemon log
and the harness's durable log. It SHALL carry the fields `harness`,
`event=dev_channels_autoconfirmed`, `claude_code_version`, `entries` and
`signature`. Its message SHALL state that a Claude Code development-channel
confirmation was answered automatically. The daemon SHALL persist, per harness,
the count of confirmations and the last one (`at`, `claude_code_version`,
`entries`, `signature`) in the state file. It SHALL emit a
`dev_channels_confirmed` event carrying the same fields.

#### Scenario: The WARN line

- **WHEN** the detector confirms for `claude-sb` under Claude Code 2.4.1
- **THEN** exactly one WARN line carries `harness=claude-sb`,
  `claude_code_version=2.4.1`, `entries=[server:switchboard]` and the signature
  ID

#### Scenario: Survives a restart

- **WHEN** the daemon restarts after three confirmations
- **THEN** `harness describe claude-sb` still shows a count of 3 and the last
  confirmation

### Requirement: REQ-12 — Claude Code Version

At spawn of a harness with `accept_dev_channels`, the daemon SHALL determine
the Claude Code version by running the harness's resolved `claude` executable
with `--version`, using the harness's environment, with a 5-second timeout. It
SHALL cache the result per executable path, size and modification time. A
failure or timeout SHALL record the version as `unknown`, SHALL NOT block the
spawn, and SHALL NOT change matching. Matching is by screen text, never by
version.

#### Scenario: Version unavailable

- **WHEN** `claude --version` times out
- **THEN** the harness spawns normally, and any log line records
  `claude_code_version=unknown`

### Requirement: REQ-13 — Doctor Warning

While any harness has `accept_dev_channels` set, `harness doctor` SHALL show a
`warn` row, whether or not a confirmation has fired. The row names each such
harness, its entries, its confirmation count, and the time and Claude Code
version of its last confirmation. The row's hint SHALL state that the
confirmation bypasses Anthropic's channel allowlist prompt and not the
organization's `channelsEnabled` policy, and SHALL link the docs callout
(REQ-15). `--json` SHALL carry the row.

#### Scenario: Standing warning

- **WHEN** one harness opts in and has never fired
- **THEN** `harness doctor` shows the warn row with a count of 0

### Requirement: REQ-14 — Metrics

The daemon SHALL expose, under SPEC-0013's registry and cardinality cap:

```
harness_dev_channels_autoconfirm_total{harness,outcome}   counter
```

`outcome` is `confirmed`, `unrecognized_prompt`, `entry_mismatch`,
`no_registration_notice`, `other_dialog`, `org_policy_blocked` or
`confirm_failed`. Only harnesses with `accept_dev_channels` SHALL have the
series.

#### Scenario: Counting outcomes

- **WHEN** one spawn confirms and the next reports `unrecognized_prompt`
- **THEN** the counter reads 1 for each of those two outcomes

### Requirement: REQ-15 — Documentation Callout

The configuration reference entry for `accept_dev_channels`, and the
push-events guide's Claude Code section, SHALL each carry a danger-level
admonition. It SHALL state:

- that the key makes Harness answer Anthropic's development-channel
  confirmation automatically, for the named entries, on every start, and log a
  WARN each time;
- that it does not bypass the organization's `channelsEnabled` policy,
  `allowedChannelPlugins` for `--channels`, MCP server consent, workspace
  trust, or tool permission prompts;
- that a channel puts text in front of the model, so senders must be gated and
  the session kept least-privileged;
- the Team/Enterprise alternative: package the channel as a plugin, have an
  admin add it to `allowedChannelPlugins`, and use
  `--channels plugin:<name>@<marketplace>`;
- the ADR-0021 alternative for event-driven work: daemon-held channels and
  one-shots, which need no development flag.

#### Scenario: The callout is present

- **WHEN** the docs site is built
- **THEN** both pages render a danger admonition naming `channelsEnabled`,
  `allowedChannelPlugins` and `--channels plugin:`

### Requirement: REQ-16 — Signature Maintenance

Every signature and excluded-dialog phrase set SHALL have at least one recorded
screen fixture (the raw PTY byte stream captured from a real Claude Code, with
its version) in the repository. A test SHALL replay every fixture through the
emulator and the detector and assert the expected outcome. Adding a signature
SHALL require a fixture. No signature SHALL be removed while its
`captured_from` versions are still current Claude Code releases.

#### Scenario: A fixture per signature

- **WHEN** the test suite runs
- **THEN** every signature's fixture produces exactly one confirmation, every
  excluded-dialog fixture produces none, and a signature without a fixture fails
  the test

### Requirement: Error Handling Standards

All error-producing operations in this spec SHALL follow structured error
handling:

- Errors SHALL be wrapped with the harness name, and config errors located by
  file and line.
- Sentinel errors SHALL be defined for the REQ-9 reasons, so tests and callers
  can distinguish them.
- A detector failure (an emulator read error, or an input write error) MUST NOT
  be swallowed. It SHALL be logged, and SHALL mark the harness for attention
  with `confirm_failed`.
- Logging SHALL be structured key-value, and SHALL never include screen content
  beyond the extracted entry names.

#### Scenario: Write failure

- **WHEN** writing the keys to the PTY fails
- **THEN** an ERROR is logged, the attention flag is set with
  `confirm_failed`, and no retry occurs

### Requirement: Concurrency Safety

The detector SHALL run as one goroutine per spawn, bound to that spawn's
context, and SHALL be cancelled on exit, stop or restart. It SHALL read the
emulator under the same synchronization the attach snapshot uses, and SHALL
send input only through the supervisor's actor loop. It SHALL check the
attached-client condition (REQ-8) within the same actor command that performs
the write, so an attach racing the write is decided in one place. Tests SHALL
run with the race detector.

#### Scenario: Attach races the write

- **WHEN** a read-write client attaches at the instant the detector decides to
  confirm
- **THEN** the actor loop sees the attached client and drops the write, and
  REQ-8 applies
