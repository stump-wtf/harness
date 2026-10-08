package extract

// Typed Extraction Tests
//
// Every fixture under testdata is a real captured delivery (or, for the push
// events, the forges' own published example for GitLab and real repository
// and commit data re-wrapped in the webhook envelope for GitHub and Gitea),
// scrubbed of anything that is not published on the forge itself. Nothing
// here is hand-written JSON: a test that builds its own fixture passes
// against broken code too (CLAUDE.md "Tests").
//
// The property test asserts the invariant REQ-8 calls out — no typed value
// begins with `-`, every value matches its pattern — over arbitrary JSON,
// and the mutation test proves the property has teeth by loosening a pattern
// and showing the check now fails.
//
// Governing: SPEC-0017 REQ-8 "Typed Event Metadata", REQ-7 "Template
// Context"; design.md § "Validation patterns".

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/quick"

	"github.com/stump-wtf/harness/internal/core"
)

// fixture loads one of testdata's real deliveries.
func fixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestFixturesRealDeliveries runs the table over every captured delivery.
// The expected values are read out of the fixture itself where the point is
// presence (repo, url), and asserted as constants where the point is the
// extraction rule (number is an integer, push has no number).
//
// REQ-8 scenario "A Gitea pull request" is the gitea-pull_request case: a
// verified Gitea pull_request for stump.wtf/harness number 412, action
// opened, yields name, action, repo, number, url and sha.
func TestFixturesRealDeliveries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		scheme core.VerifyScheme
		event  string
		check  func(t *testing.T, typed map[string]any)
	}{
		{
			"gitea pull_request", "gitea-pull_request.json", core.VerifyGitea, "pull_request",
			func(t *testing.T, typed map[string]any) {
				want := map[string]any{
					"name":   "pull_request",
					"action": "opened",
					"repo":   "stump.wtf/harness",
					"number": int64(412),
				}
				for k, v := range want {
					if typed[k] != v {
						t.Errorf("typed[%q] = %#v, want %#v", k, typed[k], v)
					}
				}
				url, _ := typed["url"].(string)
				if url != "https://gitea.stump.rocks/stump.wtf/harness/pulls/412" {
					t.Errorf("typed url = %q", url)
				}
				sha, _ := typed["sha"].(string)
				if !shaPattern.MatchString(sha) {
					t.Errorf("typed sha = %q, want a 40-hex commit id", sha)
				}
			},
		},
		{
			"github pull_request", "github-pull_request.json", core.VerifyGitHub, "pull_request",
			func(t *testing.T, typed map[string]any) {
				if typed["repo"] != "charmbracelet/crush" || typed["number"] != int64(4041) || typed["name"] != "pull_request" || typed["action"] != "opened" {
					t.Errorf("typed = %#v", typed)
				}
				if _, ok := typed["sha"].(string); !ok {
					t.Errorf("typed sha missing: %#v", typed)
				}
			},
		},
		{
			"gitea issues", "gitea-issues.json", core.VerifyGitea, "issues",
			func(t *testing.T, typed map[string]any) {
				if typed["name"] != "issues" || typed["action"] != "opened" {
					t.Errorf("typed = %#v", typed)
				}
				if n, ok := typed["number"].(int64); !ok || n < 1 {
					t.Errorf("typed number = %#v, want the issue number as an integer", typed["number"])
				}
			},
		},
		{
			"github issues", "github-issues.json", core.VerifyGitHub, "issues",
			func(t *testing.T, typed map[string]any) {
				if typed["number"] != int64(4041) || typed["repo"] != "charmbracelet/crush" {
					t.Errorf("typed = %#v", typed)
				}
			},
		},
		{
			"gitea issue_comment", "gitea-issue_comment.json", core.VerifyGitea, "issue_comment",
			func(t *testing.T, typed map[string]any) {
				if typed["name"] != "issue_comment" || typed["action"] != "created" {
					t.Errorf("typed = %#v", typed)
				}
				if n, ok := typed["number"].(int64); !ok || n != 412 {
					t.Errorf("typed number = %#v, want the commented issue's number", typed["number"])
				}
			},
		},
		{
			"github issue_comment", "github-issue_comment.json", core.VerifyGitHub, "issue_comment",
			func(t *testing.T, typed map[string]any) {
				if n, ok := typed["number"].(int64); !ok || n < 1 {
					t.Errorf("typed number = %#v, want the commented issue's number", typed["number"])
				}
			},
		},
		{
			// REQ-8: `after` is a push's head, and a push carries no number
			// at all, so {{event.number}} is unresolved for it (REQ-11).
			"gitea push", "gitea-push.json", core.VerifyGitea, "push",
			func(t *testing.T, typed map[string]any) {
				if typed["repo"] != "stump.wtf/harness" || typed["name"] != "push" {
					t.Errorf("typed = %#v", typed)
				}
				sha, _ := typed["sha"].(string)
				if sha != "725309d5959afad97ca2bba1b5d184f1c7a6a277" {
					t.Errorf("typed sha = %q, want the push's after commit", sha)
				}
				if _, ok := typed["number"]; ok {
					t.Errorf("typed number = %#v, want absent: a push has no number", typed["number"])
				}
				if _, ok := typed["url"]; ok {
					t.Errorf("typed url = %#v, want absent: a push body carries no html_url", typed["url"])
				}
			},
		},
		{
			"github push", "github-push.json", core.VerifyGitHub, "push",
			func(t *testing.T, typed map[string]any) {
				if typed["repo"] != "charmbracelet/crush" {
					t.Errorf("typed = %#v", typed)
				}
				if _, ok := typed["number"]; ok {
					t.Errorf("typed number = %#v, want absent: a push has no number", typed["number"])
				}
			},
		},
		{
			// GitLab's X-Gitlab-Event values ("Merge Request Hook") do not
			// match the name pattern, so typed.name is absent — the spec's
			// rule, applied literally. See the PR's open questions.
			"gitlab merge_request", "gitlab-merge_request.json", core.VerifyGitLab, "Merge Request Hook",
			func(t *testing.T, typed map[string]any) {
				if _, ok := typed["name"]; ok {
					t.Errorf("typed name = %#v, want absent: %q fails the name pattern", typed["name"], "Merge Request Hook")
				}
				if typed["repo"] != "gitlab-org/gitlab" || typed["number"] != int64(1) {
					t.Errorf("typed = %#v", typed)
				}
				url, _ := typed["url"].(string)
				if url != "https://gitlab.com/gitlab-org/gitlab/-/merge_requests/1" {
					t.Errorf("typed url = %q", url)
				}
			},
		},
		{
			"gitlab push", "gitlab-push.json", core.VerifyGitLab, "Push Hook",
			func(t *testing.T, typed map[string]any) {
				if typed["repo"] != "mike/diaspora" {
					t.Errorf("typed = %#v", typed)
				}
				if sha, _ := typed["sha"].(string); sha != "da1560886d4f094c3e6c9ef40349f7d38b5d27d7" {
					t.Errorf("typed sha = %q, want the push's checkout_sha", sha)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typed := FromWebhook(tc.scheme, tc.event, fixture(t, tc.file))
			tc.check(t, typed)
		})
	}
}

// TestSwitchboardDoorbell is the channel fixture: a real Switchboard
// doorbell's meta (the todo this change was queued by) yields
// meta.todo_id and meta.queue, and nothing else.
func TestSwitchboardDoorbell(t *testing.T) {
	var d struct {
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	}
	if err := json.Unmarshal(fixture(t, "switchboard-doorbell.json"), &d); err != nil {
		t.Fatal(err)
	}
	typed := FromChannel(d.Meta)
	want := map[string]any{
		"meta.todo_id": "td_982bf09a-8a1e-4f52-85bf-d647c7e94377",
		"meta.queue":   "lane-l",
	}
	if !reflect.DeepEqual(typed, want) {
		t.Errorf("typed = %#v, want %#v", typed, want)
	}
}

// TestURLonAnotherHost is REQ-8's "A URL on another host": the payload's
// pull_request.html_url names a host other than repository.html_url's, and
// typed.url is absent while every other valid field is present.
func TestURLonAnotherHost(t *testing.T) {
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(fixture(t, "gitea-pull_request.json"))))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	pr := doc["pull_request"].(map[string]any)
	pr["html_url"] = "https://evil.example.com/stump.wtf/harness/pulls/412"
	body, _ := json.Marshal(doc)
	typed := FromWebhook(core.VerifyGitea, "pull_request", body)
	if _, ok := typed["url"]; ok {
		t.Errorf("typed url = %#v, want absent for a foreign host", typed["url"])
	}
	for _, k := range []string{"name", "action", "repo", "number", "sha"} {
		if _, ok := typed[k]; !ok {
			t.Errorf("typed[%q] absent, want present: %#v", k, typed)
		}
	}
}

// TestNumberThatIsAString is REQ-8's "A number that is a string": a payload
// carrying `"number": "412; rm -rf /"` yields no typed number, and the
// string never reaches anything else either.
func TestNumberThatIsAString(t *testing.T) {
	body := json.RawMessage(`{"action":"opened","number":"412; rm -rf /","repository":{"full_name":"stump.wtf/harness","html_url":"https://gitea.stump.rocks/stump.wtf/harness"}}`)
	typed := FromWebhook(core.VerifyGitea, "pull_request", body)
	if _, ok := typed["number"]; ok {
		t.Errorf("typed number = %#v, want absent for a JSON string", typed["number"])
	}
	if typed["repo"] != "stump.wtf/harness" {
		t.Errorf("typed = %#v, want the valid fields present", typed)
	}
}

// TestDashStartingMetaValue is REQ-8's "A value starting with a dash": a
// channel notification carrying meta.todo_id = "--help" yields no
// typed.meta.todo_id.
func TestDashStartingMetaValue(t *testing.T) {
	typed := FromChannel(map[string]string{"todo_id": "--help", "queue": "lane-l"})
	want := map[string]any{"meta.queue": "lane-l"}
	if !reflect.DeepEqual(typed, want) {
		t.Errorf("typed = %#v, want %#v", typed, want)
	}
}

// TestOpaqueSources is REQ-8's "Opaque sources": a bearer source's JSON body
// containing `repository` yields no typed body fields — only the envelope
// fields (the name, from the delivery's event name) apply.
func TestOpaqueSources(t *testing.T) {
	body := json.RawMessage(`{"repository":{"full_name":"stump.wtf/harness"},"action":"opened"}`)
	for _, scheme := range []core.VerifyScheme{core.VerifyBearer, core.VerifyHMACSHA256, core.VerifyStandardWebhooks} {
		typed := FromWebhook(scheme, "deploy", body)
		want := map[string]any{"name": "deploy"}
		if !reflect.DeepEqual(typed, want) {
			t.Errorf("%s: typed = %#v, want %#v", scheme, typed, want)
		}
	}
	// An event name that fails the pattern is absent too.
	if typed := FromWebhook(core.VerifyBearer, "Deploy!", body); len(typed) != 0 {
		t.Errorf("typed = %#v, want nil for an invalid event name", typed)
	}
}

// TestMissingAndMalformed: no body, non-JSON body, wrong types, out-of-range
// numbers, overlong repos and non-https URLs are all absent, never coerced.
func TestMissingAndMalformed(t *testing.T) {
	if typed := FromWebhook(core.VerifyGitHub, "push", nil); !reflect.DeepEqual(typed, map[string]any{"name": "push"}) {
		t.Errorf("nil body: typed = %#v", typed)
	}
	if typed := FromWebhook(core.VerifyGitHub, "push", json.RawMessage("not json")); !reflect.DeepEqual(typed, map[string]any{"name": "push"}) {
		t.Errorf("non-JSON body: typed = %#v", typed)
	}
	for _, body := range []string{
		`{"action": 7}`, // action not a string
		`{"repository": {"full_name": "only-one"}}`, // one segment
		`{"repository": {"full_name": "a/b/c/d"}}`,  // three segments on a two-segment scheme
		`{"number": 0}`,          // zero
		`{"number": 4294967296}`, // over int32
		`{"number": 1.5}`,        // fraction
		`{"after": "HEAD"}`,      // not a sha
		`{"after": "ZZ90bf891e76fee5e1747ab589903a6a1f80f22"}`,
		`{"pull_request": {"html_url": "http://gitea.stump.rocks/x"}}`,                                   // not https
		`{"pull_request": {"html_url": "https://gitea.stump.rocks/x"}, "repository": {"html_url": ""}},`, // no repo host to compare
	} {
		typed := FromWebhook(core.VerifyGitHub, "push", json.RawMessage(body))
		for _, k := range []string{"action", "repo", "number", "url", "sha"} {
			if _, ok := typed[k]; ok {
				t.Errorf("body %s: typed[%q] = %#v, want absent", body, k, typed[k])
			}
		}
	}
}

// fieldPattern is the pattern each typed field must match once extracted:
// the same table the extraction used, stated as the property the property
// test asserts. A value that fails it, or begins with `-`, is a violation.
func fieldPattern(field string) (*regexp.Regexp, bool) {
	switch field {
	case "name", "action":
		return namePattern, true
	case "repo":
		return nil, true // checked segment-wise by validateRepo
	case "number":
		return nil, true // integer, checked by validateNumber
	case "url":
		return nil, true // https and host-matched, checked by urlMatchesHost
	case "sha":
		return shaPattern, true
	}
	if len(field) > 5 && field[:5] == "meta." {
		return metaValuePattern, true
	}
	return nil, false
}

// checkTyped is the property: every value the table yields is a string or an
// in-range integer, no value begins with `-`, and every string matches its
// field's pattern. This is what the quick test runs on arbitrary payloads.
func checkTyped(typed map[string]any) error {
	for k, v := range typed {
		if err := check(v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		re, known := fieldPattern(k)
		if !known {
			return fmt.Errorf("unknown typed field %q", k)
		}
		if re != nil && !re.MatchString(s) {
			return fmt.Errorf("typed %q = %q fails its pattern", k, s)
		}
		if k == "repo" {
			segs := splitRepo(s)
			if len(segs) < 2 || len(segs) > 20 {
				return fmt.Errorf("typed repo %q has %d segments", s, len(segs))
			}
			for _, seg := range segs {
				if !repoSegment.MatchString(seg) {
					return fmt.Errorf("typed repo segment %q fails its pattern", seg)
				}
			}
		}
	}
	return nil
}

func splitRepo(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// TestPropertyTypedValuesOverArbitraryJSON is REQ-8's property test: for
// arbitrary JSON payloads (arbitrary strings and integers at the top level
// and inside repository/pull_request/issue, plus arbitrary event names and
// channel meta), no extracted value begins with `-` and every value matches
// its pattern.
//
// Governing: SPEC-0017 REQ-8, issue #507 acceptance criteria.
func TestPropertyTypedValuesOverArbitraryJSON(t *testing.T) {
	f := func(top map[string]string, repo map[string]string, pr map[string]string, nums map[string]int32, event string, meta map[string]string) bool {
		doc := map[string]any{}
		for k, v := range top {
			doc[k] = v
		}
		for k, v := range nums {
			doc[k] = v
		}
		doc["repository"] = map[string]any{}
		for k, v := range repo {
			doc["repository"].(map[string]any)[k] = v
		}
		doc["pull_request"] = map[string]any{}
		for k, v := range pr {
			doc["pull_request"].(map[string]any)[k] = v
		}
		body, _ := json.Marshal(doc)
		for _, scheme := range []core.VerifyScheme{core.VerifyGitHub, core.VerifyGitea, core.VerifyGitLab} {
			if err := checkTyped(FromWebhook(scheme, event, body)); err != nil {
				t.Errorf("scheme %s, body %s: %v", scheme, body, err)
				return false
			}
		}
		if err := checkTyped(FromChannel(meta)); err != nil {
			t.Errorf("meta %v: %v", meta, err)
			return false
		}
		return true
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
}

// TestPropertyFailsWhenAPatternIsLoosened is the mutation check the issue
// demands: loosen one pattern — here namePattern, to admit a leading `-` —
// and the property check over a payload carrying such a value now FAILS.
// A property test that still passed would prove nothing.
//
// Governing: SPEC-0017 REQ-8, issue #507 acceptance criteria.
func TestPropertyFailsWhenAPatternIsLoosened(t *testing.T) {
	loosened := regexp.MustCompile(`^[a-z_-][a-z_-]{0,63}$`)
	old := namePattern
	namePattern = loosened
	defer func() { namePattern = old }()

	body := json.RawMessage(`{"repository":{"full_name":"stump.wtf/harness","html_url":"https://gitea.stump.rocks/stump.wtf/harness"}}`)
	typed := FromWebhook(core.VerifyGitea, "-pull_request", body)
	if _, ok := typed["name"]; !ok {
		t.Fatalf("the loosened pattern did not admit the value; mutation check is not exercising the property")
	}
	if err := checkTyped(typed); err == nil {
		t.Fatal("the property check passed on values a loosened pattern admitted; the test proves nothing")
	}
}

// TestString renders typed values the way a template context needs.
func TestString(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
		ok   bool
	}{
		{"pull_request", "pull_request", true},
		{int64(412), "412", true},
		{json.Number("412"), "412", true},
		{1.5, "", false},
		{nil, "", false},
	} {
		got, ok := String(tc.v)
		if got != tc.want || ok != tc.ok {
			t.Errorf("String(%#v) = %q,%v want %q,%v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
}
