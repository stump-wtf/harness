package config

// Tests for the SPEC-0014 trigger schema: the [channel.*] and [webhook.*]
// tables, the `triggers` key on [harness.*], and the [server] webhook_* keys.
//
// Two of these carry more weight than their size suggests. The first is the
// located-error table: SPEC-0014 REQ "Error Handling Standards" requires every
// rejection to name the file, the table's line, the source or harness, and the
// reason, and an error that names the wrong line sends an operator hunting
// through a config that is fine. The second is TestSecretNeverAppearsAnywhere,
// which plants a sentinel credential and asserts it is absent from every
// rendering — see its own doc for why it is written the way it is.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Source Table", REQ "Webhook
// Source Table", REQ "Triggers Key", REQ "Triggered Harness Exclusions",
// REQ "Overlap Default For Triggered Harnesses", REQ "One Consumer Per
// Endpoint", REQ "Credential Resolution", REQ "Webhook Listener", REQ
// "Error Handling Standards".
//
// @joestump 09/22/2026 - Introduced with the SPEC-0014 config schema (#454).

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// writeEnv writes an env file with mode 0600 and returns its path. 0600
// matters: a laxer mode produces the group/other warning, which would make
// every test that did not mean to exercise it carry an extra Warning.
func writeEnv(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return p
}

// writeCfg writes a harness.toml into dir and returns its path, so a test can
// exercise the real Load path (which is what resolves relative env_file paths
// against the declaring file).
func writeCfg(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// promptHarness is the minimum a triggered harness needs, since `triggers`
// requires a prompt source.
const promptHarness = "[harness.pr-review]\nharness = \"claude-code\"\nprompt = \"review it\"\n"

func TestChannelSourceParses(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "sb.env", "SB_TOKEN=s3cr3t-value\n")
	path := writeCfg(t, dir, `
[channel.sb]
url = "https://sb.example.com/mcp/x"
env_file = "sb.env"
headers = { Authorization = "Bearer ${SB_TOKEN}", X-Client = "harness" }
description = "switchboard doorbells"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src, ok := cfg.Channels["sb"]
	if !ok {
		t.Fatalf("channel.sb not registered; have %v", cfg.ChannelOrder)
	}
	if src.URL != "https://sb.example.com/mcp/x" {
		t.Errorf("URL = %q", src.URL)
	}
	if !src.Enabled {
		t.Error("an omitted enabled key must default to true")
	}
	// The expansion has to actually have happened: a test that only checked
	// the key was present would pass against a parser that stored the literal
	// "${SB_TOKEN}" and never opened the env file at all.
	if got := src.Headers["Authorization"].Reveal(); got != "Bearer s3cr3t-value" {
		t.Errorf("Authorization = %q, want the expanded value", got)
	}
	if got := src.Headers["X-Client"].Reveal(); got != "harness" {
		t.Errorf("X-Client = %q, want the literal passed through", got)
	}
	if got := src.HeaderNames(); len(got) != 2 || got[0] != "Authorization" || got[1] != "X-Client" {
		t.Errorf("HeaderNames() = %v, want sorted names", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

func TestChannelMinimal(t *testing.T) {
	cfg, err := Parse([]byte("[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n"), "harness.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := cfg.Channels["sb"]; !ok {
		t.Fatal("channel.sb not registered")
	}
	if cfg.Channels["sb"].EnvFile != "" {
		t.Error("no env_file should resolve to no env_file, not a guess")
	}
}

func TestWebhookSourceParses(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "gh.env", "GH_HOOK_SECRET=hunter2-actual\n")
	path := writeCfg(t, dir, `
[webhook.gh]
verify = "github"
env_file = "gh.env"
secret = "${GH_HOOK_SECRET}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src := cfg.Webhooks["gh"]
	if src.Verify != core.VerifyGitHub {
		t.Errorf("Verify = %q", src.Verify)
	}
	// The preset's headers must be FILLED IN, not left for the verifier to
	// re-derive per scheme: that is what lets every consumer downstream read
	// one set of fields.
	if src.SignatureHeader != "X-Hub-Signature-256" {
		t.Errorf("SignatureHeader = %q, want the github preset's", src.SignatureHeader)
	}
	if src.EventHeader != "X-GitHub-Event" || src.DeliveryHeader != "X-GitHub-Delivery" {
		t.Errorf("preset headers not filled in: %+v", src)
	}
	if src.MaxBody != core.DefaultWebhookMaxBody {
		t.Errorf("MaxBody = %d, want the 1MiB default", src.MaxBody)
	}
	if src.RateLimit != core.DefaultWebhookRateLimit {
		t.Errorf("RateLimit = %v, want the 60/m default", src.RateLimit)
	}
	if !src.Enabled {
		t.Error("an omitted enabled key must default to true")
	}
	if got := src.Secret.Reveal(); got != "hunter2-actual" {
		t.Errorf("secret did not resolve (got %d bytes)", len(got))
	}
}

func TestStandardWebhooksPreset(t *testing.T) {
	dir := t.TempDir()
	// Assembled at run time rather than written as a literal. A
	// credential-shaped string in a committed test file is a gitleaks
	// finding, and this repo's answer to that is to build the fixture rather
	// than silence the rule — see .gitleaksignore, where the one existing
	// exception exists only because a closed PR's ref cannot be rewritten.
	// 32 bytes: REQ "Standard Webhooks Verification" requires 24 to 64.
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	writeEnv(t, dir, "sb.env", "SB_NOTIFY_SECRET="+secret+"\n")
	path := writeCfg(t, dir, `
[webhook.switchboard]
verify = "standard-webhooks"
env_file = "sb.env"
secret = "${SB_NOTIFY_SECRET}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src := cfg.Webhooks["switchboard"]
	if src.SignatureHeader != "webhook-signature" || src.DeliveryHeader != "webhook-id" {
		t.Errorf("standard-webhooks headers not filled in: %+v", src)
	}
	if src.EventHeader != "" {
		t.Errorf("EventHeader = %q, want empty — the scheme reads its event name from the body", src.EventHeader)
	}
}

// TestStandardWebhooksSecretFormat covers REQ "Standard Webhooks
// Verification" item 1: the resolved secret must be `whsec_` plus the padded
// standard base64 of 24 to 64 bytes, and every rejection names the source and
// the reference but never the value. Each bad value carries a planted marker
// so the no-echo assertion has something that could actually leak.
// Governing: SPEC-0014 REQ "Standard Webhooks Verification", REQ "Credential
// Resolution".
//
// @joestump 09/23/2026 - Added in review of #584: the format was unchecked.
func TestStandardWebhooksSecretFormat(t *testing.T) {
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", n))) }
	for _, tc := range []struct {
		name, value, want string
		ok                bool
	}{
		{name: "24 bytes", value: "whsec_" + b64(24), ok: true},
		{name: "64 bytes", value: "whsec_" + b64(64), ok: true},
		{name: "bare hex, no prefix", value: "deadbeefcafe0123456789abcdef", want: "prefix"},
		{name: "not base64", value: "whsec_!!not*base64!!", want: "base64"},
		{name: "unpadded base64", value: "whsec_" + strings.TrimRight(b64(25), "="), want: "base64"},
		{name: "too short", value: "whsec_" + b64(23), want: "23 bytes"},
		{name: "too long", value: "whsec_" + b64(65), want: "65 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEnv(t, dir, "sb.env", "SB_NOTIFY_SECRET="+tc.value+"\n")
			path := writeCfg(t, dir, "\n[webhook.switchboard]\nverify = \"standard-webhooks\"\nenv_file = \"sb.env\"\nsecret = \"${SB_NOTIFY_SECRET}\"\n")
			_, err := Load(path)
			if tc.ok {
				if err != nil {
					t.Fatalf("valid secret rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("parsed without error; want the secret format rejected")
			}
			msg := err.Error()
			for _, w := range []string{"webhook.switchboard", "${SB_NOTIFY_SECRET}", "whsec_", tc.want} {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not mention %q", msg, w)
				}
			}
			if strings.Contains(msg, strings.TrimPrefix(tc.value, "whsec_")) {
				t.Errorf("error echoes the secret value: %q", msg)
			}
			var ce *Error
			if !errors.As(err, &ce) || ce.Line != 2 {
				t.Errorf("error = %#v, want a located *Error at the table's line 2", err)
			}
		})
	}
}

// TestEmptyWebhookSecretRejected: `KEY=` in an env file resolves to "", which
// would make a bearer route accept `Authorization: Bearer ` with no token and
// an HMAC route accept a signature anyone can compute. It must fail the load.
// Governing: SPEC-0014 REQ "Credential Resolution".
//
// @joestump 09/23/2026 - Added in review of #584: an empty secret parsed.
func TestEmptyWebhookSecretRejected(t *testing.T) {
	for _, verify := range []string{"bearer", "github"} {
		dir := t.TempDir()
		writeEnv(t, dir, "x.env", "S=\n")
		path := writeCfg(t, dir, "\n[webhook.w]\nverify = \""+verify+"\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n")
		_, err := Load(path)
		if err == nil {
			t.Fatalf("verify = %q: an empty resolved secret parsed", verify)
		}
		for _, w := range []string{"webhook.w", "${S}", "empty"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("verify = %q: error %q does not mention %q", verify, err, w)
			}
		}
	}
}

func TestTriggersBindAndCombineWithSchedule(t *testing.T) {
	cfg, err := Parse([]byte(`
[channel.sb]
url = "https://sb.example.com/mcp/x"

[harness.pr-review]
harness = "claude-code"
prompt = "review it"
schedule = "@every 1h"
triggers = ["channel.sb"]
`), "harness.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h := cfg.Harnesses["pr-review"]
	if !h.Triggered() {
		t.Error("Triggered() = false for a harness with both a schedule and a trigger")
	}
	if len(h.Triggers) != 1 || h.Triggers[0] != "channel.sb" {
		t.Errorf("Triggers = %v", h.Triggers)
	}
	if got := cfg.BoundHarnesses("channel.sb"); len(got) != 1 || got[0] != "pr-review" {
		t.Errorf("BoundHarnesses = %v", got)
	}
}

// TestTriggersResolveAcrossDropIns pins the reason `triggers` references are
// resolved after the last file rather than as each table is read: the main
// config and its harness_d drop-ins are ONE config view, and a binding may
// point in either direction across it.
func TestTriggersResolveAcrossDropIns(t *testing.T) {
	dir := t.TempDir()
	dropIn := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	// The drop-in declares a source the MAIN file's harness binds, and a
	// harness that binds a source the MAIN file declares.
	if err := os.WriteFile(filepath.Join(dropIn, "a.toml"), []byte(`
[channel.from-dropin]
url = "https://sb.example.com/mcp/x"

[harness.dropin-unit]
harness = "claude-code"
prompt = "go"
triggers = ["webhook.from-main"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeEnv(t, dir, "w.env", "S=abc\n")
	path := writeCfg(t, dir, `
[server]
harness_d = "harness.d"

[webhook.from-main]
verify = "bearer"
env_file = "w.env"
secret = "${S}"

[harness.main-unit]
harness = "claude-code"
prompt = "go"
triggers = ["channel.from-dropin"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Harnesses["main-unit"].Triggers) != 1 {
		t.Error("a main-file harness could not bind a drop-in source")
	}
	if len(cfg.Harnesses["dropin-unit"].Triggers) != 1 {
		t.Error("a drop-in harness could not bind a main-file source")
	}
}

// TestOverlapDefaultFollowsFiringSource covers REQ "Overlap Default For
// Triggered Harnesses". The asymmetry is the point: a cron firing that arrives
// during a run is the same work coming round again, so skipping it loses
// nothing; an event firing is a different event, and dropping it loses work
// nothing will re-deliver.
func TestOverlapDefaultFollowsFiringSource(t *testing.T) {
	tests := []struct {
		name string
		keys string
		want core.OverlapPolicy
	}{
		{"schedule alone skips", "schedule = \"@every 1h\"", core.OverlapSkip},
		{"triggers queue", "triggers = [\"channel.sb\"]", core.OverlapQueue},
		{"both queue", "schedule = \"@every 1h\"\ntriggers = [\"channel.sb\"]", core.OverlapQueue},
		{"an explicit value wins", "triggers = [\"channel.sb\"]\non_overlap = \"replace\"", core.OverlapReplace},
		{"an explicit skip wins over the queue default", "triggers = [\"channel.sb\"]\non_overlap = \"skip\"", core.OverlapSkip},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte("[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n"+
				promptHarness+tc.keys+"\n"), "harness.toml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := cfg.Harnesses["pr-review"].OnOverlap; got != tc.want {
				t.Errorf("OnOverlap = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunKeysApplyToATriggeredHarness covers the REQ "Triggered Harness
// Exclusions" row that widens the run keys from "requires schedule" to
// "requires a firing source".
func TestRunKeysApplyToATriggeredHarness(t *testing.T) {
	cfg, err := Parse([]byte("[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n"+
		promptHarness+"triggers = [\"channel.sb\"]\ntimeout = \"20m\"\nkeep_runs = 50\n"), "harness.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h := cfg.Harnesses["pr-review"]
	if h.Timeout.String() != "20m0s" {
		t.Errorf("Timeout = %v", h.Timeout)
	}
	if h.KeepRuns != 50 {
		t.Errorf("KeepRuns = %d", h.KeepRuns)
	}
}

// parseErrCase is one rejection: the config that must fail, and the substrings
// the message must carry. Each `want` entry names something the operator needs
// — the source or harness, and the offending key — because an error that says
// only "invalid" is an error that makes them read the parser.
type parseErrCase struct {
	name string
	toml string
	// line is the 1-based line the error must point at: the offending table's
	// header. 0 skips the check for the handful of cases where the location
	// is genuinely whole-file.
	line int
	want []string
}

// TestSourceParseErrors is the located-error table. Every case asserts the
// FILE and LINE as well as the text, because REQ "Error Handling Standards"
// requires them and because SPEC-0001's reload banner renders the line — an
// error that carries 0 shows the operator "using last-good config" with no
// idea where to look.
func TestSourceParseErrors(t *testing.T) {
	cases := []parseErrCase{
		{
			name: "plain http to a remote host",
			toml: "\n[channel.sb]\nurl = \"http://sb.example.com/mcp/x\"\n",
			line: 2,
			want: []string{"channel.sb", "url", "https"},
		},
		{
			name: "credentials in the url",
			toml: "\n[channel.sb]\nurl = \"https://user:token@sb.example.com/mcp/x\"\n",
			line: 2,
			want: []string{"channel.sb", "url", "headers"},
		},
		{
			// Not "must use https for a remote host": there is no host.
			name: "http url with no host",
			toml: "\n[channel.sb]\nurl = \"http:///mcp/x\"\n",
			line: 2,
			want: []string{"channel.sb", "url", "names no host"},
		},
		{
			name: "http to loopback is accepted, other schemes are not",
			toml: "\n[channel.a]\nurl = \"http://127.0.0.1:8080/mcp\"\n\n[channel.b]\nurl = \"ws://sb.example.com/mcp\"\n",
			line: 5,
			want: []string{"channel.b", "scheme", "ws"},
		},
		{
			name: "missing url",
			toml: "\n[channel.sb]\ndescription = \"x\"\n",
			line: 2,
			want: []string{"channel.sb", "url"},
		},
		{
			name: "same endpoint twice names both sources",
			toml: "\n[channel.a]\nurl = \"https://sb.example.com/mcp/x\"\n\n[channel.b]\nurl = \"https://SB.example.com:443/mcp/x/\"\n",
			line: 5,
			want: []string{"channel.a", "channel.b"},
		},
		{
			name: "invalid source name",
			toml: "\n[channel.\"-nope\"]\nurl = \"https://sb.example.com/mcp/x\"\n",
			line: 2,
			want: []string{"-nope", "source name"},
		},
		{
			name: "duplicate source name",
			toml: "\n[channel.sb]\nurl = \"https://a.example.com/mcp/x\"\n\n[channel.sb]\nurl = \"https://b.example.com/mcp/x\"\n",
			line: 5,
			want: []string{"duplicate", "channel.sb"},
		},
		{
			name: "unknown key on a channel",
			toml: "\n[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\nnope = 1\n",
			want: []string{"unknown key", "nope"},
		},
		{
			name: "webhook missing verify",
			toml: "\n[webhook.gh]\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gh", "verify"},
		},
		{
			name: "unknown verify scheme",
			toml: "\n[webhook.gh]\nverify = \"magic\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gh", "verify", "magic"},
		},
		{
			name: "preset with an overridden header",
			toml: "\n[webhook.gt]\nverify = \"gitea\"\nsignature_header = \"X-Mine\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gt", "signature_header", "gitea"},
		},
		{
			name: "standard-webhooks with an event header",
			toml: "\n[webhook.sb]\nverify = \"standard-webhooks\"\nevent_header = \"X-Event\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.sb", "event_header", "body"},
		},
		{
			name: "event filter without an event header",
			toml: "\n[webhook.b]\nverify = \"bearer\"\nevents = [\"push\"]\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.b", "events", "event_header"},
		},
		{
			name: "hmac without a signature header",
			toml: "\n[webhook.h]\nverify = \"hmac-sha256\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.h", "signature_header"},
		},
		{
			name: "oversized max_body",
			toml: "\n[webhook.gh]\nverify = \"github\"\nmax_body = \"100MiB\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gh", "max_body", "25MiB"},
		},
		{
			name: "malformed max_body",
			toml: "\n[webhook.gh]\nverify = \"github\"\nmax_body = \"1MB\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gh", "max_body", "MiB"},
		},
		{
			name: "malformed rate_limit",
			toml: "\n[webhook.gh]\nverify = \"github\"\nrate_limit = \"60/fortnight\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gh", "rate_limit"},
		},
		{
			name: "webhook missing env_file",
			toml: "\n[webhook.gh]\nverify = \"github\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.gh", "env_file"},
		},
		{
			name: "webhook missing secret",
			toml: "\n[webhook.gh]\nverify = \"github\"\nenv_file = \"x.env\"\n",
			line: 2,
			want: []string{"webhook.gh", "secret"},
		},
		{
			name: "undeclared trigger source",
			toml: "\n" + promptHarness + "triggers = [\"channel.nope\"]\n",
			line: 2,
			want: []string{"pr-review", "channel.nope"},
		},
		{
			name: "malformed trigger reference",
			toml: "\n" + promptHarness + "triggers = [\"sb\"]\n",
			line: 2,
			want: []string{"pr-review", "triggers", "sb"},
		},
		{
			name: "empty triggers list",
			toml: "\n" + promptHarness + "triggers = []\n",
			line: 2,
			want: []string{"pr-review", "triggers", "empty"},
		},
		{
			name: "duplicate trigger reference",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\", \"channel.sb\"]\n",
			line: 4,
			want: []string{"pr-review", "channel.sb", "twice"},
		},
		{
			name: "triggers without a prompt",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				"[harness.x]\nharness = \"generic\"\nargs = [\"true\"]\ntriggers = [\"channel.sb\"]\n",
			line: 4,
			want: []string{"\"x\"", "triggers", "prompt"},
		},
		{
			name: "triggers with enabled",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\"]\nenabled = true\n",
			line: 4,
			want: []string{"pr-review", "triggers", "enabled"},
		},
		{
			name: "triggers with a respawning restart policy",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\"]\nrestart = \"always\"\n",
			line: 4,
			want: []string{"pr-review", "triggers", "restart"},
		},
		{
			name: "triggers with restart unless-stopped",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\"]\nrestart = \"unless-stopped\"\n",
			line: 4,
			want: []string{"pr-review", "triggers", "unless-stopped"},
		},
		{
			name: "triggered harness in a profile",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\"]\n\n[profile.p]\nharnesses = [\"pr-review\"]\n",
			line: 9,
			want: []string{"pr-review", "triggers", "profile"},
		},
		{
			name: "timeout on a harness nothing fires",
			toml: "\n" + promptHarness + "timeout = \"20m\"\n",
			line: 2,
			want: []string{"pr-review", "timeout", "triggers"},
		},
		{
			name: "on_overlap on a harness nothing fires",
			toml: "\n" + promptHarness + "on_overlap = \"queue\"\n",
			line: 2,
			want: []string{"pr-review", "on_overlap", "triggers"},
		},
		{
			name: "keep_runs on a harness nothing fires",
			toml: "\n" + promptHarness + "keep_runs = 5\n",
			line: 2,
			want: []string{"pr-review", "keep_runs", "triggers"},
		},
		{
			name: "catch_up with nothing to miss",
			toml: "[webhook.gh]\nverify = \"github\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n\n" +
				promptHarness + "triggers = [\"webhook.gh\"]\ncatch_up = true\n",
			line: 6,
			want: []string{"pr-review", "catch_up"},
		},
		{
			// A resident hours-gated harness has no firings to miss, and
			// nothing in the runtime reads catch_up for one.
			name: "catch_up on a resident operating_hours harness",
			toml: "\n[harness.r]\nharness = \"generic\"\nargs = [\"true\"]\noperating_hours = \"Mon-Fri 09:00-17:00\"\ncatch_up = true\n",
			line: 2,
			want: []string{"\"r\"", "catch_up", "triggers"},
		},
		{
			name: "hours_shutdown on a triggered harness",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\"]\noperating_hours = \"Mon-Fri 09:00-17:00\"\nhours_shutdown = \"immediate\"\n",
			line: 4,
			want: []string{"pr-review", "hours_shutdown", "triggered"},
		},
		{
			name: "hours_shutdown_timeout on a triggered harness",
			toml: "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n" +
				promptHarness + "triggers = [\"channel.sb\"]\noperating_hours = \"Mon-Fri 09:00-17:00\"\nhours_shutdown_timeout = \"5m\"\n",
			line: 4,
			want: []string{"pr-review", "hours_shutdown_timeout", "triggered"},
		},
		{
			name: "bearer with a signature header",
			toml: "\n[webhook.b]\nverify = \"bearer\"\nsignature_header = \"X-Sig\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n",
			line: 2,
			want: []string{"webhook.b", "signature_header", "bearer"},
		},
		{
			name: "half-configured webhook TLS",
			toml: "\n[server]\nwebhook_listen = \"127.0.0.1:9000\"\nwebhook_tls_cert_file = \"/etc/x.crt\"\n",
			line: 2,
			want: []string{"webhook_tls_key_file", "webhook_tls_cert_file"},
		},
		{
			name: "webhook_listen without a port",
			toml: "\n[server]\nwebhook_listen = \"0.0.0.0\"\n",
			line: 2,
			want: []string{"webhook_listen", "0.0.0.0", "host:port"},
		},
		{
			name: "webhook_listen with a named port",
			toml: "\n[server]\nwebhook_listen = \"127.0.0.1:http\"\n",
			line: 2,
			want: []string{"webhook_listen", "numeric port"},
		},
		{
			name: "array-of-tables source",
			toml: "\n[[channel.sb]]\nurl = \"https://sb.example.com/mcp/x\"\n",
			line: 2,
			want: []string{"unrecognized table", "channel.sb"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A real env file, so a case about some OTHER key is not
			// silently satisfied by a missing-file error instead.
			dir := t.TempDir()
			writeEnv(t, dir, "x.env", "S=abc\n")
			path := writeCfg(t, dir, tc.toml)

			_, err := Load(path)
			if err == nil {
				t.Fatalf("parsed without error; want a rejection naming %v", tc.want)
			}
			msg := err.Error()
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not mention %q", msg, w)
				}
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want a located *Error", err)
			}
			if ce.File != path {
				t.Errorf("File = %q, want %q", ce.File, path)
			}
			if tc.line != 0 && ce.Line != tc.line {
				t.Errorf("Line = %d, want %d (the offending table's header)", ce.Line, tc.line)
			}
		})
	}
}

// TestBareSourceNamespaceIsReserved: before SPEC-0014 a bare `[webhook]` or
// `[channel]` table was a backward-compatible harness of that name (ADR-0006).
// The names are now namespace parents, and a legacy table under one used to
// decode into the namespace and vanish with no error. It must fail the load,
// in the main file and in a drop-in, and a namespace parent that only opens
// sub-tables must still parse.
// Governing: SPEC-0014 REQ "Channel Source Table", REQ "Webhook Source
// Table"; ADR-0006.
//
// @joestump-agent 09/23/2026 - Added in review of #584: the harness vanished.
func TestBareSourceNamespaceIsReserved(t *testing.T) {
	for _, kind := range []string{"channel", "webhook"} {
		legacy := "\n[" + kind + "]\nharness = \"generic\"\nargs = [\"true\"]\n"

		dir := t.TempDir()
		_, err := Load(writeCfg(t, dir, legacy))
		if err == nil {
			t.Fatalf("[%s] legacy harness table loaded without error", kind)
		}
		for _, w := range []string{"[" + kind + "]", "reserved", "[harness." + kind + "]", "args", "harness"} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("[%s]: error %q does not mention %q", kind, err, w)
			}
		}
		var ce *Error
		if !errors.As(err, &ce) || ce.Line != 2 {
			t.Errorf("[%s]: error = %#v, want a located *Error at line 2", kind, err)
		}

		// The same table in a drop-in.
		dir = t.TempDir()
		dropIn := filepath.Join(dir, "harness.d")
		if err := os.MkdirAll(dropIn, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dropIn, "legacy.toml"), []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(writeCfg(t, dir, "[server]\nharness_d = \"harness.d\"\n")); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("[%s] in a drop-in: err = %v, want the reserved-namespace rejection", kind, err)
		}
	}

	// Positive control: a namespace parent that only opens sub-tables.
	cfg, err := Parse([]byte("[channel]\n[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n"), "harness.toml")
	if err != nil {
		t.Fatalf("a bare [channel] parent with only sub-tables was rejected: %v", err)
	}
	if _, ok := cfg.Channels["sb"]; !ok {
		t.Error("channel.sb not registered under an explicit [channel] parent")
	}
}

// TestOperatingHoursAllowedOnATriggeredHarness is the positive control for the
// exclusion above. `operating_hours` excludes a SCHEDULE — a cron one-shot is
// already time-gated by its own expression — but is explicitly allowed on a
// harness whose only firing source is an event, because REQ "Operating Hours
// On Triggered Harnesses" exists to gate exactly those firings. A parser that
// folded triggers into the schedule exclusion would forbid the combination the
// spec is about, and every exclusion test above would still pass.
// Governing: SPEC-0014 REQ "Operating Hours On Triggered Harnesses".
func TestOperatingHoursAllowedOnATriggeredHarness(t *testing.T) {
	cfg, err := Parse([]byte("[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n"+
		promptHarness+"triggers = [\"channel.sb\"]\noperating_hours = \"Mon-Fri 09:00-17:00\"\n"), "harness.toml")
	if err != nil {
		t.Fatalf("operating_hours on a triggered harness was rejected: %v", err)
	}
	h := cfg.Harnesses["pr-review"]
	if h.OperatingHours == "" || !h.Triggered() {
		t.Errorf("harness = %+v, want both operating_hours and a trigger", h)
	}
	// And catch_up, which a channel trigger makes meaningful on its own...
	if _, err := Parse([]byte("[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n"+
		promptHarness+"triggers = [\"channel.sb\"]\ncatch_up = true\n"), "harness.toml"); err != nil {
		t.Errorf("catch_up with a channel trigger was rejected: %v", err)
	}
	// ...and operating_hours makes meaningful on a webhook-only harness, whose
	// out-of-hours skips it catches up (REQ "Operating Hours On Triggered
	// Harnesses"). Without operating_hours the same table is rejected by the
	// "catch_up with nothing to miss" case above.
	dir := t.TempDir()
	writeEnv(t, dir, "x.env", "S=abc\n")
	if _, err := Load(writeCfg(t, dir, "[webhook.gh]\nverify = \"github\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n\n"+
		promptHarness+"triggers = [\"webhook.gh\"]\noperating_hours = \"Mon-Fri 09:00-17:00\"\ncatch_up = true\n")); err != nil {
		t.Errorf("catch_up with operating_hours on a webhook harness was rejected: %v", err)
	}
}

// TestSourceUniquenessSpansDropIns covers the REQ "Channel Source Table"
// sentence that is easiest to implement wrongly: "The same name MUST NOT be
// declared twice across the main file AND ITS DROP-INS." A per-file duplicate
// check would pass every test in this package except this one.
func TestSourceUniquenessSpansDropIns(t *testing.T) {
	dir := t.TempDir()
	dropIn := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropIn, "a.toml"),
		[]byte("[channel.sb]\nurl = \"https://other.example.com/mcp/y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeCfg(t, dir, "[server]\nharness_d = \"harness.d\"\n\n[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a drop-in redeclared a main-file source without error")
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "channel.sb") {
		t.Errorf("error %q should name the duplicate source", err)
	}
	// The error must point at the MAIN file's line too, or an operator with
	// twenty drop-ins has to find the other declaration by hand.
	if !strings.Contains(err.Error(), "harness.toml") {
		t.Errorf("error %q should name where the first declaration lives", err)
	}
}

// TestDropInMayCarrySourceTables is the positive half: a drop-in is how an
// operator adds or removes one unit at a time, and a unit that fires on a
// webhook is not a unit until its source travels with it.
func TestDropInMayCarrySourceTables(t *testing.T) {
	dir := t.TempDir()
	dropIn := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropIn, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnv(t, dropIn, "w.env", "S=abc\n")
	if err := os.WriteFile(filepath.Join(dropIn, "unit.toml"), []byte(`
[webhook.gh]
verify = "github"
env_file = "w.env"
secret = "${S}"

[harness.unit]
harness = "claude-code"
prompt = "go"
triggers = ["webhook.gh"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeCfg(t, dir, "[server]\nharness_d = \"harness.d\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Webhooks["gh"]; !ok {
		t.Error("a drop-in's webhook source was not registered")
	}
	// The drop-in's relative env_file must resolve against the DROP-IN, not
	// the main config: the two live in different directories, and resolving
	// against the wrong one is a missing-file error at load on somebody
	// else's machine.
	if cfg.Webhooks["gh"].Secret.Reveal() != "abc" {
		t.Error("a drop-in's env_file did not resolve against the drop-in's own directory")
	}
	// A drop-in still may not carry the global-only tables.
	if err := os.WriteFile(filepath.Join(dropIn, "zz.toml"), []byte("[profile.p]\nharnesses = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(writeCfg(t, dir, "[server]\nharness_d = \"harness.d\"\n")); err == nil {
		t.Error("a drop-in carrying [profile.*] was accepted")
	}
}

// TestSentinelErrorsAreTestable covers the REQ "Error Handling Standards"
// requirement that distinguishable failure modes be sentinel errors callers
// can test for — not message text they have to match on.
func TestSentinelErrorsAreTestable(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "x.env", "S=abc\n")

	_, err := Load(writeCfg(t, dir, promptHarness+"triggers = [\"channel.nope\"]\n"))
	if !errors.Is(err, ErrUnknownSourceRef) {
		t.Errorf("unknown source reference: errors.Is(ErrUnknownSourceRef) = false (%v)", err)
	}
	_, err = Load(writeCfg(t, dir, "[webhook.gh]\nverify = \"github\"\nenv_file = \"x.env\"\nsecret = \"hunter2\"\n"))
	if !errors.Is(err, ErrLiteralSecret) {
		t.Errorf("literal secret: errors.Is(ErrLiteralSecret) = false (%v)", err)
	}
}

// TestSecretNeverAppearsAnywhere plants a sentinel credential and asserts it is
// absent from every rendering a credential could plausibly escape through.
//
// The trap this test is written around is that an absence assertion passes
// trivially when the value never arrived. So it FIRST asserts the secret
// resolved — `Reveal()` returns the planted value — and only then asserts that
// no rendering contains it. Without that positive control, deleting the
// expansion entirely would make every check below pass.
//
// Governing: SPEC-0014 REQ "Credential Resolution"; ADR-0008.
func TestSecretNeverAppearsAnywhere(t *testing.T) {
	const sentinel = "sentinel-Ah3nEEd1xoo9-value"
	dir := t.TempDir()
	writeEnv(t, dir, "s.env", "TOK="+sentinel+"\n")
	path := writeCfg(t, dir, `
[channel.sb]
url = "https://sb.example.com/mcp/x"
env_file = "s.env"
headers = { Authorization = "Bearer ${TOK}" }

[webhook.gh]
verify = "github"
env_file = "s.env"
secret = "${TOK}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Positive control: the value really is in the config. If this fails,
	// every absence check below is meaningless.
	if got := cfg.Webhooks["gh"].Secret.Reveal(); got != sentinel {
		t.Fatalf("secret did not resolve, so the absence checks below prove nothing")
	}
	if got := cfg.Channels["sb"].Headers["Authorization"].Reveal(); got != "Bearer "+sentinel {
		t.Fatalf("header did not resolve, so the absence checks below prove nothing")
	}

	// Every rendering a projection, a log line or a protocol frame could
	// reach for.
	jsonCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	renderings := map[string]string{
		"%v of the config":       fmt.Sprintf("%v", cfg),
		"%+v of the config":      fmt.Sprintf("%+v", cfg),
		"%#v of the config":      fmt.Sprintf("%#v", cfg),
		"json of the config":     string(jsonCfg),
		"%v of the webhook":      fmt.Sprintf("%v", cfg.Webhooks["gh"]),
		"%+v of the webhook":     fmt.Sprintf("%+v", cfg.Webhooks["gh"]),
		"%s of the secret":       fmt.Sprintf("%s", cfg.Webhooks["gh"].Secret),
		"%v of the channel":      fmt.Sprintf("%+v", cfg.Channels["sb"]),
		"%v of the header value": fmt.Sprintf("%v", cfg.Channels["sb"].Headers["Authorization"]),
	}
	for what, s := range renderings {
		if strings.Contains(s, sentinel) {
			t.Errorf("%s leaks the credential", what)
		}
	}

	// And out of the error path: a config whose secret is a literal must not
	// echo it back, because the whole reason it is refused is that it is one.
	_, err = Load(writeCfg(t, t.TempDir(), "[webhook.gh]\nverify = \"github\"\nenv_file = \"s.env\"\nsecret = \""+sentinel+"\"\n"))
	if err == nil {
		t.Fatal("a literal secret parsed")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the literal-secret error echoes the value it exists to refuse")
	}
}

// TestCredentialResolutionIgnoresTheProcessEnvironment pins the rule that
// makes the CLI and the daemon agree. config.Load runs in both, and systemd
// starts the daemon with a different environment; if a reference could resolve
// from the process environment, `harness triggers` and the daemon would
// disagree about what a source is configured with, and the disagreement would
// surface as a 401 nobody could reproduce.
// Governing: SPEC-0014 REQ "Credential Resolution".
func TestCredentialResolutionIgnoresTheProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "s.env", "TOK=from-the-file\n")
	// A conflicting value in the process environment, exactly the shape a
	// shell would have exported.
	t.Setenv("TOK", "from-the-process")

	path := writeCfg(t, dir, "[webhook.gh]\nverify = \"github\"\nenv_file = \"s.env\"\nsecret = \"${TOK}\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Webhooks["gh"].Secret.Reveal(); got != "from-the-file" {
		t.Errorf("secret resolved to %q; a reference must come from the source's own env_file, never the process environment", got)
	}

	// The other half: with no env_file at all, a reference is an ERROR
	// rather than a silent fall-through to the process environment.
	_, err = Load(writeCfg(t, t.TempDir(), "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\nheaders = { Authorization = \"${TOK}\" }\n"))
	if err == nil {
		t.Fatal("a ${NAME} reference with no env_file resolved")
	}
	if !strings.Contains(err.Error(), "env_file") {
		t.Errorf("error %q should name env_file", err)
	}
}

func TestCredentialResolutionErrors(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "s.env", "OTHER=x\n")

	t.Run("missing name", func(t *testing.T) {
		_, err := Load(writeCfg(t, dir, "[webhook.gh]\nverify = \"github\"\nenv_file = \"s.env\"\nsecret = \"${NOPE}\"\n"))
		if err == nil || !strings.Contains(err.Error(), "NOPE") {
			t.Fatalf("want an error naming the reference, got %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		_, err := Load(writeCfg(t, t.TempDir(), "[webhook.gh]\nverify = \"github\"\nenv_file = \"gone.env\"\nsecret = \"${TOK}\"\n"))
		if err == nil || !strings.Contains(err.Error(), "gone.env") {
			t.Fatalf("want an error naming the file, got %v", err)
		}
	})
}

// TestLaxEnvFileWarns covers the REQ "Credential Resolution" warning. It is a
// warning and not a failure because the operator may be on a single-user
// machine, and refusing to start over a file mode would be worse than saying
// so — but `harness doctor` has to have something to flag.
func TestLaxEnvFileWarns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.env")
	if err := os.WriteFile(p, []byte("TOK=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeCfg(t, dir, "[webhook.gh]\nverify = \"github\"\nenv_file = \"s.env\"\nsecret = \"${TOK}\"\n"))
	if err != nil {
		t.Fatalf("a group-readable env_file must warn, not fail: %v", err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "group or other") {
		t.Fatalf("Warnings = %v, want one group/other-readable finding", cfg.Warnings)
	}
	if strings.Contains(cfg.Warnings[0], "TOK=") {
		t.Error("the warning must name the path, never the file's contents")
	}

	// And the control: a 0600 file produces no warning at all, so the check
	// above is not passing because every load warns.
	dir2 := t.TempDir()
	writeEnv(t, dir2, "s.env", "TOK=x\n")
	cfg2, err := Load(writeCfg(t, dir2, "[webhook.gh]\nverify = \"github\"\nenv_file = \"s.env\"\nsecret = \"${TOK}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg2.Warnings) != 0 {
		t.Errorf("a 0600 env_file warned: %v", cfg2.Warnings)
	}
}

// TestWebhookListenerConfig covers REQ "Webhook Listener"'s config half: the
// listener is off unless an address names it, and a non-loopback bind with no
// TLS starts but warns.
func TestWebhookListenerConfig(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		dir := t.TempDir()
		writeEnv(t, dir, "x.env", "S=abc\n")
		cfg, err := Load(writeCfg(t, dir, "[webhook.gh]\nverify = \"github\"\nenv_file = \"x.env\"\nsecret = \"${S}\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.WebhookListen != "" {
			t.Errorf("WebhookListen = %q; declaring a webhook source must not open a port", cfg.Server.WebhookListen)
		}
	})
	t.Run("loopback bind is quiet", func(t *testing.T) {
		cfg, err := Parse([]byte("[server]\nwebhook_listen = \"127.0.0.1:9000\"\n"), "harness.toml")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("Warnings = %v", cfg.Warnings)
		}
	})
	for _, addr := range []string{"0.0.0.0:9000", ":9000", "10.0.0.5:9000"} {
		t.Run("non-loopback warns "+addr, func(t *testing.T) {
			cfg, err := Parse([]byte("[server]\nwebhook_listen = \""+addr+"\"\n"), "harness.toml")
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "cleartext") {
				t.Errorf("Warnings = %v, want one cleartext finding", cfg.Warnings)
			}
		})
	}
	t.Run("TLS silences the warning", func(t *testing.T) {
		cfg, err := Parse([]byte("[server]\nwebhook_listen = \"0.0.0.0:9000\"\nwebhook_tls_cert_file = \"/etc/x.crt\"\nwebhook_tls_key_file = \"/etc/x.key\"\n"), "harness.toml")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("Warnings = %v", cfg.Warnings)
		}
	})
}

func TestDisabledSourceStaysDeclared(t *testing.T) {
	cfg, err := Parse([]byte("[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\nenabled = false\n"), "harness.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	src, ok := cfg.Channels["sb"]
	if !ok {
		t.Fatal("a disabled source must stay declared and bindable")
	}
	if src.Enabled {
		t.Error("Enabled = true for enabled = false")
	}
}

// TestSourceTablesRejectedInProjectFiles covers the project half of REQ
// "Channel Source Table" / REQ "Webhook Source Table" and the `triggers` row
// of REQ "Triggered Harness Exclusions".
func TestSourceTablesRejectedInProjectFiles(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{"channel table", "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n", "channel.sb"},
		{"webhook table", "[webhook.gh]\nverify = \"github\"\n", "webhook.gh"},
		{"triggers key", "[harness.x]\nharness = \"claude-code\"\nprompt = \"go\"\ntriggers = [\"channel.sb\"]\n", "triggers"},
		// Rejected on presence: an empty list is still the daemon-owned key.
		{"empty triggers key", "[harness.x]\nharness = \"claude-code\"\nprompt = \"go\"\ntriggers = []\n", "triggers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseProject([]byte(tc.toml), "harness.toml")
			if err == nil {
				t.Fatal("a project file accepted a daemon-owned trigger key")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			// "daemon's harness.toml", not bare "harness.toml": the file
			// under test is itself named harness.toml, so the location
			// prefix alone would satisfy a bare check.
			if !strings.Contains(err.Error(), "daemon's harness.toml") {
				t.Errorf("error %q should point at the daemon's harness.toml", err)
			}
		})
	}
}

// TestEnvFileFormatMatchesSupervisor pins the duplication the envResolver
// carries. internal/supervisor imports internal/config, so config cannot
// import supervisor's parseEnvFile and the KEY=VALUE reader exists twice. This
// test is what keeps the two from drifting: it feeds the awkward shapes
// (export prefix, quotes, comments, blank lines, an `=` in the value) through
// the config side and asserts the supervisor's documented handling.
func TestEnvFileFormatMatchesSupervisor(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "s.env", strings.Join([]string{
		"# a comment",
		"",
		"export EXPORTED=plain",
		`DQUOTED="double"`,
		"SQUOTED='single'",
		"EQUALS=a=b=c",
		"  SPACED  =  padded  ",
		"not a kv line",
	}, "\n")+"\n")
	path := writeCfg(t, dir, `
[channel.sb]
url = "https://sb.example.com/mcp/x"
env_file = "s.env"
headers = { A = "${EXPORTED}", B = "${DQUOTED}", C = "${SQUOTED}", D = "${EQUALS}", E = "${SPACED}" }
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{"A": "plain", "B": "double", "C": "single", "D": "a=b=c", "E": "padded"}
	for k, v := range want {
		if got := cfg.Channels["sb"].Headers[k].Reveal(); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
}
