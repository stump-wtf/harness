// Credential Redaction
//
// Masks credentials in text taken from an agent transcript before harness
// shows it to anyone. Agents routinely run commands that carry a secret inline
// — `curl -H "Authorization: token …"`, `git remote set-url
// https://user:token@…`, `GITEA_TOKEN=… tea …` — and the transcript records the
// command verbatim, so `harness logs` and its --json would otherwise print the
// credential into a terminal, a scrollback buffer or a pasted bug report
// (ADR-0008).
//
// The rules are deliberately conservative. They match the shapes a credential
// reliably takes — URL userinfo, an Authorization-style header, a secret-named
// assignment or flag, basic auth handed to curl, a PEM private key, and vendor
// token prefixes — and leave alone what only refers to one: a `$VAR`, a
// `$(cat file)`, a `${VAR:-}` default, or a regex that describes credential
// URLs (every sweep on tars reads its token that way). A bare 40-hex string is
// a commit SHA far more often than a token, so it is not matched either. This
// is defence in depth for display, not a guarantee; a secret in an
// unrecognised shape passes through.
//
// Governing: ADR-0008 (secrets stay out of harness output), SPEC-0006 REQ "Run
// Correlation".
//
// @joestump-agent 09/11/2026 - Added in review of harness#307, after a pr-sweep
// transcript on tars carried two credentials from git remotes.
package redact

import "regexp"

// Mask is what a redacted value is replaced with.
const Mask = "[REDACTED]"

// rules run in order. Each keeps the label that identified the secret and swaps
// only the value. No value rule starts on "[" or "$", so a Mask already in
// place, or a reference to a secret, is never re-matched — String is
// idempotent.
var rules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// A PEM private key: the whole block, or to the end of a clipped one.
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), Mask},
	// URL userinfo with a password: scheme://user:secret@host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s/@:\[\]]+:)[^\s/@$\[][^\s/@]*@`), "${1}" + Mask + "@"},
	// A token as the whole http(s) userinfo: https://<token>@host.
	{regexp.MustCompile(`(?i)\b(https?://)[^\s/@:\[\]$]+@`), "${1}" + Mask + "@"},
	// Authorization-style headers: with a scheme word, the value after it; with
	// none, a bare value long enough not to be the scheme word itself (the
	// longest, bearer and digest, are six letters).
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*[:=]\s*(?:bearer|token|basic|digest)\s+)[^\s"',;\[$][^\s"',;]*`), "${1}" + Mask},
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*[:=]\s*)[^\s"',;\[$][^\s"',;]{7,}`), "${1}" + Mask},
	{regexp.MustCompile(`(?i)\b((?:x-)?(?:api-?key|auth-?token|access-?token|private-token|gitea-token)\s*:\s*)[^\s"',;\[$][^\s"',;@]*`), "${1}" + Mask},
	// Vendor token shapes: GitHub, GitLab, Slack, OpenAI/Anthropic-style sk-,
	// AWS access key ids, OpenBao/Vault tokens, and JWTs.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|sk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|hv[sbr]\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})\b`), Mask},
	// A secret-named assignment: GITEA_TOKEN=…, "password": "…", api_key: ….
	// The name must END in the secret word (max_tokens= is not a secret) and
	// not sit inside ${…} (a default, not a value); a value starting with "="
	// is Go's := and one starting with "-" is the next flag.
	{regexp.MustCompile(`(?i)(^|[^\w.${-])([a-z0-9_.-]*(?:token|secret|password|passwd|passphrase|api[_-]?key|access[_-]?key|private[_-]?key)"?\s*[=:]\s*)("[^"$][^"]*"|'[^'$][^']*'|[^\s"',;&=\[$-][^\s"',;&]*)`), "${1}${2}" + Mask},
	// A secret-named flag given its value as the next word.
	{regexp.MustCompile(`(?i)(\s--?(?:password|passwd|token|api-key|secret|client-secret)\s+)("[^"$][^"]*"|'[^'$][^']*'|[^\s"'\[$-]\S*)`), "${1}" + Mask},
	// Basic auth handed to curl or wget through their -u and --user flags.
	// Scoped to those tools, because docker's -u names a uid and gid.
	{regexp.MustCompile(`(\b(?:curl|wget)\b[^|;&\n]*?\s(?:-u|--user)\s+[^\s:]+:)([^\s\[$]\S*)`), "${1}" + Mask},
}

// String returns s with every recognised credential replaced by Mask.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
