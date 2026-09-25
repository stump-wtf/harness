package tmpl

// Governing: ADR-0023 (command one-shots and templating), SPEC-0017 REQ-10
// "Untrusted Free Text" — the fence format, nonce, control stripping and cap.
//
// The fence delimits text an outside party wrote; it does not neutralise
// prompt injection, and nothing here claims to. What it does guarantee is
// structural: the closing delimiter carries a fresh CSPRNG nonce that the
// content is checked not to contain, so no value — however it was crafted,
// whatever nonce it guesses — can close the block early and speak outside it.
//
// @joestump-agent 09/23/2026 - Added for #501.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxUntrustedBytes caps a fenced value's content, measured after control
	// characters are stripped (REQ-10).
	MaxUntrustedBytes = 4096

	// nonceBytes is 128 bits, the floor REQ-10 sets.
	nonceBytes = 16

	// maxNonceAttempts bounds regeneration. A 128-bit nonce colliding with a
	// 4 KiB value is not going to happen once, let alone this many times in a
	// row; the bound exists so a broken random source fails loudly instead of
	// spinning.
	maxNonceAttempts = 8
)

// randRead is the nonce's entropy source; tests replace it to force a
// collision and prove regeneration.
var randRead = rand.Read

// errNonce is returned when no nonce absent from the content could be drawn.
var errNonce = errors.New("tmpl: could not draw a fence nonce the content does not contain")

// Fence renders one untrusted field as a REQ-10 block:
//
//	<untrusted-data source="SOURCE" field="PATH" nonce="NONCE">
//	CONTENT
//	</untrusted-data nonce="NONCE">
//
// CONTENT is value with NUL, every C0 and C1 control except newline and tab,
// DEL and any invalid UTF-8 removed, then capped at MaxUntrustedBytes on a
// rune boundary with a trailing "[truncated N bytes]" line. When present is
// false the block is rendered with empty content, so a template's shape does
// not depend on whether the event carried the field.
func Fence(source, field, value string, present bool) (string, error) {
	content := ""
	if present {
		content = capUTF8(stripControls(value), MaxUntrustedBytes)
	}
	nonce, err := freshNonce(content)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<untrusted-data source=\"%s\" field=\"%s\" nonce=\"%s\">\n%s\n</untrusted-data nonce=\"%s\">",
		attrEscape(source), attrEscape(field), nonce, content, nonce), nil
}

// freshNonce draws a hex nonce that does not occur in content, redrawing on a
// collision (REQ-10: "regenerated if CONTENT contains it").
func freshNonce(content string) (string, error) {
	buf := make([]byte, nonceBytes)
	for range maxNonceAttempts {
		if _, err := randRead(buf); err != nil {
			return "", fmt.Errorf("tmpl: fence nonce: %w", err)
		}
		n := hex.EncodeToString(buf)
		if !strings.Contains(content, n) {
			return n, nil
		}
	}
	return "", errNonce
}

// stripControls drops NUL, C0 controls other than '\n' and '\t', DEL, C1
// controls, and bytes that are not valid UTF-8 — the last so that a raw 0x80
// to 0x9F byte cannot pass as a C1 control to a Latin-1 reader, and so the
// cap below can trust every rune boundary it finds.
func stripControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size == 1: // invalid byte
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// capUTF8 cuts valid UTF-8 s to at most limit bytes without splitting a rune,
// appending a marker that states how many bytes were dropped.
func capUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n[truncated %d bytes]", s[:cut], len(s)-cut)
}

// attrEscape makes a string safe inside a double-quoted attribute of the
// fence's opening tag. Field paths are grammar-restricted and source names are
// operator config, so this is belt and braces: neither can break the tag.
func attrEscape(s string) string {
	return attrReplacer.Replace(stripControls(s))
}

var attrReplacer = strings.NewReplacer(
	"&", "&amp;",
	`"`, "&quot;",
	"<", "&lt;",
	">", "&gt;",
	"\n", " ",
	"\t", " ",
)
