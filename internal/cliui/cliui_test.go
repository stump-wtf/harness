package cliui

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// newTestPrinter returns a Printer that writes to a buffer with JSON on or off.
// Tests use this instead of mutating the package-global Default so they stay
// parallel-safe.
func newTestPrinter(json bool, buf *bytes.Buffer) *Printer {
	return NewPrinter(Options{JSON: json, Out: buf})
}

func TestClassifyDaemonDown(t *testing.T) {
	t.Parallel()
	_, err := net.Dial("unix", "/tmp/harness-does-not-exist-test.sock")
	if err == nil {
		t.Fatal("expected dial to fail")
	}
	level, title, msg, hint := classify(err)
	if level != LevelError {
		t.Errorf("level = %v, want LevelError", level)
	}
	if title != "daemon not running" {
		t.Errorf("title = %q, want %q", title, "daemon not running")
	}
	if !strings.Contains(msg, "can't reach") {
		t.Errorf("msg = %q, want it to mention daemon unreachable", msg)
	}
	if !strings.Contains(msg, "/tmp/harness-does-not-exist-test.sock") {
		t.Errorf("msg = %q, want socket path included", msg)
	}
	if hint == "" {
		t.Errorf("want non-empty hint")
	}
}

func TestClassifyPathErrorOnSocket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socket := filepath.Join(dir, "harness.sock")
	_, err := os.ReadFile(socket)
	if err == nil {
		t.Fatal("expected ReadFile to fail")
	}
	if !isDaemonDown(err) {
		t.Errorf("isDaemonDown = false for %v", err)
	}
}

func TestClassifyPermissionDenied(t *testing.T) {
	t.Parallel()
	err := &os.PathError{
		Op:   "dial",
		Path: "/run/user/1000/harness.sock",
		Err:  os.ErrPermission,
	}
	level, title, msg, hint := classify(err)
	if level != LevelError {
		t.Errorf("level = %v", level)
	}
	if title != "permission denied" {
		t.Errorf("title = %q, want %q", title, "permission denied")
	}
	if !strings.Contains(msg, "access") {
		t.Errorf("msg = %q", msg)
	}
	if hint == "" {
		t.Errorf("want non-empty hint")
	}
}

func TestClassifyMissingConfig(t *testing.T) {
	t.Parallel()
	err := &os.PathError{
		Op:   "open",
		Path: "/home/user/.config/harness/harness.toml",
		Err:  os.ErrNotExist,
	}
	level, title, msg, hint := classify(err)
	if level != LevelError {
		t.Errorf("level = %v", level)
	}
	if title != "no config file" {
		t.Errorf("title = %q, want %q", title, "no config file")
	}
	if !strings.Contains(msg, "config not found") {
		t.Errorf("msg = %q", msg)
	}
	if !strings.Contains(msg, "/home/user/.config/harness/harness.toml") {
		t.Errorf("msg = %q, want path included", msg)
	}
	if hint == "" {
		t.Errorf("want non-empty hint")
	}
}

func TestClassifyGenericPassesThrough(t *testing.T) {
	t.Parallel()
	custom := errors.New("config: bad toml at line 42")
	level, title, msg, hint := classify(custom)
	if level != LevelError {
		t.Errorf("level = %v", level)
	}
	if title != "error" {
		t.Errorf("title = %q, want %q", title, "error")
	}
	if msg != "config: bad toml at line 42" {
		t.Errorf("msg = %q, want the raw error", msg)
	}
	if hint != "" {
		t.Errorf("hint = %q, want empty for unknown error", hint)
	}
}

func TestCleanMessageStripsPrefixes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"harness: something broke":        "something broke",
		"harness daemon: listen failed":   "listen failed",
		"client: dial unix /tmp/x: noent": "dial unix /tmp/x: noent",
		"  harness: spaced ":              "spaced",
		"no prefix":                       "no prefix",
	}
	for in, want := range cases {
		if got := cleanMessage(in); got != want {
			t.Errorf("cleanMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevelString(t *testing.T) {
	t.Parallel()
	cases := map[Level]string{
		LevelError:   "error",
		LevelWarn:    "warn",
		LevelInfo:    "info",
		LevelSuccess: "ok",
	}
	for lvl, want := range cases {
		if got := lvl.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", lvl, got, want)
		}
	}
}

func TestLevelGlyph(t *testing.T) {
	t.Parallel()
	cases := map[Level]string{
		LevelError:   "✗",
		LevelWarn:    "⚠",
		LevelInfo:    "•",
		LevelSuccess: "✓",
	}
	for lvl, want := range cases {
		if got := lvl.Glyph(); got != want {
			t.Errorf("%v.Glyph() = %q, want %q", lvl, got, want)
		}
	}
}

func TestLevelColor(t *testing.T) {
	t.Parallel()
	pal := DefaultColors()
	// Just verify each level returns a distinct, non-empty color.
	seen := map[string]bool{}
	for _, lvl := range []Level{LevelError, LevelWarn, LevelInfo, LevelSuccess} {
		c := lvl.Color(pal)
		if c == nil {
			t.Errorf("level %v has empty color", lvl)
			continue
		}
		r, g, b, _ := c.RGBA()
		seen[fmt.Sprintf("%d/%d/%d", r, g, b)] = true
	}
	// Expect at least 3 distinct colors (info/success might share family).
	if len(seen) < 3 {
		t.Errorf("expected >=3 distinct colors, got %d", len(seen))
	}
}

// DefaultColors returns the design-system palette resolved at full colour for
// tests that need it. The profile is pinned rather than detected: under `go
// test` stdout is not a terminal, so a detected theme would degrade every token
// to nil and the distinctness assertion would be vacuous.
func DefaultColors() theme.Colors {
	return theme.New(colorprofile.TrueColor, true, theme.DefaultPalette()).Colors()
}

func TestPrinterReportStyled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestPrinter(false, &buf)
	// renderStyled is private, so we go through Report — but Report also
	// checks IsTTY(os.Stderr). When stderr is not a TTY (the test runner
	// case), Report emits plain text. So this test asserts the plain path
	// for non-TTY; the styled path is covered by TestRenderStyledConsistent
	// below which calls renderStyled directly.
	p.Report(LevelError, "mytitle", "boom", "fix it")
	out := buf.String()
	if !strings.HasPrefix(out, "harness mytitle: ") {
		t.Errorf("expected plain prefix with title, got %q", out)
	}
}

func TestPrinterReportPlainWhenJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestPrinter(true, &buf)
	p.Report(LevelError, "", "boom", "fix it")
	out := buf.String()
	if !strings.HasPrefix(out, "harness error: ") {
		t.Errorf("expected plain prefix with default title, got %q", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("missing message: %q", out)
	}
	// JSON mode must not include the hint or any box-drawing glyphs.
	if strings.Contains(out, "→") || strings.Contains(out, "╭") {
		t.Errorf("expected no styling in JSON mode, got %q", out)
	}
}

func TestPrinterReportUsesCustomTitleInPlainMode(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestPrinter(true, &buf)
	p.Report(LevelError, "daemon not running", "msg", "hint")
	out := buf.String()
	if !strings.HasPrefix(out, "harness daemon not running: ") {
		t.Errorf("expected custom title prefix, got %q", out)
	}
}

func TestPrinterReportSkipsEmptyMessage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestPrinter(false, &buf)
	p.Report(LevelInfo, "t", "", "ignored")
	if buf.String() != "" {
		t.Errorf("expected no output for empty message, got %q", buf.String())
	}
}

func TestPrinterFatalReturnsOne(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestPrinter(true, &buf)
	if got := p.Fatal(errors.New("nope")); got != 1 {
		t.Errorf("Fatal(nope) = %d, want 1", got)
	}
	if got := p.Fatal(nil); got != 0 {
		t.Errorf("Fatal(nil) = %d, want 0", got)
	}
}

func TestPrinterFatalMsgReturnsOne(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestPrinter(true, &buf)
	if got := p.FatalMsg("bad", "msg", "fix"); got != 1 {
		t.Errorf("FatalMsg = %d, want 1", got)
	}
}

// --- styled-box width tests (call renderStyled directly so TTY is irrelevant)

func (p *Printer) renderStyledTest(level Level, title, msg, hint string) string {
	var buf bytes.Buffer
	p2 := NewPrinter(Options{Out: &buf})
	p2.renderStyled(level, title, msg, hint)
	return buf.String()
}

func TestRenderStyledContainsTitleMessageAndHint(t *testing.T) {
	t.Parallel()
	out := (&Printer{}).renderStyledTest(LevelError, "daemon exploded", "the daemon exploded", "try turning it off and on again")
	if !strings.Contains(out, "daemon exploded") {
		t.Errorf("missing title in:\n%s", out)
	}
	if !strings.Contains(out, "the daemon exploded") {
		t.Errorf("missing message in:\n%s", out)
	}
	if !strings.Contains(out, "try turning it off and on again") {
		t.Errorf("missing hint in:\n%s", out)
	}
	if !strings.Contains(out, "╭") && !strings.Contains(out, "┌") {
		t.Errorf("expected a border, got:\n%s", out)
	}
}

func TestRenderStyledOmitsHintWhenEmpty(t *testing.T) {
	t.Parallel()
	out := (&Printer{}).renderStyledTest(LevelInfo, "hi", "hello", "")
	if !strings.Contains(out, "hello") {
		t.Errorf("missing message in:\n%s", out)
	}
	if strings.Contains(out, "→") {
		t.Errorf("unexpected hint marker when hint was empty:\n%s", out)
	}
}

func TestBlockWidthClamped(t *testing.T) {
	t.Parallel()
	p := NewPrinter(Options{})
	w := p.blockWidth()
	if w < minBlockWidth || w > maxBlockWidth {
		t.Errorf("blockWidth() = %d, want in [%d, %d]", w, minBlockWidth, maxBlockWidth)
	}
}

func TestRenderStyledConsistentWidthAcrossMessages(t *testing.T) {
	t.Parallel()
	short := mustBoxWidth(t, LevelError, "e", "nope", "")
	long := mustBoxWidth(t, LevelError, "e",
		"open /home/user/.config/harness/harness.toml: no such file or directory", "")
	if short != long {
		t.Errorf("box width changed: short=%d long=%d (want equal)", short, long)
	}
}

func TestRenderStyledWidthClampedToMax(t *testing.T) {
	t.Parallel()
	w := mustBoxWidth(t, LevelInfo, "i", "hi", "")
	if w > maxBlockWidth+2 { // +2 for the border columns
		t.Errorf("rendered width %d exceeds max %d (incl. border)", w, maxBlockWidth)
	}
	if w < minBlockWidth {
		t.Errorf("rendered width %d below min %d", w, minBlockWidth)
	}
}

func TestWordWrapShort(t *testing.T) {
	t.Parallel()
	if got := wordWrap("hello world", 40); got != "hello world" {
		t.Errorf("wordWrap short = %q", got)
	}
}

func TestWordWrapBreaks(t *testing.T) {
	t.Parallel()
	got := wordWrap("one two three four", 8)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 11 {
			t.Errorf("line too long: %q (%d)", line, len(line))
		}
	}
	if !strings.Contains(got, "\n") {
		t.Errorf("expected wrapping, got %q", got)
	}
}

// mustBoxWidth renders a block and returns its visible width (in cells).
func mustBoxWidth(t *testing.T, level Level, title, msg, hint string) int {
	t.Helper()
	out := (&Printer{}).renderStyledTest(level, title, msg, hint)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "╭") {
			return utf8.RuneCountInString(stripANSI(line))
		}
	}
	t.Fatalf("no top border in:\n%s", out)
	return 0
}

// stripANSI removes SGR escape sequences for length measurement.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// TestClassifyNoProjectFound: the config.ErrNoProjectFound sentinel renders
// through the classify/hint channel like every other actionable error
// (SPEC-0004), with a down-specific hint when the wrap chain names the verb.
func TestClassifyNoProjectFound(t *testing.T) {
	t.Parallel()
	base := fmt.Errorf("%w (searched from /x to /)", config.ErrNoProjectFound)

	level, title, _, hint := classify(fmt.Errorf("harness down: %w", base))
	if level != LevelError || title != "no project file" {
		t.Errorf("classify(down) = %v %q, want LevelError %q", level, title, "no project file")
	}
	if !strings.Contains(hint, "harness down PROJECT") {
		t.Errorf("down hint = %q, want the explicit-project escape hatch", hint)
	}

	_, title, _, hint = classify(fmt.Errorf("harness up: %w", base))
	if title != "no project file" {
		t.Errorf("classify(up) title = %q, want %q", title, "no project file")
	}
	if strings.Contains(hint, "harness down PROJECT") {
		t.Errorf("up hint = %q, should not point at the down escape hatch", hint)
	}
	if hint == "" {
		t.Error("up hint empty, want an actionable hint")
	}
}

// TestClassifySurvivesVerbContextWrap: SPEC-0004 REQ "Error Handling
// Standards" wraps errors with verb context at each layer boundary; the
// classifier must keep matching the underlying shape through the %w chain
// (daemon-unreachable stays "daemon not running", not a generic error).
func TestClassifySurvivesVerbContextWrap(t *testing.T) {
	t.Parallel()
	_, dialErr := net.Dial("unix", "/tmp/harness-does-not-exist-wrap-test.sock")
	if dialErr == nil {
		t.Fatal("expected dial to fail")
	}
	wrapped := fmt.Errorf("harness ps: %w", dialErr)
	_, title, _, _ := classify(wrapped)
	if title != "daemon not running" {
		t.Errorf("classify(wrapped dial error) title = %q, want %q", title, "daemon not running")
	}
}
