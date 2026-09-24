package webhook

// Tests for the `hmac-sha256` scheme and the `github`, `gitea` and `gitlab`
// presets.
//
// Every expected MAC below is a literal computed OUTSIDE this code — GitHub's
// documented example, RFC 4231 test case 2, and `openssl dgst -sha256 -hmac`
// for the Gitea body — never by signing with crypto/hmac in the test. A test
// that signs with the same primitive the verifier uses and then verifies
// proves only that the code agrees with itself.
//
// Every source is built by config.Load from a real `[webhook.*]` table, so the
// preset headers under test are the ones the parser fills in, not ones this
// file assumed.
//
// Governing: SPEC-0014 REQ "Webhook Verification", REQ "Webhook Source Table",
// REQ "Event Delivery To The Run".

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
)

// vector is one scheme's known-answer case.
type vector struct {
	scheme string
	// table is the [webhook.<name>] body after verify/env_file/secret.
	table string
	// secret is the env file's S= value, quoted as the env file needs.
	secret string
	body   string
	// hdr authenticates body.
	hdr map[string]string
	// sig is the header carrying the signature or token, and good its value.
	sig, good string
}

// Known answers.
const (
	// GitHub's documented example ("Validating webhook deliveries"): secret
	// "It's a Secret to Everybody", payload "Hello, World!".
	githubSecret = "It's a Secret to Everybody"
	githubBody   = "Hello, World!"
	githubSig    = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"

	// RFC 4231 §4.3, test case 2: key "Jefe".
	rfcKey  = "Jefe"
	rfcBody = "what do ya want for nothing?"
	rfcMAC  = "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"

	// printf '%s' "$giteaBody" | openssl dgst -sha256 -hmac "$giteaSecret".
	// The body is JSON that does NOT survive a decode/re-encode unchanged
	// (the spaces), so verifying anything but the raw bytes fails it.
	giteaSecret = "gitea-hook-secret-Uu3ahx"
	giteaBody   = `{"action":"opened", "number": 7}`
	giteaSig    = "0e42c3ad49b50f79b457e005437e457bcf7b34d25ff9e0a5bba0c2906616c3f8"

	gitlabToken = "gitlab-token-Eeph5ooz"
)

func vectors() []vector {
	return []vector{
		{
			scheme: "hmac-sha256", table: "signature_header = \"X-Signature\"\nevent_header = \"X-Event\"\ndelivery_header = \"X-Delivery\"\n",
			secret: rfcKey, body: rfcBody,
			hdr: map[string]string{"X-Signature": rfcMAC, "X-Event": "build", "X-Delivery": "d-hmac-1"},
			sig: "X-Signature", good: rfcMAC,
		},
		{
			scheme: "hmac-sha256", table: "signature_header = \"X-Signature\"\nsignature_prefix = \"v1=\"\n",
			secret: rfcKey, body: rfcBody,
			hdr: map[string]string{"X-Signature": "v1=" + rfcMAC},
			sig: "X-Signature", good: "v1=" + rfcMAC,
		},
		{
			scheme: "github", secret: githubSecret, body: githubBody,
			hdr: map[string]string{"X-Hub-Signature-256": githubSig, "X-GitHub-Event": "push", "X-GitHub-Delivery": "72d3162e-cc78-11e3-81ab-4c9367dc0958"},
			sig: "X-Hub-Signature-256", good: githubSig,
		},
		{
			scheme: "gitea", secret: giteaSecret, body: giteaBody,
			hdr: map[string]string{"X-Gitea-Signature": giteaSig, "X-Gitea-Event": "pull_request", "X-Gitea-Delivery": "d-gitea-1"},
			sig: "X-Gitea-Signature", good: giteaSig,
		},
		{
			scheme: "gitlab", secret: gitlabToken, body: `{"object_kind":"push"}`,
			hdr: map[string]string{"X-Gitlab-Token": gitlabToken, "X-Gitlab-Event": "Push Hook", "X-Gitlab-Event-UUID": "d-gitlab-1"},
			sig: "X-Gitlab-Token", good: gitlabToken,
		},
	}
}

// loadSource parses a real [webhook.<name>] table and returns the source the
// daemon would serve.
func loadSource(t *testing.T, name, scheme, secret, extra string) core.WebhookSource {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.env"), []byte("S=\""+secret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toml := "[webhook." + name + "]\nverify = \"" + scheme + "\"\nenv_file = \"s.env\"\nsecret = \"${S}\"\n" + extra
	p := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("load %s table: %v", scheme, err)
	}
	src, ok := cfg.Webhooks[name]
	if !ok {
		t.Fatalf("no webhook.%s after load", name)
	}
	if string(src.Secret.Reveal()) != secret {
		t.Fatalf("%s: the env file did not resolve to the vector's secret", scheme)
	}
	return src
}

func headerOf(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}

// TestKnownAnswerVectors: each scheme accepts the independently computed
// signature for its body, and refuses the same signature once one body byte
// changes, once one signature digit changes, and without the header at all.
func TestKnownAnswerVectors(t *testing.T) {
	for _, vec := range vectors() {
		t.Run(vec.scheme+"/"+vec.sig, func(t *testing.T) {
			v, err := NewVerifier(loadSource(t, "r", vec.scheme, vec.secret, vec.table))
			if err != nil {
				t.Fatalf("NewVerifier: %v", err)
			}
			if err := v.Verify(headerOf(vec.hdr), []byte(vec.body)); err != nil {
				t.Fatalf("the known-good vector is refused: %v", err)
			}

			altered := []byte(vec.body)
			altered[len(altered)-1] ^= 0x01
			if vec.scheme == "gitlab" {
				// A token does not cover the body; one byte of change is
				// still a pass, which is exactly what GitLab offers.
				if err := v.Verify(headerOf(vec.hdr), altered); err != nil {
					t.Errorf("gitlab refused a body change it never signed: %v", err)
				}
			} else if err := v.Verify(headerOf(vec.hdr), altered); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("one body byte changed: Verify = %v, want ErrUnauthorized", err)
			}

			flipped := headerOf(vec.hdr)
			last := vec.good[len(vec.good)-1]
			swap := byte('0')
			if last == '0' {
				swap = '1'
			}
			flipped.Set(vec.sig, vec.good[:len(vec.good)-1]+string(swap))
			if err := v.Verify(flipped, []byte(vec.body)); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("one signature digit changed: Verify = %v, want ErrUnauthorized", err)
			}

			missing := headerOf(vec.hdr)
			missing.Del(vec.sig)
			if err := v.Verify(missing, []byte(vec.body)); !errors.Is(err, ErrUnauthorized) {
				t.Errorf("no %s: Verify = %v, want ErrUnauthorized", vec.sig, err)
			}
		})
	}
}

// TestHMACRefusesMalformedSignatures: every shape short of a correct MAC in
// the right header is refused, and no refusal echoes what was presented.
func TestHMACRefusesMalformedSignatures(t *testing.T) {
	gh, err := NewVerifier(loadSource(t, "gh", "github", githubSecret, ""))
	if err != nil {
		t.Fatal(err)
	}
	hex := strings.TrimPrefix(githubSig, "sha256=")
	cases := []struct {
		name   string
		values []string
		other  map[string]string
		ok     bool
	}{
		{"exact", []string{githubSig}, nil, true},
		{"uppercase hex", []string{"sha256=" + strings.ToUpper(hex)}, nil, true},
		{"missing", nil, nil, false},
		{"empty", []string{""}, nil, false},
		{"prefix only", []string{"sha256="}, nil, false},
		{"no prefix", []string{hex}, nil, false},
		{"wrong prefix", []string{"sha1=" + hex}, nil, false},
		{"uppercase prefix", []string{"SHA256=" + hex}, nil, false},
		{"not hex", []string{"sha256=" + strings.Repeat("zz", 32)}, nil, false},
		{"short", []string{githubSig[:len(githubSig)-2]}, nil, false},
		{"long", []string{githubSig + "00"}, nil, false},
		{"base64 instead of hex", []string{"sha256=dXEH6g6yUJ/CESIczphLijdXC211hsIsRs9DefyLBD4="}, nil, false},
		{"two headers, both right", []string{githubSig, githubSig}, nil, false},
		{"two headers, one right", []string{"sha256=" + strings.Repeat("0", 64), githubSig}, nil, false},
		// Right MAC, wrong header: GitHub's legacy SHA-1 header and Gitea's.
		{"legacy header", nil, map[string]string{"X-Hub-Signature": githubSig}, false},
		{"gitea header", nil, map[string]string{"X-Gitea-Signature": hex}, false},
	}
	for _, c := range cases {
		h := http.Header{}
		for _, v := range c.values {
			h.Add("X-Hub-Signature-256", v)
		}
		for k, v := range c.other {
			h.Set(k, v)
		}
		err := gh.Verify(h, []byte(githubBody))
		if (err == nil) != c.ok {
			t.Errorf("%s: Verify = %v, want ok=%v", c.name, err, c.ok)
			continue
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: %v does not wrap ErrUnauthorized", c.name, err)
		}
		if strings.Contains(err.Error(), hex[:16]) || strings.Contains(err.Error(), "zz") || strings.Contains(err.Error(), githubSecret) {
			t.Errorf("%s: error %q echoes a presented value or the secret", c.name, err)
		}
	}
}

// TestGitLabTokenIsExact: the token must be the secret, byte for byte, in
// exactly one X-Gitlab-Token header.
func TestGitLabTokenIsExact(t *testing.T) {
	gl, err := NewVerifier(loadSource(t, "gl", "gitlab", gitlabToken, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		values []string
		ok     bool
	}{
		{"exact", []string{gitlabToken}, true},
		{"missing", nil, false},
		{"empty", []string{""}, false},
		{"prefix of the token", []string{gitlabToken[:len(gitlabToken)-1]}, false},
		{"token plus a byte", []string{gitlabToken + "x"}, false},
		{"case changed", []string{strings.ToUpper(gitlabToken)}, false},
		{"as a bearer", []string{"Bearer " + gitlabToken}, false},
		{"two headers", []string{gitlabToken, gitlabToken}, false},
	} {
		h := http.Header{}
		for _, v := range c.values {
			h.Add("X-Gitlab-Token", v)
		}
		err := gl.Verify(h, nil)
		if (err == nil) != c.ok {
			t.Errorf("%s: Verify = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), gitlabToken[7:]) {
			t.Errorf("%s: error %q echoes the token", c.name, err)
		}
	}
	// And a Bearer header carrying the right token is not a GitLab token.
	h := http.Header{}
	h.Set("Authorization", "Bearer "+gitlabToken)
	if err := gl.Verify(h, nil); err == nil {
		t.Error("gitlab accepted the token in Authorization")
	}
}

// TestPresetRefusesADisagreeingSource: a preset source whose signature header
// is not the preset's gets a route that refuses everything, never a verifier
// reading a header the config does not name.
func TestPresetRefusesADisagreeingSource(t *testing.T) {
	src := loadSource(t, "gh", "github", githubSecret, "")
	src.SignatureHeader = "X-Other"
	if _, err := NewVerifier(src); !errors.Is(err, ErrSchemeUnavailable) {
		t.Errorf("github with a foreign signature header: %v, want ErrSchemeUnavailable", err)
	}
	hm := loadSource(t, "hm", "hmac-sha256", rfcKey, "signature_header = \"X-Signature\"\n")
	hm.SignatureHeader = ""
	if _, err := NewVerifier(hm); !errors.Is(err, ErrSchemeUnavailable) {
		t.Errorf("hmac-sha256 without a signature header: %v, want ErrSchemeUnavailable", err)
	}
}

// TestSchemeDeliveriesOverTheListener runs each vector through a real bound
// listener: the signed body is a 202 that fires exactly once, with the body
// verbatim and the event and delivery from the scheme's headers; the same
// signature over a one-byte-different body is a 401 that fires nothing. The
// signature or token never reaches the event or the log.
func TestSchemeDeliveriesOverTheListener(t *testing.T) {
	for _, vec := range vectors() {
		t.Run(vec.scheme+"/"+vec.sig, func(t *testing.T) {
			src := loadSource(t, "r", vec.scheme, vec.secret, vec.table)
			f := &fakeFirer{}
			srv, logs := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src}, "r"))
			url := "http://" + srv.Addr() + "/hooks/r"

			hdr := map[string]string{"Content-Type": "application/json", "User-Agent": "vector/1"}
			for k, v := range vec.hdr {
				hdr[k] = v
			}

			if vec.scheme != "gitlab" {
				altered := []byte(vec.body)
				altered[0] ^= 0x01
				resp, body := do(t, "POST", url, hdr, string(altered))
				if resp.StatusCode != http.StatusUnauthorized || !bytes.Equal(body, errorBodies[errUnauthorized]) {
					t.Fatalf("altered body: %d %q, want the uniform 401", resp.StatusCode, body)
				}
				if n := len(f.fired()); n != 0 {
					t.Fatalf("an altered body fired %d time(s)", n)
				}
			}
			bad := map[string]string{}
			for k, v := range hdr {
				bad[k] = v
			}
			bad[vec.sig] = strings.Repeat("f", len(vec.good))
			if resp, _ := do(t, "POST", url, bad, vec.body); resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("wrong signature: %d, want 401", resp.StatusCode)
			}
			if n := len(f.fired()); n != 0 {
				t.Fatalf("a wrong signature fired %d time(s)", n)
			}

			resp, body := do(t, "POST", url, hdr, vec.body)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("signed delivery: %d %s", resp.StatusCode, body)
			}
			evs := f.fired()
			if len(evs) != 1 {
				t.Fatalf("signed delivery fired %d times, want 1", len(evs))
			}
			ev := evs[0]
			// A JSON body lands in `body`, anything else in `body_text`;
			// either way, verbatim.
			if got := string(ev.Webhook.Body) + ev.Webhook.BodyText; got != vec.body {
				t.Errorf("body = %q, want it verbatim %q", got, vec.body)
			}
			if src.EventHeader != "" && ev.Webhook.Event != vec.hdr[src.EventHeader] {
				t.Errorf("event = %q, want %q from %s", ev.Webhook.Event, vec.hdr[src.EventHeader], src.EventHeader)
			}
			if src.DeliveryHeader != "" {
				want := vec.hdr[src.DeliveryHeader]
				if ev.Webhook.Delivery != want || ev.EventID != want {
					t.Errorf("delivery/event_id = %q/%q, want %q from %s", ev.Webhook.Delivery, ev.EventID, want, src.DeliveryHeader)
				}
			}

			// webhook.headers is Content-Type, User-Agent and the event and
			// delivery headers — nothing else, and never the signature.
			allowed := map[string]bool{"Content-Type": true, "User-Agent": true}
			if src.EventHeader != "" {
				allowed[src.EventHeader] = true
			}
			if src.DeliveryHeader != "" {
				allowed[src.DeliveryHeader] = true
			}
			if len(ev.Webhook.Headers) != len(allowed) {
				t.Errorf("headers = %v, want exactly %v", ev.Webhook.Headers, allowed)
			}
			for k := range ev.Webhook.Headers {
				if !allowed[k] {
					t.Errorf("header %q reached the event", k)
				}
			}
			enc, err := ev.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(enc), vec.good) || strings.Contains(strings.ToLower(string(enc)), strings.ToLower(vec.sig)) {
				t.Errorf("the event carries the %s header or its value:\n%s", vec.sig, enc)
			}
			if strings.Contains(string(enc), vec.secret) {
				t.Errorf("the event carries the secret:\n%s", enc)
			}

			out := logs.String()
			if !strings.Contains(out, "webhook.r") || !strings.Contains(out, "127.0.0.1:") {
				t.Errorf("the refusal log names neither route nor peer:\n%s", out)
			}
			for _, leak := range []string{vec.good, strings.Repeat("f", len(vec.good)), vec.secret} {
				if strings.Contains(out, leak) {
					t.Errorf("the log carries %q:\n%s", leak, out)
				}
			}
		})
	}
}

// TestVerifiesTheRawBytes: a body that decodes to the same JSON as the signed
// one, but is not the same bytes, is refused. A verifier handed a parsed and
// re-encoded body would pass the compacted form and refuse the signed one.
func TestVerifiesTheRawBytes(t *testing.T) {
	src := loadSource(t, "gt", "gitea", giteaSecret, "")
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src}, "gt"))
	url := "http://" + srv.Addr() + "/hooks/gt"
	hdr := map[string]string{"Content-Type": "application/json", "X-Gitea-Signature": giteaSig}

	var v any
	if err := json.Unmarshal([]byte(giteaBody), &v); err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(v)
	if string(compact) == giteaBody {
		t.Fatal("fixture: the signed body must not survive a re-encode unchanged")
	}
	if resp, _ := do(t, "POST", url, hdr, string(compact)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the re-encoded body: %d, want 401", resp.StatusCode)
	}
	if resp, body := do(t, "POST", url, hdr, giteaBody); resp.StatusCode != http.StatusAccepted {
		t.Errorf("the signed bytes: %d %s, want 202", resp.StatusCode, body)
	}
	if n := len(f.fired()); n != 1 {
		t.Errorf("fired %d times, want 1 (the signed bytes only)", n)
	}
}
