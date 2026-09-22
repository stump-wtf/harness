package main

// Doctor Telemetry Section
//
// `harness doctor` reports what [telemetry] resolves to, per signal, in the
// same setting/source/value shape as the process settings table: whether the
// signal is enabled, its endpoint (or file path), compression and timeout,
// and each header by NAME with a truncated SHA-256 fingerprint of its value —
// never the value (SPEC-0015 REQ-3, REQ-14). It answers "is the credential the
// daemon sends the one I rotated in?" by comparing fingerprints, without
// putting the credential on a terminal.
//
// Doctor resolves in its own process, so the environment is the caller's
// shell, not the daemon's: a systemd EnvironmentFile or a service user's
// unreadable env_file can make the daemon's answer differ. The table says so,
// and a resolve failure is a warning row rather than a hidden section.
//
// Governing: ADR-0022; SPEC-0015 REQ-3 (header names and fingerprints only),
// REQ-14 (doctor reports the resolved settings); SPEC-0010 REQ "Source
// Attribution" (the format).
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"fmt"
	"io"
	"sort"

	"charm.land/lipgloss/v2"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/telemetry"
)

// telemetrySetting is one resolved telemetry setting.
type telemetrySetting struct {
	Name   string // "logs.endpoint", "logs.header.authorization", …
	Source string // "env" | "env_file" | "file" | "default"
	Value  string
}

// telemetryReport resolves tc against env the way the daemon does at startup.
// It returns nothing when no destination is configured (the daemon then
// builds nothing either), and a warning row when resolution fails — the
// failure the daemon would refuse to start on.
func telemetryReport(tc core.TelemetryConfig, env telemetry.Env) ([]telemetrySetting, *check) {
	res, err := telemetry.Resolve(tc, env)
	if err != nil {
		return nil, &check{
			name:   "telemetry",
			level:  cliui.LevelWarn,
			detail: err.Error(),
			hint:   "the daemon refuses to start with this [telemetry] config; if it runs under another user or environment, its resolution can differ from this shell's",
		}
	}
	if res == nil {
		return nil, nil
	}
	var out []telemetrySetting
	add := func(name, source, value string) {
		out = append(out, telemetrySetting{Name: name, Source: source, Value: value})
	}
	for _, s := range []struct {
		name       string
		sig        *telemetry.Signal
		sdkDisable bool // consented in the file, turned off by OTEL_SDK_DISABLED
	}{
		{telemetry.SignalLogs, res.Logs, tc.Logs && res.Logs == nil},
		{telemetry.SignalTraces, res.Traces, tc.Traces && res.Traces == nil},
	} {
		switch {
		case s.sig != nil:
			add(s.name+".enabled", telemetry.SourceFile, "true")
		case s.sdkDisable:
			add(s.name+".enabled", telemetry.SourceEnv, "false (OTEL_SDK_DISABLED)")
			continue
		default:
			add(s.name+".enabled", telemetry.SourceDefault, "false")
			continue
		}
		sig := s.sig
		add(s.name+".endpoint", sig.URLSource, telemetry.SafeURL(sig.URL))
		add(s.name+".compression", sig.CompressionSource, sig.Compression)
		add(s.name+".timeout", sig.TimeoutSource, sig.Timeout.String())
		names := make([]string, 0, len(sig.Headers))
		for k := range sig.Headers {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			add(s.name+".header."+k, sig.HeaderSources[k], "sha256:"+telemetry.Fingerprint(sig.Headers[k]))
		}
	}
	if path := res.EventsFile(); path != "" {
		add(telemetry.SignalEventsFile+".enabled", telemetry.SourceFile, "true")
		add(telemetry.SignalEventsFile+".path", telemetry.SourceFile, path)
	} else {
		add(telemetry.SignalEventsFile+".enabled", telemetry.SourceDefault, "false")
	}
	return out, nil
}

// printTelemetryTable renders the telemetry section after the settings table.
func printTelemetryTable(w io.Writer, ts []telemetrySetting) {
	if len(ts) == 0 {
		return
	}
	fmt.Fprintln(w)
	t := NewTable(w, "TELEMETRY", "SOURCE", "VALUE")
	for _, s := range ts {
		source := s.Source
		if t.colored && s.Source != telemetry.SourceDefault {
			source = lipgloss.NewStyle().Foreground(t.pal.Accent).Render(source)
		}
		t.Row(s.Name, source, s.Value)
	}
	t.RowFull("", t.dimItalic("resolved in this shell's environment; the daemon's may differ"))
	_ = t.Flush()
}
