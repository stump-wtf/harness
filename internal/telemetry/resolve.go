package telemetry

// Resolution
//
// Turns the parsed [telemetry] table plus the daemon's environment into what
// each OTLP signal will actually do: its URL, headers, compression and
// timeout, and where each came from. It runs once, at daemon startup, before
// any harness is started, so a signal that is consented to but cannot be
// delivered refuses the start rather than being discovered later.
//
// Precedence per setting, per signal (SPEC-0014 REQ-3):
//
//	signal-specific env  >  generic env  >  [telemetry] in harness.toml  >  default
//
// where "env" is the process environment first, then [telemetry] env_file.
// An exported-but-empty variable counts as unset. Environment variables never
// ENABLE a signal — only logs/traces in harness.toml do — because an
// OTEL_EXPORTER_OTLP_ENDPOINT inherited from a login shell is not consent to
// publish transcripts. Headers come only from the environment or env_file,
// and env_file values are returned here, never put into the process
// environment, so no supervised harness inherits the collector credential.
//
// Nothing this file produces for a log line carries a header value or URL
// userinfo: headers are reported as a name and a truncated SHA-256
// fingerprint, endpoints with userinfo and query stripped.
//
// Governing: ADR-0021; SPEC-0014 REQ-3, REQ-5, REQ-14; ADR-0008.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/otlpexport"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// Setting sources, as REQ-14 names them.
const (
	SourceEnv     = "env"
	SourceEnvFile = "env_file"
	SourceFile    = "file"
	SourceDefault = "default"
)

// Signal names, used for subscriptions, stats and log lines.
const (
	SignalLogs       = "logs"
	SignalTraces     = "traces"
	SignalEventsFile = "events_file"
)

// EnvKeys is every variable Resolve reads. Only these are honoured from
// [telemetry] env_file; every other key in it is ignored.
var EnvKeys = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"OTEL_EXPORTER_OTLP_LOGS_HEADERS",
	"OTEL_EXPORTER_OTLP_TRACES_HEADERS",
	"OTEL_EXPORTER_OTLP_COMPRESSION",
	"OTEL_EXPORTER_OTLP_LOGS_COMPRESSION",
	"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
	"OTEL_EXPORTER_OTLP_TIMEOUT",
	"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT",
	"OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL",
	"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
	"OTEL_RESOURCE_ATTRIBUTES",
	"OTEL_SDK_DISABLED",
}

// Env is what Resolve reads from the outside world; tests supply their own.
type Env struct {
	// Getenv reads the daemon's process environment.
	Getenv func(string) string
	// ReadEnvFile reads [telemetry] env_file, returning only EnvKeys, an
	// optional warning, and an error when the file is missing or unreadable.
	ReadEnvFile func(path string) (vals map[string]string, warning string, err error)
	// Hostname names the host for the host.name resource attribute.
	Hostname func() (string, error)
	// Version is service.version and the instrumentation scope version.
	Version string
}

// ProcessEnv is the production Env.
func ProcessEnv(version string) Env {
	return Env{Getenv: os.Getenv, ReadEnvFile: ReadEnvFile, Hostname: os.Hostname, Version: version}
}

// ReadEnvFile reads the EnvKeys from path with the harness env_file parser.
// A missing file is an error here, unlike a harness env_file: an OTLP signal
// whose credential file vanished must not start sending without it. A group-
// or world-readable file is a warning, as for harness env files (ADR-0008).
func ReadEnvFile(path string) (map[string]string, string, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "", fmt.Errorf("[telemetry] env_file %q does not exist", path)
	case err != nil:
		return nil, "", fmt.Errorf("[telemetry] env_file %q is not readable: %w", path, err)
	case info.IsDir():
		return nil, "", fmt.Errorf("[telemetry] env_file %q is a directory", path)
	}
	var warn string
	if info.Mode().Perm()&0o077 != 0 {
		warn = fmt.Sprintf("[telemetry] env_file %q is readable by group or others (mode %04o); it holds collector credentials — chmod 600 it", path, info.Mode().Perm())
	}
	vals, err := supervisor.EnvFileValues(path, EnvKeys)
	if err != nil {
		return nil, warn, fmt.Errorf("[telemetry] env_file %q is not readable: %w", path, err)
	}
	return vals, warn, nil
}

// Signal is one OTLP signal's resolved delivery settings.
type Signal struct {
	Name string
	// URL is the full request URL; URLSource where it came from.
	URL       string
	URLSource string
	// Headers are the merged request headers, keyed by lowercase name.
	// HeaderSources records each one's source.
	Headers       map[string]string
	HeaderSources map[string]string
	// Compression is "none" or "gzip".
	Compression       string
	CompressionSource string
	// Timeout bounds one request.
	Timeout       time.Duration
	TimeoutSource string
}

// Endpoint is the otlpexport form of s.
func (s *Signal) Endpoint() otlpexport.Endpoint {
	return otlpexport.Endpoint{URL: s.URL, Headers: s.Headers, Gzip: s.Compression == "gzip", Timeout: s.Timeout}
}

// LogKeyvals is s as charm log key/value pairs, safe to print: the endpoint
// without userinfo or query, and each header as name=fingerprint.
func (s *Signal) LogKeyvals() []any {
	names := make([]string, 0, len(s.Headers))
	for k := range s.Headers {
		names = append(names, k)
	}
	sort.Strings(names)
	hs := make([]string, 0, len(names))
	for _, k := range names {
		hs = append(hs, fmt.Sprintf("%s=sha256:%s(%s)", k, Fingerprint(s.Headers[k]), s.HeaderSources[k]))
	}
	headers := strings.Join(hs, ",")
	if headers == "" {
		headers = "none"
	}
	return []any{
		"signal", s.Name,
		"endpoint", SafeURL(s.URL), "endpoint_source", s.URLSource,
		"compression", s.Compression, "compression_source", s.CompressionSource,
		"timeout", s.Timeout, "timeout_source", s.TimeoutSource,
		"headers", headers,
	}
}

// Resolved is the whole pipeline's startup configuration.
type Resolved struct {
	// Config is the [telemetry] table with defaults filled in.
	Config core.TelemetryConfig
	// Logs and Traces are nil when that signal is off.
	Logs   *Signal
	Traces *Signal
	// Resource is the one OTLP resource every unit belongs to (REQ-5).
	Resource []otlpexport.KeyValue
	// Version is the build version, for the instrumentation scope.
	Version string
	// Warnings and Notes are for the daemon log, already value-free.
	Warnings []string
	Notes    []string
}

// EventsFile is the resolved events file path, or "".
func (r *Resolved) EventsFile() string { return r.Config.EventsFile }

// Enabled reports whether anything is left to run.
func (r *Resolved) Enabled() bool {
	return r != nil && (r.Logs != nil || r.Traces != nil || r.Config.EventsFile != "")
}

// Resolve applies the environment to cfg. With no destination configured it
// returns (nil, nil): the daemon then builds nothing at all.
func Resolve(cfg core.TelemetryConfig, env Env) (*Resolved, error) {
	if env.Getenv == nil {
		env.Getenv = func(string) string { return "" }
	}
	if !cfg.Enabled() {
		return nil, nil
	}
	cfg = cfg.WithDefaults()
	res := &Resolved{Config: cfg, Version: env.Version}

	// env_file is read only when a network signal needs it; the events file
	// alone has no use for collector settings.
	var fileVals map[string]string
	if cfg.OTLPEnabled() && cfg.EnvFile != "" {
		if env.ReadEnvFile == nil {
			env.ReadEnvFile = ReadEnvFile
		}
		vals, warn, err := env.ReadEnvFile(cfg.EnvFile)
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
		if err != nil {
			return nil, err
		}
		fileVals = vals
	}
	lookup := func(key string) (string, string) {
		if v := strings.TrimSpace(env.Getenv(key)); v != "" {
			return v, SourceEnv
		}
		if v := strings.TrimSpace(fileVals[key]); v != "" {
			return v, SourceEnvFile
		}
		return "", ""
	}

	res.Resource = resourceAttrs(env, lookup, res)

	if !cfg.OTLPEnabled() {
		return res, nil
	}
	if v, _ := lookup("OTEL_SDK_DISABLED"); strings.EqualFold(v, "true") {
		res.Notes = append(res.Notes, "OTEL_SDK_DISABLED=true: the OTLP logs and traces signals are off (the events file, if configured, still runs)")
		return res, nil
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"} {
		if v, _ := lookup(key); v != "" && v != "http/json" {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s=%q is ignored: Harness speaks only OTLP/HTTP JSON (http/json)", key, v))
		}
	}

	for _, name := range []string{SignalLogs, SignalTraces} {
		on := (name == SignalLogs && cfg.Logs) || (name == SignalTraces && cfg.Traces)
		if !on {
			continue
		}
		sig, err := resolveSignal(name, cfg, lookup, res)
		if err != nil {
			return nil, err
		}
		if name == SignalLogs {
			res.Logs = sig
		} else {
			res.Traces = sig
		}
	}
	return res, nil
}

// IgnoredEnvNote reports OTLP variables present in the process environment
// while no OTLP signal is enabled — the laptop case REQ-1 says to mention once
// — or "" when there is nothing to say.
func IgnoredEnvNote(cfg core.TelemetryConfig, getenv func(string) string) string {
	if cfg.OTLPEnabled() || getenv == nil {
		return ""
	}
	var set []string
	for _, k := range EnvKeys {
		if strings.HasPrefix(k, "OTEL_EXPORTER_OTLP_") && strings.TrimSpace(getenv(k)) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		return ""
	}
	return "OTEL_EXPORTER_OTLP_* variables are set (" + strings.Join(set, ", ") + ") but ignored: no telemetry is exported unless [telemetry] logs = true or traces = true in harness.toml"
}

func resolveSignal(name string, cfg core.TelemetryConfig, lookup func(string) (string, string), res *Resolved) (*Signal, error) {
	up := strings.ToUpper(name)
	sig := &Signal{Name: name, Headers: map[string]string{}, HeaderSources: map[string]string{}}

	// Endpoint: the signal-specific variable is a full URL used as-is; the
	// generic variable and the file's endpoint are base URLs.
	specific := "OTEL_EXPORTER_OTLP_" + up + "_ENDPOINT"
	if v, src := lookup(specific); v != "" {
		if err := checkURL(v); err != nil {
			return nil, fmt.Errorf("telemetry %s: %s (from %s): %w", name, specific, src, err)
		}
		sig.URL, sig.URLSource = v, src
	} else if v, src := lookup("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		if err := checkURL(v); err != nil {
			return nil, fmt.Errorf("telemetry %s: OTEL_EXPORTER_OTLP_ENDPOINT (from %s): %w", name, src, err)
		}
		sig.URL, sig.URLSource = joinSignalPath(v, name), src
	} else if cfg.Endpoint != "" {
		sig.URL, sig.URLSource = joinSignalPath(cfg.Endpoint, name), SourceFile
	} else {
		return nil, fmt.Errorf("telemetry: [telemetry] %s = true but no endpoint is configured — set [telemetry] endpoint in harness.toml, or %s or OTEL_EXPORTER_OTLP_ENDPOINT in the daemon's environment or [telemetry] env_file", name, specific)
	}

	// Headers: generic first, signal-specific overriding per key.
	for _, key := range []string{"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_" + up + "_HEADERS"} {
		v, src := lookup(key)
		if v == "" {
			continue
		}
		hs, bad := parseHeaders(v)
		for _, i := range bad {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: entry %d is not name=value and was skipped", key, i+1))
		}
		for k, val := range hs {
			sig.Headers[k] = val
			sig.HeaderSources[k] = src
		}
	}

	// Compression.
	sig.Compression, sig.CompressionSource = "none", SourceDefault
	if cfg.Compression != "" {
		sig.Compression, sig.CompressionSource = cfg.Compression, SourceFile
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_" + up + "_COMPRESSION"} {
		v, src := lookup(key)
		switch {
		case v == "":
		case v == "none" || v == "gzip":
			sig.Compression, sig.CompressionSource = v, src
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s=%q is ignored (want none or gzip)", key, v))
		}
	}

	// Timeout: the variables are milliseconds.
	sig.Timeout, sig.TimeoutSource = core.DefaultTelemetryTimeout, SourceDefault
	if cfg.Timeout > 0 {
		sig.Timeout, sig.TimeoutSource = cfg.Timeout, SourceFile
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_" + up + "_TIMEOUT"} {
		v, src := lookup(key)
		if v == "" {
			continue
		}
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s=%q is ignored (want a positive number of milliseconds)", key, v))
			continue
		}
		sig.Timeout, sig.TimeoutSource = time.Duration(ms)*time.Millisecond, src
	}

	if len(sig.Headers) > 0 && cleartextRemote(sig.URL) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("telemetry %s: headers will be sent in cleartext to a non-loopback http:// endpoint (%s); use https", name, SafeURL(sig.URL)))
	}
	return sig, nil
}

// resourceAttrs builds the resource: the three attributes Harness owns, then
// OTEL_RESOURCE_ATTRIBUTES, which may add but never override them.
func resourceAttrs(env Env, lookup func(string) (string, string), res *Resolved) []otlpexport.KeyValue {
	host := ""
	if env.Hostname != nil {
		host, _ = env.Hostname()
	}
	out := []otlpexport.KeyValue{
		{Key: "service.name", Value: "harness"},
		{Key: "service.version", Value: env.Version},
		{Key: "host.name", Value: host},
	}
	own := map[string]bool{"service.name": true, "service.version": true, "host.name": true}
	v, _ := lookup("OTEL_RESOURCE_ATTRIBUTES")
	if v == "" {
		return out
	}
	extra, bad := parseHeaders(v)
	for _, i := range bad {
		res.Warnings = append(res.Warnings, fmt.Sprintf("OTEL_RESOURCE_ATTRIBUTES: entry %d is not key=value and was skipped", i+1))
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if own[k] {
			continue
		}
		out = append(out, otlpexport.KeyValue{Key: k, Value: extra[k]})
	}
	return out
}

// parseHeaders parses the OTel "k=v,k2=v2" list with percent-decoded values,
// keys lowercased. bad lists the indexes of malformed entries.
func parseHeaders(s string) (map[string]string, []int) {
	out := map[string]string{}
	var bad []int
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			bad = append(bad, i)
			continue
		}
		v = strings.TrimSpace(v)
		if dec, err := url.PathUnescape(v); err == nil {
			v = dec
		}
		out[strings.ToLower(k)] = v
	}
	return out, bad
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("not an absolute http or https URL with a host")
	}
	return nil
}

// joinSignalPath appends /v1/<signal> to a base URL.
func joinSignalPath(base, signal string) string {
	return strings.TrimRight(base, "/") + "/v1/" + signal
}

// SafeURL is raw without userinfo or query, for a log line.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func cleartextRemote(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// Fingerprint is the first 12 hex characters of v's SHA-256: enough to tell
// whether a rotated credential landed, nothing usable.
func Fingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}
