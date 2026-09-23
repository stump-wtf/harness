package core

// Unit tests for the pure helpers the trigger schema is built on: the size and
// rate-limit grammars, endpoint normalization, reference parsing, and the
// Secret type's redaction.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Source Table", REQ "Webhook Rate
// Limit", REQ "One Consumer Per Endpoint", REQ "Triggers Key", REQ "Credential
// Resolution".
//
// @joestump 09/22/2026 - Introduced with the SPEC-0014 config schema (#454).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseByteSize(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"1B", 1},
		{"512B", 512},
		{"1KiB", 1024},
		{"1MiB", 1 << 20},
		{"25MiB", 25 << 20},
		{" 2MiB ", 2 << 20},
	}
	for _, tc := range ok {
		got, err := ParseByteSize(tc.in)
		if err != nil {
			t.Errorf("ParseByteSize(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// "MB" is refused on purpose: a config that writes MB and means MiB is a
	// config whose author is guessing, and silently picking one meaning makes
	// the guess permanent.
	bad := []string{"", "1", "1MB", "1 mib", "0B", "-1MiB", "1GiB", "MiB", "99999999999999999999MiB"}
	for _, in := range bad {
		if got, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) = %d, want an error", in, got)
		}
	}
}

func TestFormatByteSizeRoundTrips(t *testing.T) {
	for _, in := range []string{"1B", "999B", "1KiB", "3KiB", "1MiB", "25MiB"} {
		n, err := ParseByteSize(in)
		if err != nil {
			t.Fatalf("ParseByteSize(%q): %v", in, err)
		}
		if got := FormatByteSize(n); got != in {
			t.Errorf("FormatByteSize(ParseByteSize(%q)) = %q", in, got)
		}
	}
}

func TestParseRateLimit(t *testing.T) {
	ok := []struct {
		in   string
		want RateLimit
	}{
		{"0", RateLimit{}},
		{"1/s", RateLimit{Count: 1, Window: time.Second}},
		{"60/m", RateLimit{Count: 60, Window: time.Minute}},
		{"1000/h", RateLimit{Count: 1000, Window: time.Hour}},
	}
	for _, tc := range ok {
		got, err := ParseRateLimit(tc.in)
		if err != nil {
			t.Errorf("ParseRateLimit(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseRateLimit(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if rendered := got.String(); rendered != tc.in {
			t.Errorf("RateLimit(%q).String() = %q", tc.in, rendered)
		}
	}
	if !(RateLimit{}).Unlimited() {
		t.Error(`"0" must mean no limit`)
	}
	for _, in := range []string{"", "60", "60/", "/m", "0/m", "-1/m", "60/d", "abc/m"} {
		if got, err := ParseRateLimit(in); err == nil {
			t.Errorf("ParseRateLimit(%q) = %+v, want an error", in, got)
		}
	}
}

// TestNormalizeEndpoint covers REQ "One Consumer Per Endpoint". Normalization
// is comparison-only — ChannelSource.URL keeps what the operator wrote — so
// the risk it carries is a false EQUAL, not a wrong dial.
func TestNormalizeEndpoint(t *testing.T) {
	same := [][2]string{
		{"https://sb.example.com/mcp/x", "https://SB.Example.com/mcp/x"},
		{"https://sb.example.com/mcp/x", "HTTPS://sb.example.com/mcp/x"},
		{"https://sb.example.com/mcp/x", "https://sb.example.com:443/mcp/x"},
		{"https://sb.example.com/mcp/x", "https://sb.example.com/mcp/x/"},
		{"http://localhost/mcp", "http://localhost:80/mcp/"},
	}
	for _, p := range same {
		if a, b := NormalizeEndpoint(p[0]), NormalizeEndpoint(p[1]); a != b {
			t.Errorf("NormalizeEndpoint(%q)=%q != NormalizeEndpoint(%q)=%q", p[0], a, p[1], b)
		}
	}
	// Differences that must survive normalization, or two genuinely separate
	// endpoints would collapse into one and the second would be rejected.
	differ := [][2]string{
		{"https://sb.example.com/mcp/x", "https://sb.example.com/mcp/y"},
		{"https://sb.example.com/mcp/x", "https://sb.example.com:8443/mcp/x"},
		{"https://a.example.com/mcp/x", "https://b.example.com/mcp/x"},
		{"https://sb.example.com/mcp/x", "https://sb.example.com/mcp/x?q=1"},
	}
	for _, p := range differ {
		if a, b := NormalizeEndpoint(p[0]), NormalizeEndpoint(p[1]); a == b {
			t.Errorf("NormalizeEndpoint collapsed %q and %q into %q", p[0], p[1], a)
		}
	}
}

func TestParseTriggerRef(t *testing.T) {
	for _, in := range []string{"channel.sb", "webhook.gitea-pr", "channel.a_b-1"} {
		ref, err := ParseTriggerRef(in)
		if err != nil {
			t.Errorf("ParseTriggerRef(%q): %v", in, err)
			continue
		}
		if ref.String() != in {
			t.Errorf("ParseTriggerRef(%q).String() = %q", in, ref.String())
		}
	}
	for _, in := range []string{"", "sb", "queue.sb", "channel.", "channel", ".sb", "channel.-bad", "channel.sb.x"} {
		if ref, err := ParseTriggerRef(in); err == nil {
			t.Errorf("ParseTriggerRef(%q) = %+v, want an error", in, ref)
		}
	}
}

func TestVerifySchemePresets(t *testing.T) {
	for _, v := range VerifySchemes {
		if !v.Valid() {
			t.Errorf("%q is listed but not Valid()", v)
		}
	}
	if VerifyScheme("").Valid() {
		t.Error(`"" must not be a valid verify scheme: verify is required, and defaulting one would pick a verifier for the operator`)
	}
	if VerifyBearer.Preset() || VerifyHMACSHA256.Preset() {
		t.Error("the operator-configured schemes must not be presets")
	}
	// Every preset must supply its own signature header, or the parser would
	// fill in an empty one and the verifier would read nothing.
	for _, v := range VerifySchemes {
		if !v.Preset() {
			continue
		}
		sig, _, _, _, ok := PresetHeaders(v)
		if !ok || sig == "" {
			t.Errorf("preset %q supplies no signature header", v)
		}
	}
	// standard-webhooks reads its event name from the body, so it must NOT
	// claim an event header — the parser rejects `event_header` on it, and a
	// preset that supplied one would contradict that.
	if _, _, event, _, _ := PresetHeaders(VerifyStandardWebhooks); event != "" {
		t.Errorf("standard-webhooks supplies event header %q", event)
	}
	if !VerifyBearer.ReadsEventFromHeader() || !VerifyHMACSHA256.ReadsEventFromHeader() {
		t.Error("the operator-configured schemes read their event from a header")
	}
	if VerifyStandardWebhooks.ReadsEventFromHeader() {
		t.Error("standard-webhooks reads its event from the body")
	}
}

// TestPresetHeadersMatchTheSpec pins every preset's header set to the table in
// SPEC-0014 REQ "Webhook Verification". The delivery header is what the
// runtime de-duplicates on (REQ "Webhook Filtering"), so a preset that left it
// empty would silently turn off replay protection for every route using it —
// which is what the gitlab preset did before X-Gitlab-Event-UUID was added.
// Governing: SPEC-0014 REQ "Webhook Verification", REQ "Webhook Filtering".
//
// @joestump 09/23/2026 - Added in review of #584: gitlab had no delivery header.
func TestPresetHeadersMatchTheSpec(t *testing.T) {
	for _, tc := range []struct {
		v                                 VerifyScheme
		signature, prefix, event, deliver string
	}{
		{VerifyGitHub, "X-Hub-Signature-256", "sha256=", "X-GitHub-Event", "X-GitHub-Delivery"},
		{VerifyGitea, "X-Gitea-Signature", "", "X-Gitea-Event", "X-Gitea-Delivery"},
		{VerifyGitLab, "X-Gitlab-Token", "", "X-Gitlab-Event", "X-Gitlab-Event-UUID"},
		{VerifyStandardWebhooks, "webhook-signature", "", "", "webhook-id"},
	} {
		sig, prefix, event, delivery, ok := PresetHeaders(tc.v)
		if !ok {
			t.Errorf("%q is not a preset", tc.v)
			continue
		}
		if sig != tc.signature || prefix != tc.prefix || event != tc.event || delivery != tc.deliver {
			t.Errorf("PresetHeaders(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				tc.v, sig, prefix, event, delivery, tc.signature, tc.prefix, tc.event, tc.deliver)
		}
	}
}

// TestSecretRedacts covers every rendering the type claims to mask. The
// positive control is Reveal: if it did not return the value, the masking
// assertions would pass against a Secret that was simply empty.
// Governing: SPEC-0014 REQ "Credential Resolution".
func TestSecretRedacts(t *testing.T) {
	const value = "s3cret-Wee7ohgh-value"
	s := Secret(value)

	if s.Reveal() != value {
		t.Fatal("Reveal does not return the value, so the masking checks below prove nothing")
	}
	if s.Empty() {
		t.Error("Empty() = true for a non-empty secret")
	}
	if !Secret("").Empty() {
		t.Error("Empty() = false for an empty secret")
	}

	type holder struct {
		Token Secret
		Name  string
	}
	h := holder{Token: s, Name: "keep-me"}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	renderings := map[string]string{
		"String":       s.String(),
		"%s":           fmt.Sprintf("%s", s),
		"%v":           fmt.Sprintf("%v", s),
		"%q":           fmt.Sprintf("%q", s),
		"GoString":     s.GoString(),
		"%#v":          fmt.Sprintf("%#v", s),
		"json":         string(b),
		"%v struct":    fmt.Sprintf("%v", h),
		"%+v struct":   fmt.Sprintf("%+v", h),
		"%#v struct":   fmt.Sprintf("%#v", h),
		"nested %v":    fmt.Sprintf("%v", []holder{h}),
		"nested map":   fmt.Sprintf("%v", map[string]holder{"k": h}),
		"pointer %+v":  fmt.Sprintf("%+v", &h),
		"slice secret": fmt.Sprintf("%v", []Secret{s}),
	}
	for what, got := range renderings {
		if strings.Contains(got, value) {
			t.Errorf("%s renders the secret: %s", what, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("%s does not carry the %q mask: %s", what, redacted, got)
		}
	}
	// The mask must not swallow the rest of the struct: a redaction that
	// blanked its neighbours would make every log line useless.
	if !strings.Contains(fmt.Sprintf("%+v", h), "keep-me") {
		t.Error("redaction dropped a sibling field")
	}
}

func TestHarnessTriggered(t *testing.T) {
	cases := []struct {
		name string
		h    Harness
		want bool
	}{
		{"neither", Harness{}, false},
		{"schedule only", Harness{Schedule: "@every 1h"}, true},
		{"triggers only", Harness{Triggers: []string{"channel.sb"}}, true},
		{"both", Harness{Schedule: "@every 1h", Triggers: []string{"webhook.gh"}}, true},
		{"empty trigger list", Harness{Triggers: []string{}}, false},
	}
	for _, tc := range cases {
		if got := tc.h.Triggered(); got != tc.want {
			t.Errorf("%s: Triggered() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBoundHarnessesPreservesConfigOrder(t *testing.T) {
	cfg := &Config{
		Harnesses: map[string]Harness{
			"a": {Name: "a", Triggers: []string{"webhook.gh"}},
			"b": {Name: "b", Triggers: []string{"channel.sb"}},
			"c": {Name: "c", Triggers: []string{"webhook.gh", "channel.sb"}},
		},
		HarnessOrder: []string{"a", "b", "c"},
	}
	if got := cfg.BoundHarnesses("webhook.gh"); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Errorf("BoundHarnesses(webhook.gh) = %v, want [a c] in config order", got)
	}
	if got := cfg.BoundHarnesses("channel.sb"); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Errorf("BoundHarnesses(channel.sb) = %v", got)
	}
	if got := cfg.BoundHarnesses("webhook.nope"); len(got) != 0 {
		t.Errorf("BoundHarnesses of an unbound source = %v", got)
	}
}

func TestSourceNameGrammar(t *testing.T) {
	for _, ok := range []string{"a", "sb", "gitea-pr", "A_1", strings.Repeat("x", 64)} {
		if !SourceNameRe.MatchString(ok) {
			t.Errorf("%q should be a valid source name", ok)
		}
	}
	for _, bad := range []string{"", "-a", "_a", "a.b", "a b", "a/b", strings.Repeat("x", 65)} {
		if SourceNameRe.MatchString(bad) {
			t.Errorf("%q should not be a valid source name", bad)
		}
	}
}
