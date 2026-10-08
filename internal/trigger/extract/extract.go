// Package extract holds the per-scheme typed-metadata table for event
// envelopes: what one field of a `typed` object comes from, which JSON paths
// answer it in priority order, and what validates it.
//
// The body of a webhook delivery is attacker-controlled by construction:
// anyone who can open a pull request can put anything in it. The table is
// therefore the ONLY way a payload byte can become a typed value: a value
// that is missing, of the wrong JSON type, or fails its validation is simply
// absent — never copied through unvalidated, truncated into shape, or
// normalized into passing. Templates render from `typed` and never from the
// raw payload, so this package is the whole boundary (REQ-8).
//
// Every pattern starts with an alphanumeric (or is numeric), so no typed
// value can begin with `-`. The property test asserts that over arbitrary
// JSON payloads.
//
// Governing: SPEC-0017 REQ-7 "Template Context", REQ-8 "Typed Event
// Metadata"; design.md § "Validation patterns", § "Typed extraction happens
// once, when the envelope is written".
//
// @joestump-agent 10/08/2026 - Added for #507.
package extract

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// namePattern validates `name` and `action` (SPEC-0017 design.md §
// "Validation patterns").
var namePattern = regexp.MustCompile(`^[a-z][a-z_]{0,63}$`)

// shaPattern is a 40-hex (SHA-1) or 64-hex (SHA-256) commit id.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// repoSegment is one path segment of a repository full name.
var repoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// metaKeyPattern and metaValuePattern are the channel `meta.<key>` rules.
var (
	metaKeyPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	metaValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// maxURLBytes bounds a typed `url` value.
const maxURLBytes = 2048

// maxNumber is the largest typed `number` (2^31 - 1): a JSON integer that
// overflows an int32 could not be a forge issue or PR number, and would be a
// value chosen to be awkward rather than an identifier.
const maxNumber = int64(1<<31 - 1)

// field is one entry of a scheme's table: the typed field it yields, the
// JSON paths that answer it in priority order, and how a value found at one
// of those paths is validated, with the whole document available for rules
// that compare against another field. The first path whose value validates
// wins.
type field struct {
	name      string
	jsonPaths []string
	validate  func(doc, v any) (any, bool)
}

// stringField builds a field whose value is a JSON string matching re.
func stringField(name string, re *regexp.Regexp, paths ...string) field {
	return field{
		name:      name,
		jsonPaths: paths,
		validate: func(_, v any) (any, bool) {
			s, ok := v.(string)
			if !ok || !re.MatchString(s) {
				return nil, false
			}
			return s, true
		},
	}
}

// bodyFields is the github/gitea table (REQ-8). `name` is answered from the
// delivery's event header — the envelope's `webhook.event` — rather than the
// body, so it is applied by FromWebhook, not by this table.
var gitHubFields = []field{
	stringField("action", namePattern, "action"),
	{"repo", []string{"repository.full_name"}, ignoreDoc(validateRepo(2))},
	{"number", []string{"pull_request.number", "issue.number", "number"}, ignoreDoc(validateNumber)},
	{"url", []string{"pull_request.html_url", "issue.html_url"}, urlMatchesHost("repository.html_url")},
	{"sha", []string{"pull_request.head.sha", "after"}, ignoreDoc(stringPattern(shaPattern))},
}

// gitLabFields is the gitlab table (REQ-8).
var gitLabFields = []field{
	{"action", []string{"object_attributes.action"}, ignoreDoc(stringPattern(namePattern))},
	{"repo", []string{"project.path_with_namespace"}, ignoreDoc(validateRepo(20))},
	{"number", []string{"object_attributes.iid"}, ignoreDoc(validateNumber)},
	{"url", []string{"object_attributes.url"}, urlMatchesHost("project.web_url")},
	{"sha", []string{"object_attributes.last_commit.id", "checkout_sha"}, ignoreDoc(stringPattern(shaPattern))},
}

// hostSourceFields are the per-scheme tables, keyed by verification scheme.
// bearer, hmac-sha256 and standard-webhooks are opaque: no body fields, only
// the envelope fields apply.
var schemeFields = map[core.VerifyScheme][]field{
	core.VerifyGitHub: gitHubFields,
	core.VerifyGitea:  gitHubFields,
	core.VerifyGitLab: gitLabFields,
}

// stringPattern builds a validator for a JSON string matching re.
func stringPattern(re *regexp.Regexp) func(any) (any, bool) {
	return func(v any) (any, bool) {
		s, ok := v.(string)
		if !ok || !re.MatchString(s) {
			return nil, false
		}
		return s, true
	}
}

// ignoreDoc adapts a single-argument validator to the field signature.
func ignoreDoc(f func(any) (any, bool)) func(doc, v any) (any, bool) {
	return func(_, v any) (any, bool) { return f(v) }
}

// validateNumber accepts a JSON integer, 1 to 2^31-1. A JSON string is
// rejected whatever it holds: `"number": "412"` is absent, not parsed
// (REQ-8 scenario "A number that is a string").
func validateNumber(v any) (any, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || i < 1 || i > maxNumber {
		return nil, false
	}
	return i, true
}

// validateRepo accepts 2 to maxSegments slash-joined segments, each
// `[A-Za-z0-9][A-Za-z0-9_.-]{0,99}`. github and gitea are exactly 2; gitlab's
// nested groups go to 20.
func validateRepo(maxSegments int) func(any) (any, bool) {
	return func(v any) (any, bool) {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		segs := strings.Split(s, "/")
		if len(segs) < 2 || len(segs) > maxSegments {
			return nil, false
		}
		for _, seg := range segs {
			if !repoSegment.MatchString(seg) {
				return nil, false
			}
		}
		return s, true
	}
}

// urlMatchesHost validates the URL field: an absolute https URL, at most
// 2048 bytes, no whitespace or control characters, whose host equals the
// host of the repository's own URL at hostPath. If the repository's URL is
// missing or unusable there is nothing to compare against, so the URL is
// absent (REQ-8 scenario "A URL on another host").
func urlMatchesHost(hostPath string) func(doc, v any) (any, bool) {
	return func(doc, v any) (any, bool) {
		s, ok := v.(string)
		if !ok || s == "" || len(s) > maxURLBytes {
			return nil, false
		}
		for i := 0; i < len(s); i++ {
			if c := s[i]; c <= ' ' || c == 0x7f {
				return nil, false
			}
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, false
		}
		repo, ok := lookup(doc, hostPath).(string)
		if !ok || repo == "" {
			return nil, false
		}
		ru, err := url.Parse(repo)
		if err != nil || ru.Host == "" || !strings.EqualFold(ru.Host, u.Host) {
			return nil, false
		}
		return s, true
	}
}

// lookup walks body along a dotted path ("pull_request.head.sha") and
// returns the value there, or nil when any step is missing or not an object.
func lookup(body any, path string) any {
	cur := body
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[seg]
	}
	return cur
}

// FromWebhook extracts the typed values a verified webhook delivery yields
// for scheme verify: `name` from the delivery's event name (the envelope's
// `webhook.event`), and the body fields the scheme's table names. For an
// opaque scheme (bearer, hmac-sha256, standard-webhooks) the body is never
// read: only `name` applies, and it is absent when the event name does not
// pass the pattern (REQ-8 scenario "Opaque sources").
//
// body may be nil or non-JSON; both yield only the fields that need no body.
// It returns nil when nothing validates, so the envelope omits `typed`
// entirely rather than carrying an empty object.
func FromWebhook(verify core.VerifyScheme, event string, body json.RawMessage) map[string]any {
	out := map[string]any{}
	if namePattern.MatchString(event) {
		out["name"] = event
	}
	fields, ok := schemeFields[verify]
	if !ok || len(body) == 0 {
		if len(out) == 0 {
			return nil
		}
		return out
	}
	var doc any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		// A non-JSON body yields nothing beyond the event name; the envelope
		// kept the raw text for the agent to read.
		if len(out) == 0 {
			return nil
		}
		return out
	}
	for _, f := range fields {
		for _, path := range f.jsonPaths {
			typed, ok := f.validate(doc, lookup(doc, path))
			if ok {
				out[f.name] = typed
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FromChannel extracts the typed values a channel notification yields: every
// `meta` key matching `^[a-z][a-z0-9_]{0,31}$` whose value is a string
// matching `^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`, exposed as
// `meta.<key>`. This is what covers Switchboard's `meta.todo_id` and
// `meta.queue`. A key that fails is absent, never passed through.
func FromChannel(meta map[string]string) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range meta {
		if !metaKeyPattern.MatchString(k) || !metaValuePattern.MatchString(v) {
			continue
		}
		out["meta."+k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// String renders a typed value the way a template context needs it: strings
// as themselves, numbers in decimal. Values of any other shape came from a
// hand-edited file rather than the table, and report false.
func String(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int64:
		return strconv.FormatInt(t, 10), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// check is a one-stop re-assertion used by the property test: v is a typed
// value the table could have produced only if it holds a string or an
// integer in range, and never begins with `-`.
func check(v any) error {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "-") {
			return fmt.Errorf("typed string %q begins with '-'", t)
		}
		return nil
	case int64:
		if t < 0 {
			return fmt.Errorf("typed integer %d is negative", t)
		}
		return nil
	}
	return fmt.Errorf("typed value %#v is neither string nor integer", v)
}
