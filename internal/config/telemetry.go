package config

// Telemetry Table
//
// Parses and validates the global [telemetry] table (SPEC-0014 REQ-2) into
// core.TelemetryConfig. Everything static is checked here, at load, with the
// offending key's line: durations, sizes, the compression enum, the endpoint's
// shape. What depends on the running daemon — the OTEL_EXPORTER_OTLP_*
// environment and [telemetry] env_file — is resolved later by
// telemetry.Resolve, because a file the TUI round-trips must not depend on the
// environment of whichever process parsed it.
//
// Two refusals carry the ADR-0008 line. A `headers` key is rejected outright:
// headers are how OTLP carries credentials, and rather than guess which values
// are secret, harness.toml carries none. An endpoint with userinfo is rejected
// for the same reason. The table is global-only; project files and harness_d
// drop-ins reject it where they reject other fleet-wide tables.
//
// Governing: ADR-0021; SPEC-0014 REQ-2, REQ-13.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"net/url"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// rawTelemetry mirrors the [telemetry] table before validation. Tuning keys
// are pointers so an absent key takes its default and an explicit zero is
// rejected rather than silently defaulted.
type rawTelemetry struct {
	Logs        bool    `toml:"logs"`
	Traces      bool    `toml:"traces"`
	EventsFile  string  `toml:"events_file"`
	ExportAll   bool    `toml:"export_all"`
	OmitPrompts bool    `toml:"omit_prompts"`
	Endpoint    string  `toml:"endpoint"`
	EnvFile     string  `toml:"env_file"`
	Compression *string `toml:"compression"`
	Timeout     *string `toml:"timeout"`

	QueueSize       *int    `toml:"queue_size"`
	BatchSize       *int    `toml:"batch_size"`
	BatchInterval   *string `toml:"batch_interval"`
	IdleFlush       *string `toml:"idle_flush"`
	ShutdownTimeout *string `toml:"shutdown_timeout"`
	EventsFileMaxMB *int    `toml:"events_file_max_mb"`
	EventsFileKeep  *int    `toml:"events_file_keep"`

	// Headers is decoded only so its presence can be refused (REQ-2).
	Headers any `toml:"headers"`
}

// telemetryHeadersErr is the refusal for a headers key or [telemetry.headers].
func telemetryHeadersErr(filename string, line int) *Error {
	return newError(filename, line,
		"[telemetry]: \"headers\" is not allowed in harness.toml — OTLP headers carry credentials (ADR-0008); set OTEL_EXPORTER_OTLP_HEADERS in the daemon's environment or in the file named by [telemetry] env_file")
}

// removedOTelEndpointErr is the migration error for [daemon] otel_endpoint
// (SPEC-0014 REQ-13). It is refused, not aliased: the key never did anything,
// and aliasing it would turn an inert line into live publication on upgrade.
func removedOTelEndpointErr(filename string, line int) *Error {
	return newError(filename, line,
		"[daemon]: \"otel_endpoint\" was removed (it never exported anything) — to export agent telemetry, add a [telemetry] table with traces = true (and/or logs = true) and set endpoint there or OTEL_EXPORTER_OTLP_ENDPOINT in the environment; see docs/usage/configuration.md")
}

// buildTelemetry validates rt into a core.TelemetryConfig. data and line
// locate each key's own line for the error.
func buildTelemetry(filename string, data []byte, line int, rt rawTelemetry) (core.TelemetryConfig, error) {
	keyLine := func(key string) int {
		if l := lineOfKeyInTable(data, "telemetry", key); l > 0 {
			return l
		}
		return line
	}
	fail := func(key, format string, args ...any) (core.TelemetryConfig, error) {
		return core.TelemetryConfig{}, newError(filename, keyLine(key), "[telemetry] %q: "+format, append([]any{key}, args...)...)
	}

	if rt.Headers != nil {
		return core.TelemetryConfig{}, telemetryHeadersErr(filename, keyLine("headers"))
	}

	tc := core.DefaultTelemetryConfig()
	tc.Logs = rt.Logs
	tc.Traces = rt.Traces
	tc.ExportAll = rt.ExportAll
	tc.OmitPrompts = rt.OmitPrompts

	if rt.EventsFile != "" {
		p := strings.TrimSpace(rt.EventsFile)
		if p == "" {
			return fail("events_file", "must not be blank (omit it to turn the events file off)")
		}
		tc.EventsFile = resolveConfigPath(p, filename)
	}
	if rt.EnvFile != "" {
		p := strings.TrimSpace(rt.EnvFile)
		if p == "" {
			return fail("env_file", "must not be blank")
		}
		tc.EnvFile = resolveConfigPath(p, filename)
	}

	if ep := strings.TrimSpace(rt.Endpoint); ep != "" {
		u, err := url.Parse(ep)
		switch {
		case err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			return fail("endpoint", "must be an absolute http or https URL with a host (got %q)", redactURL(ep))
		case u.User != nil:
			return fail("endpoint", "must not carry userinfo — a credential in the URL is a credential in harness.toml (ADR-0008); put it in OTEL_EXPORTER_OTLP_HEADERS via [telemetry] env_file")
		}
		tc.Endpoint = strings.TrimRight(ep, "/")
	}

	if rt.Compression != nil {
		c := strings.TrimSpace(*rt.Compression)
		if c != "none" && c != "gzip" {
			return fail("compression", "must be \"none\" or \"gzip\" (got %q)", *rt.Compression)
		}
		tc.Compression = c
	}

	durations := []struct {
		key string
		raw *string
		dst *time.Duration
	}{
		{"timeout", rt.Timeout, &tc.Timeout},
		{"batch_interval", rt.BatchInterval, &tc.BatchInterval},
		{"idle_flush", rt.IdleFlush, &tc.IdleFlush},
		{"shutdown_timeout", rt.ShutdownTimeout, &tc.ShutdownTimeout},
	}
	for _, d := range durations {
		if d.raw == nil {
			continue
		}
		v, err := time.ParseDuration(strings.TrimSpace(*d.raw))
		if err != nil || v <= 0 {
			return fail(d.key, "must be a positive duration such as \"5s\" or \"5m\" (got %q)", *d.raw)
		}
		*d.dst = v
	}

	ints := []struct {
		key string
		raw *int
		dst *int
	}{
		{"queue_size", rt.QueueSize, &tc.QueueSize},
		{"batch_size", rt.BatchSize, &tc.BatchSize},
		{"events_file_max_mb", rt.EventsFileMaxMB, &tc.EventsFileMaxMB},
		{"events_file_keep", rt.EventsFileKeep, &tc.EventsFileKeep},
	}
	for _, n := range ints {
		if n.raw == nil {
			continue
		}
		if *n.raw <= 0 {
			return fail(n.key, "must be a positive integer (got %d)", *n.raw)
		}
		*n.dst = *n.raw
	}

	if tc.BatchSize > tc.QueueSize {
		return fail("batch_size", "must not exceed queue_size (%d > %d)", tc.BatchSize, tc.QueueSize)
	}
	if tc.IdleFlush < tc.BatchInterval {
		return fail("idle_flush", "must be at least batch_interval (%s < %s)", tc.IdleFlush, tc.BatchInterval)
	}
	return tc, nil
}

// redactURL drops userinfo from a URL for an error message; an unparseable
// value is elided entirely rather than echoed.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	u.User = nil
	return u.String()
}

// lineOfKeyInTable finds the 1-based line of key's assignment inside the
// [table] header's body, or 0. lineOfKey searches the whole file, which names
// the wrong line for a leaf like "timeout" that harness tables also use.
func lineOfKeyInTable(data []byte, table, key string) int {
	lines := strings.Split(string(data), "\n")
	in := false
	for i, line := range lines {
		if m := headerRe.FindStringSubmatch(line); m != nil {
			in = strings.Join(splitKey(m[1]), ".") == table
			continue
		}
		if arrayHeaderRe.MatchString(line) {
			in = false
			continue
		}
		if !in {
			continue
		}
		m := keyAssignRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1] + m[2] + m[3]
		if name == key {
			return i + 1
		}
	}
	return 0
}
