package remote

// Governing: ADR-0008 (remote access is SSH public-key auth only — an
// authorized-keys allowlist; no passwords, no bearer tokens; optional per-key
// read-only scoping). SPEC-0002 (Transport Bindings). This file resolves the
// configured allowlist (inline keys + an optional authorized_keys file) into a
// set of public keys, each tagged read-only or read-write, and answers the
// "may this key attach, and how?" question the SSH auth handler asks.

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/ssh"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// authorizedKey is one entry of the resolved allowlist: a parsed SSH public key
// and whether a session authenticating with it may only open read-only attaches
// (ADR-0008 per-key read-only scoping).
type authorizedKey struct {
	key      ssh.PublicKey
	readOnly bool
}

// allowlist is the resolved set of keys permitted to attach. An empty allowlist
// authorizes nobody — there are never unauthenticated sessions (ADR-0008).
type allowlist []authorizedKey

// authorize reports whether key is on the allowlist and, if so, whether it is
// restricted to read-only attaches. A key absent from the list is refused.
func (a allowlist) authorize(key ssh.PublicKey) (readOnly bool, ok bool) {
	for _, e := range a {
		if ssh.KeysEqual(e.key, key) {
			return e.readOnly, true
		}
	}
	return false, false
}

// loadAllowlist resolves the configured inline keys plus an optional
// authorized_keys file into a single allowlist. Inline keys carry their
// read-only flag from config ([[server.key]] read_only); file entries are
// read-only when their line carries a leading `read-only` option
// (`read-only ssh-ed25519 AAAA… comment`), mirroring an OpenSSH-style
// authorized_keys annotation. A nil/empty result with no error means the
// caller configured an empty allowlist — the caller must reject enabling the
// server in that case.
func loadAllowlist(keys []core.AuthorizedKey, file string) (allowlist, error) {
	var out allowlist
	for _, k := range keys {
		line := strings.TrimSpace(k.Line)
		if line == "" {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("remote: parse authorized key %q: %w", firstField(line), err)
		}
		out = append(out, authorizedKey{key: pk, readOnly: k.ReadOnly})
	}
	if strings.TrimSpace(file) != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("remote: read authorized_keys_file: %w", err)
		}
		fileKeys, err := parseAuthorizedKeysBlob(data)
		if err != nil {
			return nil, err
		}
		out = append(out, fileKeys...)
	}
	return out, nil
}

// parseAuthorizedKeysBlob parses an OpenSSH authorized_keys file body into
// allowlist entries. Blank lines and `#` comments are skipped; an entry is
// read-only when its options include the `read-only` token (ADR-0008).
func parseAuthorizedKeysBlob(data []byte) (allowlist, error) {
	var out allowlist
	rest := data
	for {
		rest = bytes.TrimLeft(rest, "\r\n\t ")
		if len(rest) == 0 {
			break
		}
		// Skip comment lines: ParseAuthorizedKey handles them but returns the
		// next real key, which would drop the comment's line accounting — do it
		// explicitly so a file of only comments terminates cleanly.
		if rest[0] == '#' {
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				rest = rest[i+1:]
				continue
			}
			break
		}
		pk, _, options, r, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			return nil, fmt.Errorf("remote: parse authorized_keys file: %w", err)
		}
		out = append(out, authorizedKey{key: pk, readOnly: hasReadOnlyOption(options)})
		if len(r) == len(rest) {
			// No forward progress (defensive): stop rather than spin.
			break
		}
		rest = r
	}
	return out, nil
}

// hasReadOnlyOption reports whether the authorized_keys option list marks the
// entry read-only. Accepts `read-only` and `read_only` spellings.
func hasReadOnlyOption(options []string) bool {
	for _, o := range options {
		switch strings.ToLower(strings.TrimSpace(o)) {
		case "read-only", "read_only", "readonly":
			return true
		}
	}
	return false
}

// firstField returns the first whitespace-delimited token of s, for terse error
// messages that never echo a whole key line.
func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}
