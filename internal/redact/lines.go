package redact

// Line-at-a-Time Redaction
//
// String masks a PEM private key by matching from its BEGIN line to its END
// line, which only works when the whole block is in the string. The durable
// log is masked one screen row at a time — on its way to disk and again when a
// tail is served — so each call sees one line of the block: the BEGIN line is
// masked and every base64 body line after it passes through intact. That body
// IS the key. Lines carries the one piece of state a line-at-a-time caller
// loses, so a `cat id_ed25519` in a harness does not land the key on disk.
//
// Governing: ADR-0008 (secrets, as amended for #312); issue #312.
//
// @joestump 09/23/2026 - Added in review of harness#345, after a PEM body was
// shown to survive both the write-time and read-time masking line by line.

import (
	"regexp"
	"strings"
)

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// pemBody is what a line inside a key block looks like once the frame a
	// TUI draws around tool output is trimmed away: base64, or one of the
	// legacy RFC 1421 headers (Proc-Type, DEK-Info) an encrypted RSA key
	// carries before its body.
	pemBody = regexp.MustCompile(`^(?:[A-Za-z0-9+/=]+|[A-Za-z-]+: .*)$`)
)

// Lines masks a sequence of lines, one call per line, in order. The zero value
// is ready to use. It is not safe for concurrent use; give each stream its own.
type Lines struct {
	inKey bool
}

// String returns ln masked as String would mask it, and additionally masks
// every line of a PEM private key body that follows a BEGIN line.
//
// A key block ends at its END line, or at the first line that could not be
// part of one — a clipped `cat`, a TUI's "+20 lines" fold — so a key whose END
// never arrives costs a few lines of masking, never the rest of the log.
func (l *Lines) String(ln string) string {
	if l.inKey {
		if pemEnd.MatchString(ln) {
			l.inKey = false
			return String(ln)
		}
		switch body := trimFrame(ln); {
		case body == "":
			return ln
		case pemBody.MatchString(body):
			return Mask
		}
		l.inKey = false
	}
	out := String(ln)
	if loc := pemBegin.FindStringIndex(ln); loc != nil && !pemEnd.MatchString(ln[loc[1]:]) {
		l.inKey = true
	}
	return out
}

// trimFrame strips the whitespace and box-drawing a TUI puts around a line of
// tool output ("│ … │", "⎿ …"), leaving the content an agent printed.
func trimFrame(ln string) string {
	return strings.TrimFunc(ln, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '|' || r > 0x7f
	})
}
