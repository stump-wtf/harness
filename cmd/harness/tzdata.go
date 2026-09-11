package main

// Embedded Time Zone Database
//
// A schedule may pin its zone with a CRON_TZ=/TZ= prefix (SPEC-0008 REQ
// "Schedule Time Zone"), and config validation resolves that zone with
// time.LoadLocation — in the CLI and in the daemon alike. Without this import
// the lookup depends on the host's zoneinfo, so a harness.toml that loads on a
// laptop fails to load in a minimal container image with no
// /usr/share/zoneinfo. Embedding the database (~450 KB) makes the same file
// mean the same schedule on every host; the host's database is still used
// whenever it is present.
//
// Governing: ADR-0013, SPEC-0008 REQ "Schedule Time Zone".
//
// @joestump-agent 09/11/2026 - Added for issue #117.

import _ "time/tzdata"
