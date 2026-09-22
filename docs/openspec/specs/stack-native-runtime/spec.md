---
status: draft
date: 2026-09-22
implements: [ADR-0031]
extends: [SPEC-0018]
requires: [SPEC-0008, SPEC-0010, SPEC-0014]
---

# SPEC-0024: Native Runtime for the Stack Installer

## Overview

SPEC-0018 specifies `harness init` and `harness stack` over a Docker Compose
bundle. This spec adds a **second runtime** to `harness stack`: `native`, in
which Harness installs pinned Switchboard and Cairn **server binaries**, writes
a **launchd or `systemd --user` service unit** for each, and runs them against
**the operator's own Postgres and object store**, which it requires, verifies
and never installs.

Everything else is the same record. The manifest, the lock file, the version
verification, the end-to-end self-test, `upgrade`, `doctor` and the tenancy
rules of SPEC-0018 apply to both runtimes; this spec says how each is satisfied
when there is no container to inspect. Where a requirement here has no
qualifier, it **adds to** the SPEC-0018 requirement it names rather than
replacing it.

`harness init` is unchanged. It connects a client and its personas to an
instance over that instance's public APIs and does not observe how the
instance's processes were started.

**This is a companion spec, not an amendment.** The pull request that accepts
the Operation Stumply design set is still open against SPEC-0018, so this spec
is written beside it. Folding these requirements into SPEC-0018 once that pull
request merges is a tracked follow-up story on the stack-installer epic.

Terms are SPEC-0018's. Additionally: a **component** is one installable piece
of the stack (Switchboard, Cairn); a **service unit** is the launchd property
list or `systemd --user` unit that supervises one component; the **install
prefix** is the bundle-relative directory holding verified binaries; a
**datastore** is the operator's Postgres or object store.

See ADR-0031 for the decision, the options, and exactly which part of ADR-0024's
rejected option "2B" this adopts and which part it continues to reject.

## Requirements

### Requirement: REQ-1 Runtime Selection

`harness stack init` SHALL accept a runtime of `docker` (the default) or
`native`, from `--runtime`, the answers file key `runtime`, or an interactive
prompt. The chosen runtime SHALL be recorded in `stack.lock.json` and SHALL be
reported by `harness stack status`, including in `--json`. Every other `harness
stack` subcommand SHALL read the runtime from the lock and SHALL NOT accept a
`--runtime` flag. A bundle's runtime SHALL NOT be changed in place.

#### Scenario: Runtime recorded at init

- **WHEN** an operator runs `harness stack init --runtime native`
- **THEN** `stack.lock.json` records `"runtime": "native"`, and `stack status
  --json` reports the same value

#### Scenario: Changing runtime in place is refused

- **GIVEN** a bundle whose lock records `docker`
- **WHEN** the operator runs `harness stack up --runtime native`
- **THEN** the command fails with a usage error (exit `2`) saying a bundle's
  runtime is fixed at `init`, and naming `stack init --dir` with a new directory
  as the way to run the other runtime against the same datastores

#### Scenario: No mixed runtime

- **WHEN** `stack init --runtime native` runs on a host where the manifest has
  no native artifact for one component and this host's `os/arch`
- **THEN** the command fails, names that component and this `os/arch`, and does
  NOT start that component under Docker while starting the other natively

### Requirement: REQ-2 Native Platform Support

The native runtime SHALL support `darwin` (launchd) and `linux` (`systemd
--user`), on `amd64` and `arm64`. On any other operating system, or where the
host's service manager is absent or not usable by the invoking user, `harness
stack init --runtime native` SHALL fail before writing any file, naming the
platform and the missing service manager. The native runtime SHALL NOT fall
back to Docker, and SHALL NOT supervise the services itself.

#### Scenario: Windows

- **WHEN** `stack init --runtime native` runs on Windows
- **THEN** it exits `2`, says the native runtime supports macOS and Linux only,
  and writes nothing

#### Scenario: Linux without a user systemd instance

- **WHEN** `systemctl --user` is not usable for the invoking user
- **THEN** `stack init --runtime native` fails, names `systemd --user`, and
  points at `loginctl enable-linger` in the documentation

### Requirement: REQ-3 Native Manifest Entries

The embedded manifest of SPEC-0018 REQ-16 SHALL carry, for each native
component and each supported `os/arch`: an artifact URL, the archive's SHA-256,
the path of the binary inside the archive, and a minimum version; and MAY carry
a signature URL and a verification key identifier. The build SHALL fail when a
native entry's artifact URL is absent, is not an immutable release URL, or has
no SHA-256; when a signature URL is present without a key identifier; or when
a component declared native-capable is missing an entry for a supported
`os/arch`. `stack.lock.json` SHALL record, per installed component, the
manifest version, the resolved version, the artifact URL and the SHA-256
actually verified.

#### Scenario: A native entry without a digest

- **WHEN** a manifest change adds a Switchboard native artifact with no SHA-256
- **THEN** `make test` fails, naming the component and the `os/arch`

#### Scenario: A moving artifact reference

- **WHEN** a native artifact URL resolves to a `latest` release rather than a
  tagged one
- **THEN** `make test` fails, naming the component

#### Scenario: A signature promise Harness cannot keep

- **WHEN** a manifest entry declares a signature URL and no verification key
  identifier
- **THEN** `make test` fails, naming the component

### Requirement: REQ-4 Binary Fetch And Verification

`harness stack up` and `harness stack upgrade` SHALL, for each native
component: download the manifest's artifact for this `os/arch` into a temporary
directory inside the bundle; compute its SHA-256 and compare it to the
manifest's; verify its signature when the manifest carries one; extract the
named binary; and install it atomically at
`bin/<component>/<version>/<binary>` with mode `0755`. The digest SHALL be
compared against the manifest, and SHALL NOT be taken from a checksum file
fetched from the artifact's origin. On any mismatch or verification failure the
command SHALL delete the download, install nothing, leave any previously
installed version in place, and exit non-zero naming the component, the URL,
the expected digest and the computed one. An already-installed version whose
digest matches the lock SHALL NOT be re-downloaded. `harness stack` SHALL NOT
accept an artifact override that carries no SHA-256.

#### Scenario: Digest mismatch

- **WHEN** the downloaded Switchboard archive's SHA-256 differs from the
  manifest's
- **THEN** nothing is installed, the previous binary is untouched, no service is
  restarted, and the error names Switchboard, both digests and the URL

#### Scenario: No re-download of a verified binary

- **GIVEN** `bin/switchboard/v0.3.1/switchboard` whose digest matches the lock
- **WHEN** `stack up` runs again
- **THEN** no artifact is fetched for Switchboard and the command reports it as
  already installed

#### Scenario: An override without a digest

- **WHEN** an operator runs `stack up --artifact cairnd=https://example.invalid/cairnd.tar.gz`
- **THEN** the command fails and asks for the `sha256:` digest

#### Scenario: Signature verification once signatures exist

- **GIVEN** a manifest entry carrying a signature URL and key identifier
- **WHEN** the signature does not verify
- **THEN** the command fails for that component even though the SHA-256 matched

### Requirement: REQ-5 Service Unit Generation

`harness stack init --runtime native` SHALL write, per component, one service
unit into the bundle: a launchd property list on macOS, a `systemd --user` unit
on Linux. Each unit SHALL run the installed binary by its versioned path, set
its working directory inside the bundle, restart the service on failure but not
after a clean stop, route stdout and stderr to a bundle log path, and load its
environment only by reference to files under `secrets/`. A unit SHALL contain
no secret value and no datastore connection string. Units SHALL be user-scoped
(`gui/<uid>` on macOS, `systemd --user` on Linux); no subcommand SHALL write a
system-wide unit, require `sudo`, or create an operating-system user. `stack
init` SHALL NOT load or start a unit.

#### Scenario: No secret in a unit

- **WHEN** `stack init --runtime native` has run
- **THEN** a scan of every generated unit file, and of `stack.lock.json`, for
  every generated secret value and for the datastore connection strings finds
  none

#### Scenario: Units are user-scoped

- **WHEN** `stack init --runtime native` has run
- **THEN** no file has been written under `/Library/LaunchDaemons`,
  `/etc/systemd/system` or any other path requiring elevated privileges, and no
  subcommand has invoked `sudo`

#### Scenario: A hand-edited unit

- **GIVEN** an operator has edited a generated unit
- **WHEN** `stack init` or `stack upgrade` would rewrite it
- **THEN** it shows a diff and writes only after confirmation (or `--yes`)

#### Scenario: A clean stop is not restarted

- **WHEN** `harness stack down` stops Switchboard
- **THEN** the service manager does not restart it

### Requirement: REQ-6 External Postgres Required And Verified

`harness stack init --runtime native` SHALL require one Postgres connection URL
per service, supplied by flag, answers file or prompt, and SHALL verify, before
writing any bundle file, that: each URL connects; the server version is at
least the manifest's minimum; the two URLs name **different databases** and
authenticate as **different roles**; each role can create objects in its own
database; and neither role can connect to or read the other's database. It
SHALL install, configure, start, stop, restart or upgrade no database, on any
platform, under any flag. When a check fails it SHALL print the `CREATE ROLE`,
`CREATE DATABASE`, `GRANT` and `REVOKE` statements that would satisfy it, and
SHALL execute none of them.

#### Scenario: A shared role

- **WHEN** both connection URLs authenticate as the same role
- **THEN** `stack init` fails, says one role per service is required, prints the
  SQL, and writes nothing

#### Scenario: A shared database

- **WHEN** both connection URLs name the same database
- **THEN** `stack init` fails for the same reason, and there is no flag that
  permits it

#### Scenario: Cross-database access

- **GIVEN** two distinct roles and databases where Cairn's role can also read
  Switchboard's database
- **THEN** `stack init` fails the isolation check, names both, and prints the
  `REVOKE`

#### Scenario: Harness does not install Postgres

- **WHEN** no Postgres is reachable at the supplied URL
- **THEN** `stack init` fails saying the native runtime requires an existing
  Postgres, and offers no option to install or start one

#### Scenario: Server below the minimum

- **WHEN** the server's version is below the manifest's minimum
- **THEN** `stack init` fails, naming both versions, and changes nothing

### Requirement: REQ-7 External Object Store Required And Verified

`harness stack init --runtime native` SHALL require S3-compatible object-store
details (endpoint, region, bucket, access key, secret key) and SHALL verify them
with a round trip: put an object carrying a fresh random nonce under a
Harness-owned key prefix, read it back, compare the body, and delete it. It
SHALL NOT create a bucket unless `--create-bucket` is given, and SHALL NOT
install or start an object store. A native bundle including Cairn without
object-store details SHALL be refused.

#### Scenario: Round-trip proof, not a reachable endpoint

- **WHEN** the endpoint answers but the credentials cannot write the bucket
- **THEN** `stack init` fails at the put, names the bucket and the operation,
  and does not report the object store as configured

#### Scenario: A missing bucket

- **WHEN** the bucket does not exist and `--create-bucket` was not given
- **THEN** `stack init` fails, names the bucket, and creates nothing

#### Scenario: Cairn without an object store

- **WHEN** `stack init --runtime native` includes Cairn and no object-store
  answers
- **THEN** it exits `2` and names the missing answers

### Requirement: REQ-8 Native Stack Up And Down

`harness stack up` under the native runtime SHALL: verify the datastores are
reachable; fetch and verify each component's binary (REQ-4); write and load each
service unit; start each service; wait for its health check up to `--timeout`
(default 5 minutes); verify that each installed binary's SHA-256 equals the
lock and that each service's reported version equals the manifest's; and then
run `harness init --connect` (skippable with `--no-init`) and the self-test
(REQ-9, skippable with `--no-selftest`). `harness stack down` SHALL stop and
unload the services and SHALL keep the binaries, units, secrets and all
datastore contents. `--volumes` SHALL be refused under the native runtime.
Failures SHALL exit non-zero naming the failed step and component.

#### Scenario: Binary drift

- **GIVEN** an installed binary whose SHA-256 differs from the lock
- **WHEN** `stack up` verifies the stack
- **THEN** it fails, names the component, both digests, and suggests `stack
  upgrade` or a re-install

#### Scenario: A service never becomes healthy

- **WHEN** Cairn's health check does not pass within the timeout
- **THEN** `stack up` exits `1`, names Cairn, prints the last 50 lines of its
  service log with any secret-looking value masked, and does not run `init`

#### Scenario: Data kept by default

- **WHEN** an operator runs `stack down` and then `stack up`
- **THEN** every endpoint, todo and artifact created before is still present

#### Scenario: `--volumes` is refused

- **WHEN** an operator runs `stack down --volumes` on a native bundle
- **THEN** the command fails, says Harness will not destroy a datastore it did
  not create, names the Postgres host and database and the bucket, and deletes
  nothing

### Requirement: REQ-9 Self-Test Parity

The end-to-end self-test of SPEC-0018 REQ-25 SHALL run unchanged under the
native runtime: the same steps, the same nonce, the same read-back of the todo
as the endpoint and of the artifact from Cairn, and the same content comparison.
No step SHALL be skipped, weakened or reported `pass` on a health check, a
delivery log or a zero exit. The report SHALL name the runtime. A native `stack
up` whose self-test fails SHALL exit non-zero.

#### Scenario: The whole loop, natively

- **WHEN** the self-test runs on a working native stack
- **THEN** every step reports `pass`, and the report names the runtime, the todo
  ID, the run ID and the artifact handle

#### Scenario: A forged artifact, natively

- **WHEN** the handle in the todo's result names an artifact whose body lacks
  the nonce
- **THEN** step 6 fails, even though the todo is `done` and the run exited 0

#### Scenario: No weaker pass on the native path

- **WHEN** every service's health check passes but no run starts within the
  timeout
- **THEN** the self-test fails and `stack up` exits non-zero

### Requirement: REQ-10 Native Upgrade And Rollback

`harness stack upgrade` under the native runtime SHALL move the bundle to the
manifest embedded in the running binary. It SHALL print the plan and the upgrade
notes and ask for confirmation (or `--yes`); fetch and verify every new binary
(REQ-4) **before** stopping any service; write a `pg_dump` of each service's
database to `$XDG_STATE_HOME/harness/stack/backups/<timestamp>/` with mode
`0600` before that service starts at a new version; upgrade one service at a
time, waiting for its health check; and then re-run the verification of REQ-8
and the self-test. It SHALL keep the previously installed version on disk. A
step the manifest marks breaking SHALL require `--accept-breaking`. A target
older than the lock SHALL be refused. When a service fails its health check
after the change, `upgrade` SHALL repoint that service's unit at the previous
installed version, restart it, stop, and report the backup path. `upgrade`
SHALL NOT upgrade, restart or migrate the operator's Postgres or object store;
when the manifest's minimum Postgres version rises above the running server, it
SHALL refuse and name both versions.

#### Scenario: Verify before stopping anything

- **WHEN** the new Cairn artifact fails its digest check
- **THEN** no service was stopped, no unit was rewritten, and the previous
  versions are still running

#### Scenario: A failed service upgrade rolls back

- **WHEN** Switchboard fails its health check after the binary change
- **THEN** its unit points at the previous installed version and that version is
  running, the backup path is printed, and later services were not upgraded

#### Scenario: A breaking step

- **GIVEN** the target Switchboard version drops a configuration the bundle uses
- **WHEN** `stack upgrade` runs without `--accept-breaking`
- **THEN** it prints the note, changes nothing, and exits `3`

#### Scenario: A Postgres major version is the operator's

- **WHEN** the target manifest's minimum Postgres version is above the running
  server
- **THEN** `upgrade` refuses, names both versions, and does not upgrade, dump-
  and-restore, or restart the operator's server

### Requirement: REQ-11 Native Doctor

`harness stack doctor` under the native runtime SHALL check: the host (the
service manager present and usable by this user, a writable install prefix,
ports free, disk space, and `pg_dump` present and not older than the server);
the bundle (secret file modes, unit files present, unloaded or drifted from
what `stack init` would write, installed binary digests versus the lock,
reported versions versus the manifest); the datastores (both connections live,
server version, the REQ-6 isolation checks still holding, and the REQ-7 object
round trip); reverse-proxy streaming where the operator has declared one; and
the connected personas (each credential still authenticates). It SHALL print
each check as `ok`, `warn` or `fail` with a one-line next step, and SHALL exit
`0` when no check fails, `1` otherwise. `--e2e` SHALL add the self-test. It
SHALL print no secret and no connection string.

#### Scenario: Isolation that rotted after install

- **GIVEN** the operator later granted Cairn's role access to Switchboard's
  database
- **WHEN** `stack doctor` runs
- **THEN** the isolation check fails, names both roles and databases, and prints
  the `REVOKE`

#### Scenario: A unit that is no longer loaded

- **WHEN** Switchboard's unit exists but the service manager has it unloaded
- **THEN** doctor fails that check, names the unit and the load command

#### Scenario: Doctor never prints a connection string

- **WHEN** `stack doctor` reports on the datastores
- **THEN** the output names hosts, databases and buckets, and contains no
  password and no full connection URL

### Requirement: REQ-12 Command And Reporting Parity

Every `harness stack` subcommand SHALL exist under both runtimes. Where a
subcommand or flag cannot be satisfied natively, it SHALL fail with a
runtime-specific error naming the reason, and SHALL NOT silently succeed or
no-op. `harness stack status` SHALL report, per service: its state, the
installed binary's digest, the lock's digest, the reported version, and its URL;
per generated secret: its file and fingerprint; per datastore: host, database
or bucket, and server version, with no credential; and the result and time of
the last self-test. `--json` SHALL emit the same field names in both runtimes,
plus `runtime`, as a stable document.

#### Scenario: One JSON shape

- **WHEN** `stack status --json` is run on a Docker bundle and on a native
  bundle
- **THEN** both documents carry the same field names, differing only in
  `runtime` and in the per-service identity fields' values

#### Scenario: No silent no-op

- **WHEN** an operator runs a Compose-only flag against a native bundle
- **THEN** the command fails with an error naming the runtime and the flag,
  rather than ignoring it

#### Scenario: Status never prints a secret

- **WHEN** `stack status --json` runs on a native bundle
- **THEN** the output contains fingerprints, host names and database names, and
  no secret value and no connection URL

### Requirement: REQ-13 Documentation And Path Selection

The documentation SHALL present Docker and native as two supported runtimes,
and SHALL contain a page stating which suits which operator: what each requires
of the host, what Harness manages and what the operator manages in each, and
what each does not do. The native page SHALL state that Harness installs no
database and no object store, that services are supervised by the operator's
service manager rather than by Harness, that the runtime is fixed at `init`, and
that Windows is unsupported. Neither runtime SHALL be described as a fallback,
a workaround or an unsupported path.

#### Scenario: A reader choosing a runtime

- **WHEN** a reader opens the stack installation documentation
- **THEN** both runtimes are listed with their host requirements, and the native
  page names Postgres and an object store as operator-supplied prerequisites

### Requirement: REQ-14 Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (for example, "fetch cairnd v0.4.0 darwin/arm64: verify digest: mismatch").
- Sentinel errors MUST be defined for the failure modes callers distinguish:
  unsupported platform, missing native artifact for this `os/arch`, digest
  mismatch, signature failure, service-manager unavailable, unit load failure,
  datastore unreachable, datastore version too old, shared role or database, and
  a refused runtime change.
- Silent error swallowing MUST NOT occur. A failure to remove a superseded
  binary after an upgrade is a reported warning, not a dropped error.
- Structured logging MUST be used, and no log entry may carry a secret value or
  a datastore connection string.

#### Scenario: A located failure

- **WHEN** a binary's digest does not match
- **THEN** the error names the component, the version, the `os/arch`, the URL
  and both digests, and carries no credential

## Security Requirements

The native runtime downloads and executes binaries on the operator's host with
the operator's own privileges, writes files a service manager reads, and holds
credentials for two datastores it did not provision. It serves no public route
of its own.

### Authentication

Every call `harness init` makes to Switchboard and Cairn SHALL carry the user's
own credential, exactly as SPEC-0018 requires; the runtime changes nothing
about it. Datastore credentials SHALL be used only by the services themselves
and by the validation and `pg_dump` steps of this spec, and SHALL NOT be passed
to any agent, written to any client configuration, or included in the discovery
document.

### Binary Provenance

Every downloaded artifact SHALL be verified against a SHA-256 carried in the
embedded manifest before extraction, and against a signature where the manifest
declares one. A verification failure SHALL install nothing and retain nothing.
Artifacts SHALL be fetched over HTTPS, and the client SHALL NOT follow a
redirect to a different host than the manifest's URL.

### Rate Limiting

Artifact downloads and datastore validation SHALL be performed serially per
component, and a `429` or `503` carrying `Retry-After` from an artifact host
SHALL be honoured by waiting, up to the command's timeout, rather than retried
immediately.

### Security Headers

Not applicable: the native runtime serves no HTTP route. Where the operator
declares a reverse proxy, `stack doctor` SHALL check that Switchboard's `/mcp/*`
routes are not response-buffered, because a buffered proxy silently converts a
doorbell into a message delivered at connection close.

### Request Body Size Limits

Responses from Switchboard, Cairn and the object store SHALL be read with a
1 MiB cap, except artifact downloads, which SHALL be capped at the size the
manifest declares and SHALL abort when the stream exceeds it.

### CSRF Protection

Not applicable: the native runtime opens no listener. `harness init`'s loopback
OAuth listener and its protections are unchanged by this spec.

### Redirect Validation

HTTP clients SHALL NOT follow a redirect to a different scheme or host than the
configured instance URL or the manifest's artifact host, so neither a bearer
token nor a download can be moved to another origin.

### File Permissions And Least Privilege

Installed binaries SHALL be `0755` under a directory the invoking user owns.
Secret files SHALL be `0600` in a `0700` directory. Unit files SHALL contain no
secret. No subcommand SHALL write a system-wide service unit, invoke `sudo`,
create an operating-system user, or run a service as `root`.

### Tenancy

SPEC-0018 REQ-27 applies unchanged. The native runtime SHALL NOT create an
instance-global user resource, SHALL NOT generate any credential that can act
for users in Switchboard or Cairn, and SHALL NOT write to either product's
database directly — including the operator's Postgres, which it reads from only
to verify. Neither product's development login SHALL be enabled under any flag,
and the identity-provider and enrollment-mode requirements of SPEC-0018 REQ-18
apply to a native bundle exactly as to a Compose one.
