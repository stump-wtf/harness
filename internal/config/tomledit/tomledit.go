// Package tomledit is a comment-preserving, atomic editor for the two table
// families SPEC-0026 commands write: [stable.<name>] and harness tables
// ([harness.<name>] or the bare [<name>] spelling the loader accepts). The
// daemon never edits config on its own; this package exists for the
// `harness agent` CLI tree (ADR-0044).
//
// It operates on the file's bytes with TOML-aware header detection — a "["
// inside a multi-line array or multi-line string is not a header — so every
// key outside the edited table keeps its line, order and comments. Writes are
// atomic (temp file + fsync + rename, mode preserved); a failed write leaves
// the original byte-identical.
//
// The API is deliberately narrow: there is no "any header" entry point, so no
// SPEC-0026 command can write [mcp.*], [job.*], [server], [profile.*],
// [adapter.*] or [skill_repo.*] — the structural half of REQ-11, which #813
// checks end to end.
//
// Governing: ADR-0044; SPEC-0026 REQ-1, REQ-6, REQ-8, REQ-9, REQ-11.
package tomledit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrTableNotFound names a table the caller expected to be present.
var ErrTableNotFound = errors.New("table not found")

// ErrKeyNotFound names a key the caller expected to be present.
var ErrKeyNotFound = errors.New("key not found")

// ErrDuplicateTable names a table that appears more than once; editing it in
// place would be ambiguous.
var ErrDuplicateTable = errors.New("duplicate table")

// ErrArrayTable names a table spelled [[...]] (an array-of-tables entry),
// which this editor never touches.
var ErrArrayTable = errors.New("array table")

// Editor holds the file bytes being edited. Create with Load or New.
type Editor struct {
	data []byte
	mode os.FileMode
	// set when loaded from disk, for mode preservation and rename target.
	path string
}

// New wraps existing harness.toml bytes.
func New(data []byte) *Editor { return &Editor{data: data, mode: 0o644} }

// Load reads path into an Editor, remembering its mode for the atomic write.
func Load(path string) (*Editor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tomledit: read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return &Editor{data: data, mode: mode, path: path}, nil
}

// Bytes returns the edited content.
func (e *Editor) Bytes() []byte { return e.data }

// Save writes atomically: temp file in the same directory, fsync, rename over
// the target, mode preserved. On any failure the original file is untouched.
func (e *Editor) Save(path string) error {
	if path == "" {
		path = e.path
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tomledit-*")
	if err != nil {
		return fmt.Errorf("tomledit: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(e.mode); err != nil {
		tmp.Close()
		return fmt.Errorf("tomledit: chmod temp: %w", err)
	}
	if _, err := tmp.Write(e.data); err != nil {
		tmp.Close()
		return fmt.Errorf("tomledit: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("tomledit: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tomledit: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("tomledit: rename into place: %w", err)
	}
	tmpName = "" // renamed; nothing to clean up
	return nil
}

// header is one table header occurrence: its byte offset and the offset just
// past the header line (the table body starts there).
type header struct {
	name      string // canonical dotted name, e.g. "harness.pr" or "stable.acme"
	start     int    // offset of the '['
	bodyStart int    // offset of the first byte after the header line
	array     bool   // spelled [[...]]
}

// headerName normalizes a header's literal name: trimmed, and with the
// surrounding brackets stripped by the scanner.
func canonicalName(literal string) string {
	return strings.TrimSpace(literal)
}

// findHeaders walks the file once and returns every table header occurrence.
// The scan is TOML-aware at the whole-file level: multi-line strings are
// skipped as wholes, and bracket/brace nesting outside strings and comments
// is tracked, so a '[' inside a multi-line string or array is never mistaken
// for a header (the bug removeHarnessTOML had). A nested array's last
// element ([[1, 2]] or []) is shaped exactly like a table header and can
// only be told apart by the nesting level; on malformed input whose brackets
// never balance the scan stops finding headers at all, which makes edits
// fail closed with ErrTableNotFound instead of corrupting the file.
func findHeaders(data []byte) []header {
	var headers []header
	i := 0
	// Bracket and brace nesting outside strings and comments. Header
	// detection is suppressed while it is positive.
	depth := 0
	n := len(data)
	for i < n {
		// Multi-line basic string.
		if bytes.HasPrefix(data[i:], []byte(`"""`)) {
			end := bytes.Index(data[i+3:], []byte(`"""`))
			if end < 0 {
				break
			}
			i = i + 3 + end + 3
			continue
		}
		// Multi-line literal string.
		if bytes.HasPrefix(data[i:], []byte("'''")) {
			end := bytes.Index(data[i+3:], []byte("'''"))
			if end < 0 {
				break
			}
			i = i + 3 + end + 3
			continue
		}
		switch data[i] {
		case '"': // single-line basic string
			j := i + 1
			for j < n {
				if data[j] == '\\' {
					j += 2
					continue
				}
				if data[j] == '"' || data[j] == '\n' {
					break
				}
				j++
			}
			i = j + 1
			continue
		case '\'': // single-line literal string
			j := bytes.IndexByte(data[i+1:], '\n')
			if j < 0 {
				break
			}
			i = i + 1 + j
			continue
		case '[':
			if depth > 0 {
				depth++
				i++
				continue
			}
			if lineStartsAt(data, i) {
				h, ok := parseHeader(data, i)
				if ok {
					headers = append(headers, h)
					i = h.bodyStart
					continue
				}
			}
			depth++
			i++
			continue
		case ']':
			if depth > 0 {
				depth--
			}
			i++
			continue
		case '{':
			depth++
			i++
			continue
		case '}':
			if depth > 0 {
				depth--
			}
			i++
			continue
		case '#': // comment to end of line
			j := bytes.IndexByte(data[i:], '\n')
			if j < 0 {
				break
			}
			i += j
			continue
		}
		i++
	}
	return headers
}

// lineStartsAt reports whether data[i] is the first non-space byte of its
// line.
func lineStartsAt(data []byte, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch data[j] {
		case ' ', '\t':
			continue
		case '\n':
			return true
		default:
			return false
		}
	}
	return true
}

// parseHeader reads a header starting at data[i] == '['. It returns ok=false
// for anything that is not a well-formed header line (e.g. a line-continuation
// artifact), leaving the caller to advance one byte.
func parseHeader(data []byte, i int) (header, bool) {
	n := len(data)
	array := false
	if i+1 < n && data[i+1] == '[' {
		array = true
	}
	open := i
	if array {
		open = i + 1
	}
	// Find the closing bracket on this same logical line.
	close := bytes.IndexByte(data[open:], ']')
	if close < 0 {
		return header{}, false
	}
	close += open
	if array {
		if close+1 >= n || data[close+1] != ']' {
			return header{}, false
		}
	}
	nameEnd := close
	if array {
		nameEnd = close + 1
	}
	// The header line must end at the newline (only spaces after ']').
	rest := data[nameEnd+1:]
	lineEnd := bytes.IndexByte(rest, '\n')
	seg := rest
	if lineEnd >= 0 {
		seg = rest[:lineEnd]
	} else {
		lineEnd = len(rest) - 1 // no newline: last line of the file
	}
	if strings.TrimSpace(string(seg)) != "" {
		return header{}, false
	}
	bodyStart := nameEnd + 1
	if lineEnd >= 0 && lineEnd < len(rest) {
		bodyStart = nameEnd + 1 + lineEnd + 1 // past the newline
	} else {
		bodyStart = n
	}
	name := string(data[i+1 : close])
	if array {
		name = string(data[i+2 : close])
	}
	return header{
		name:      canonicalName(name),
		start:     i,
		bodyStart: bodyStart,
		array:     array,
	}, true
}

// findTable returns the single header whose canonical name equals name,
// wrapped ErrTableNotFound / ErrArrayTable / ErrDuplicateTable otherwise.
// Bare names are matched against both spellings for harness tables via
// harnessNames; for stable tables the caller passes the full "stable.x".
func (e *Editor) findTable(names ...string) (header, error) {
	headers := findHeaders(e.data)
	var found *header
	for _, h := range headers {
		match := false
		for _, want := range names {
			if h.name == want {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		if h.array {
			return header{}, fmt.Errorf("%w: [[%s]] is an array table", ErrArrayTable, h.name)
		}
		if found != nil {
			return header{}, fmt.Errorf("%w: [%s] appears more than once", ErrDuplicateTable, h.name)
		}
		hc := h
		found = &hc
	}
	if found == nil {
		return header{}, fmt.Errorf("%w: none of [%s]", ErrTableNotFound, strings.Join(names, "] ["))
	}
	return *found, nil
}

// tableEnd returns the exclusive end offset of the table starting at h: the
// start of the next header, or the end of the file. Trailing blank lines
// before the next header belong to the NEXT table, not this one.
func (e *Editor) tableEnd(h header) int {
	headers := findHeaders(e.data)
	for _, nh := range headers {
		if nh.start > h.start {
			// Keep at most one blank line with the removed table so the file
			// does not grow or glue sections: end at the last non-blank line
			// of this table's body.
			end := nh.start
			body := e.data[h.bodyStart:end]
			trimmed := bytes.TrimRight(body, " \t\n")
			return h.bodyStart + len(trimmed) + trailingNewline(body[len(trimmed):])
		}
	}
	return len(e.data)
}

func trailingNewline(tail []byte) int {
	if len(tail) == 0 {
		return 0
	}
	return 1
}

// bareKeyRe is the grammar for everything this editor writes verbatim into
// the file: harness table names and keys are bare keys (letters, digits,
// '-', '_'). The check is not cosmetic: a name or key ends up inside the
// file's bytes, and a crafted one could smuggle a whole table — "[mcp.x]"
// as a key would write one — past the narrow-API guarantee of REQ-11.
var bareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// stableNameRe is the stable-name grammar from SPEC-0026.
var stableNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func validName(re *regexp.Regexp, what, s string) error {
	if !re.MatchString(s) {
		return fmt.Errorf("tomledit: invalid %s %q", what, s)
	}
	return nil
}

func validKey(s string) error {
	if !bareKeyRe.MatchString(s) {
		return fmt.Errorf("tomledit: invalid key %q (letters, digits, '-' and '_' only)", s)
	}
	return nil
}

// AddStable appends a [stable.<name>] table at the end of the file, writing
// the given keys in order (strings, string lists, booleans). It refuses when
// the table already exists.
func (e *Editor) AddStable(name string, keys [][2]any) error {
	if err := validName(stableNameRe, "stable name", name); err != nil {
		return err
	}
	return e.addTable("stable."+name, nil, keys)
}

// AddHarness appends a [harness.<name>] table at the end of the file. It
// refuses when a table for that harness already exists under EITHER spelling
// — the loader registers [harness.<name>] and bare [<name>] as the same
// harness (duplicate harness "name"), so appending the other spelling would
// produce a config that fails to load.
func (e *Editor) AddHarness(name string, keys [][2]any) error {
	if err := validName(bareKeyRe, "harness name", name); err != nil {
		return err
	}
	return e.addTable("harness."+name, []string{name}, keys)
}

func (e *Editor) addTable(dotted string, altNames []string, keys [][2]any) error {
	names := append([]string{dotted}, altNames...)
	if _, err := e.findTable(names...); err == nil {
		return fmt.Errorf("%w: [%s] already exists", ErrDuplicateTable, dotted)
	} else if !errors.Is(err, ErrTableNotFound) {
		return err
	}
	var b strings.Builder
	b.WriteString("[" + dotted + "]\n")
	if err := writeKeys(&b, keys); err != nil {
		return err
	}
	body := b.String()
	out := strings.TrimRight(string(e.data), "\n")
	if out != "" {
		out += "\n\n"
	}
	e.data = []byte(out + body)
	return nil
}

// RemoveStable removes the [stable.<name>] table, header through the end of
// its body (multi-line aware), comments included.
func (e *Editor) RemoveStable(name string) error {
	return e.removeTable("stable." + name)
}

// RemoveHarness removes a harness table under either spelling,
// [harness.<name>] or bare [<name>].
func (e *Editor) RemoveHarness(name string) error {
	return e.removeTable("harness."+name, name)
}

func (e *Editor) removeTable(names ...string) error {
	h, err := e.findTable(names...)
	if err != nil {
		return err
	}
	end := e.tableEnd(h)
	// Cut from the start of the header line (include leading indentation,
	// already zero — start IS the '['; back up over nothing).
	e.data = append(append([]byte{}, e.data[:h.start]...), e.data[end:]...)
	// Tidy the seam the cut creates: more than one blank line between the
	// previous table's last key and the next header collapses to one. The
	// tidy touches only the junction bytes — a whole-file replace would
	// rewrite blank lines inside multi-line strings the cut never went near,
	// silently corrupting values this editor promises to preserve.
	seam := h.start
	before, after := e.data[:seam], e.data[seam:]
	trailing := 0
	for trailing < len(before) && before[len(before)-1-trailing] == '\n' {
		trailing++
	}
	leading := 0
	for leading < len(after) && after[leading] == '\n' {
		leading++
	}
	if excess := trailing + leading - 2; excess > 0 {
		dropAfter := min(excess, leading)
		dropBefore := excess - dropAfter
		if dropBefore > trailing {
			dropBefore = trailing
		}
		e.data = append(before[:len(before)-dropBefore], after[dropAfter:]...)
	}
	return nil
}

// RemoveHarnessKey deletes one key line from [harness.<name>] (either
// spelling), including a multi-line value's continuation lines. It fails
// closed with ErrKeyNotFound when the key is absent, and never touches any
// other line. The upgrade review uses it to let a local override go so the
// package's value applies (issue #882).
func (e *Editor) RemoveHarnessKey(table, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	h, err := e.findTable("harness."+table, table)
	if err != nil {
		return err
	}
	end := e.tableEnd(h)
	kv, ok := e.findKey(h.bodyStart, end, key)
	if !ok {
		return fmt.Errorf("%w: key %q in [harness.%s]", ErrKeyNotFound, key, table)
	}
	// Take the rest of the value's last line and its newline too: a
	// trailing comment belongs to the key it annotates, and removing the
	// last key must not leave a stray blank or comment line behind.
	valueEnd := len(e.data)
	if nl := bytes.IndexByte(e.data[kv.valueEnd:], '\n'); nl >= 0 {
		valueEnd = kv.valueEnd + nl + 1
	}
	e.data = splice(e.data, kv.start, valueEnd, nil)
	return nil
}

// SetStableKey sets one key inside [stable.<name>], in place: the value line
// is replaced when present, the key appended to the table when absent.
func (e *Editor) SetStableKey(table, key string, value any) error {
	return e.setKey("stable."+table, key, value)
}

// SetHarnessKey sets one key inside a harness table under either spelling.
func (e *Editor) SetHarnessKey(table, key string, value any) error {
	return e.setKey("harness."+table, key, value, table)
}

func (e *Editor) setKey(dotted, key string, value any, altNames ...string) error {
	if err := validKey(key); err != nil {
		return err
	}
	names := append([]string{dotted}, altNames...)
	h, err := e.findTable(names...)
	if err != nil {
		return err
	}
	end := e.tableEnd(h)
	encoded, err := encodeValue(value)
	if err != nil {
		return err
	}
	if kv, ok := e.findKey(h.bodyStart, end, key); ok {
		// The existing value may span lines (a multi-line array or string);
		// replacing only the key's first line would orphan its continuation
		// lines as garbage, so the splice runs through the value's true end.
		// It reuses the file's own key text — its quoting and indentation —
		// rather than re-emitting the caller's key string: a quoted key
		// rewritten unquoted can stop parsing, and only the file's bytes are
		// known good here.
		replacement := kv.keyText + " = " + encoded
		e.data = splice(e.data, kv.start, kv.valueEnd, []byte(replacement))
		return nil
	}
	// Absent: append inside the table, after the last key. tableEnd already
	// normalized the body to at most one trailing newline, so the new key
	// lands after it (or after a newline we add when the file ended without
	// one).
	body := e.data[h.bodyStart:end]
	lines := splitLines(body)
	insert := []byte(key + " = " + encoded + "\n")
	if len(body) > 0 && body[len(body)-1] != '\n' {
		insert = append([]byte("\n"), insert...)
	}
	insertAt := h.bodyStart + len(body)
	// Land after the table's last key or value line, not between it and its
	// trailing comments: walking back over blank and comment lines is safe
	// because a value's closing line — a string's triple quote, an array's
	// ']' — is itself neither blank nor a comment.
	for k := len(lines); k > 0; k-- {
		t := strings.TrimSpace(lines[k-1].content)
		if t == "" || strings.HasPrefix(t, "#") {
			insertAt = h.bodyStart + lines[k-1].start
			continue
		}
		break
	}
	e.data = splice(e.data, insertAt, insertAt, insert)
	return nil
}

// keyValue is one key/value pair inside a table body, as absolute offsets
// into the file.
type keyValue struct {
	start    int    // first byte of the key's line, indentation included
	valueEnd int    // where the value ends, per valueSpanEnd
	keyText  string // the file's own text before '=', trailing blanks trimmed
}

// findKey returns the pair whose key is key in the table body [from, to).
// It walks the body key line by key line and jumps over each value through
// valueSpanEnd, so a multi-line value's continuation lines are never tested
// as key starts: once its quotes are stripped, `  "model=opus",` inside an
// array reads exactly like a `model` key line, and so does `model = "x"`
// inside a multi-line string. Every lookup of an existing key goes through
// here, never through a plain line scan.
func (e *Editor) findKey(from, to int, key string) (keyValue, bool) {
	for off := from; off < to; {
		lineEnd := to
		if nl := bytes.IndexByte(e.data[off:to], '\n'); nl >= 0 {
			lineEnd = off + nl
		}
		line := string(e.data[off:lineEnd])
		k := keyLineKey(line)
		if k == "" {
			off = lineEnd + 1
			continue
		}
		eqInLine := strings.Index(line, "=")
		kv := keyValue{
			start:    off,
			valueEnd: valueSpanEnd(e.data, off+eqInLine+1),
			keyText:  strings.TrimRight(line[:eqInLine], " \t"),
		}
		if k == key {
			return kv, true
		}
		// Resume at the line after the value's last line; a trailing
		// comment between the value and that newline is not a key line.
		nl := bytes.IndexByte(e.data[kv.valueEnd:], '\n')
		if nl < 0 {
			break
		}
		off = kv.valueEnd + nl + 1
	}
	return keyValue{}, false
}

// keyLineKey returns the TOML key of a `key = value` line, or "" when the
// line is not a key line (comment, blank). It cannot recognize a value's
// continuation line on its own; callers walk key lines with findKey.
func keyLineKey(line string) string {
	s := strings.TrimLeft(line, " \t")
	if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "[") {
		return ""
	}
	eq := strings.Index(s, "=")
	if eq <= 0 {
		return ""
	}
	key := strings.TrimSpace(s[:eq])
	return strings.Trim(key, `"'`)
}

// valueSpanEnd returns the offset where a key's value ends, given the offset
// just past its '='. It understands the shapes harness.toml carries — strings
// (single- and multi-line, basic and literal), arrays with nested brackets,
// booleans — and stops when the value is complete: outside every string, at
// bracket depth zero, at the value's newline or at a trailing comment's '#'.
// Stopping at the comment is what lets an in-place replacement preserve one;
// stopping at the newline is what keeps a multi-line array's continuation
// lines from being orphaned when the value is replaced wholesale.
func valueSpanEnd(data []byte, i int) int {
	n := len(data)
	depth := 0
	for i < n {
		c := data[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\n':
			if depth == 0 {
				return i
			}
			i++
		case c == '#':
			if depth == 0 {
				// Leave the whitespace between the value and the comment in
				// the remainder, so a replacement keeps `value # comment`
				// shaped instead of gluing `value"# comment`.
				for i > 0 && (data[i-1] == ' ' || data[i-1] == '\t') {
					i--
				}
				return i
			}
			for i < n && data[i] != '\n' {
				i++
			}
		case bytes.HasPrefix(data[i:], []byte(`"""`)):
			j := bytes.Index(data[i+3:], []byte(`"""`))
			if j < 0 {
				return n
			}
			i = i + 3 + j + 3
		case bytes.HasPrefix(data[i:], []byte("'''")):
			j := bytes.Index(data[i+3:], []byte("'''"))
			if j < 0 {
				return n
			}
			i = i + 3 + j + 3
		case c == '"':
			j := i + 1
			for j < n {
				if data[j] == '\\' {
					j += 2
					continue
				}
				if data[j] == '"' || data[j] == '\n' {
					break
				}
				j++
			}
			i = j + 1
		case c == '\'':
			j := bytes.IndexByte(data[i+1:], '\n')
			if j < 0 {
				return n
			}
			i = i + 1 + j
		case c == '[':
			depth++
			i++
		case c == ']':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return n
}

type lineInfo struct {
	content string
	start   int // offset within the slice passed to splitLines
	length  int // length of the line including its newline
}

// splitLines is line-oriented only where that is safe: a table body between
// two headers, where every key starts a fresh line.
func splitLines(body []byte) []lineInfo {
	var out []lineInfo
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			out = append(out, lineInfo{string(body[start:i]), start, i - start + 1})
			start = i + 1
		}
	}
	if start < len(body) {
		out = append(out, lineInfo{string(body[start:]), start, len(body) - start})
	}
	return out
}

func splice(data []byte, from, to int, insert []byte) []byte {
	out := make([]byte, 0, len(data)-(to-from)+len(insert))
	out = append(out, data[:from]...)
	out = append(out, insert...)
	out = append(out, data[to:]...)
	return out
}

// encodeValue renders the only value shapes these commands write: TOML basic
// strings with escapes, string lists, booleans and integers.
//
// @joestump-agent 10/10/2026 - Integers, for a suggested restart_delay written
// onto a harness table (harness#931).
func encodeValue(v any) (string, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case string:
		return encodeString(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case []string:
		parts := make([]string, len(t))
		for i, s := range t {
			parts[i] = encodeString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	default:
		return "", fmt.Errorf("tomledit: unsupported value type %T (strings, string lists, booleans, integers only)", v)
	}
}

func encodeString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func writeKeys(b *strings.Builder, keys [][2]any) error {
	for _, kv := range keys {
		if err := validKey(kv[0].(string)); err != nil {
			return err
		}
		enc, err := encodeValue(kv[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "%s = %s\n", kv[0].(string), enc)
	}
	return nil
}
