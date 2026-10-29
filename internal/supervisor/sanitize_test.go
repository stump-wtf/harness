package supervisor

// Governing: #279 — the durable per-harness log stores sanitized output
// history (scrolled-off screen lines + a final-screen flush), never the raw
// PTY byte stream. These tests pin the three properties that make `harness
// logs` readable again: in-place repaints (a TUI agent's per-second
// spinner/timer frames) produce nothing, scrolled output lines land verbatim,
// and the end-of-run screen flush recovers a short run that never scrolled.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// spinnerFrames simulates what a full-screen agent TUI emits every second
// while a tool runs: home the cursor, clear the line, repaint a status line
// whose only change is the elapsed-seconds counter. One frame per second of a
// run must yield ZERO log lines.
func spinnerFrames(seconds int) []byte {
	var b bytes.Buffer
	for i := 1; i <= seconds; i++ {
		b.WriteString("\x1b[H\x1b[2J") // home + clear
		fmt.Fprintf(&b, "\x1b[1;1H✻ Working (%ds)…\r\n", i)
		// Last painted row carries no trailing newline — a real TUI leaves the
		// cursor on the row it just wrote, exactly so it does not scroll.
		fmt.Fprintf(&b, "\x1b[2;1H\x1b[31mtool: bash\x1b[0m")
	}
	return b.Bytes()
}

// TestPtyHistoryRepaintsProduceNoLines is the #279 regression: a 60-second
// tool run's worth of repaint frames must not add a single line (previously
// the raw tee recorded one junk line per second).
func TestPtyHistoryRepaintsProduceNoLines(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	if _, err := h.Write(spinnerFrames(60)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("repaint-only stream wrote %d bytes, want 0:\n%q", out.Len(), out.String())
	}
}

// TestPtyHistoryRepaintsContainNoEscapes doubles down on the same stream: even
// after scrolling content, the log must never contain an ESC byte.
func TestPtyHistoryRepaintsContainNoEscapes(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write(spinnerFrames(10))
	// Now let real output scroll everything off.
	for i := 0; i < 40; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("real line %d\r\n", i)))
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Fatalf("log contains ESC bytes:\n%q", out.String())
	}
}

// TestPtyHistoryCapturesScrolledLines verifies printed lines land verbatim
// once they scroll off the screen (screen is 24 rows).
func TestPtyHistoryCapturesScrolledLines(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	for i := 1; i <= 30; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("line %02d\r\n", i)))
	}
	got := out.String()
	// Lines 1..7 (30 - 24 + 1) have scrolled off and must be recorded.
	for i := 1; i <= 7; i++ {
		want := fmt.Sprintf("line %02d\n", i)
		if !strings.Contains(got, want) {
			t.Errorf("log missing scrolled-off %q; got:\n%s", want, got)
		}
	}
	// Lines still on screen must not be double-recorded yet.
	if strings.Contains(got, "line 08\n") && strings.Contains(got, "line 08\nline 09") {
		t.Errorf("on-screen lines recorded before scrolling:\n%s", got)
	}
}

// TestPtyHistoryFlushLandsFinalScreen verifies the short-run path: output that
// never scrolled is appended when the stream ends.
func TestPtyHistoryFlushLandsFinalScreen(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write([]byte("\x1b[32mhello\x1b[0m world\r\n"))
	_, _ = h.Write([]byte("second line\r\n"))
	h.Flush()
	got := out.String()
	for _, want := range []string{"hello world\n", "second line\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("flushed screen missing %q; got:\n%s", want, got)
		}
	}
	if bytes.ContainsRune([]byte(got), 0x1b) {
		t.Errorf("flushed screen contains ESC bytes:\n%q", got)
	}
}

// Credential-shaped fixtures, assembled at run time from split literals so no
// high-entropy token exists as a contiguous string in this source. Assertions
// reference these rather than restating the value — restating it puts the
// literal back and the secret scan flags it.
var (
	pwSecret     = "0123456789" + "abcdef0123"
	bearerSecret = "sk-" + "abcdefghijklmnopqrstuvwxyz"
)

var credFixtures = strings.NewReplacer(
	"<PW_REMOTE>", "https://joestump-agent:"+pwSecret+"@gitea.stump.rocks/a/b.git",
	"<BEARER>", "Authorization: Bearer "+bearerSecret,
)

// TestPtyHistoryMasksCredentialsBeforeDisk: the durable log records what the
// program printed, so an agent running `git remote set-url` with a token in
// the URL puts it in the file verbatim. Masking at write time is what keeps
// the secret out of the artifact — the log is readable directly under
// $XDG_STATE_HOME regardless of what the protocol serves (ADR-0008 as
// amended; issue #312).
func TestPtyHistoryMasksCredentialsBeforeDisk(t *testing.T) {
	remote := credFixtures.Replace("<PW_REMOTE>")
	bearer := credFixtures.Replace("<BEARER>")

	var out bytes.Buffer
	h := newPtyHistory(&out, 200, 24)
	_, _ = h.Write([]byte("git remote set-url origin " + remote + "\r\n"))
	_, _ = h.Write([]byte("curl -H '" + bearer + "' https://example.com\r\n"))
	h.Flush()
	got := out.String()

	for _, secret := range []string{pwSecret, bearerSecret} {
		if strings.Contains(got, secret) {
			t.Errorf("durable log kept a credential (%q):\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("nothing was masked at all:\n%s", got)
	}
	// The surrounding command must survive: --raw still shows the shape of
	// what ran, only not the secret inside it.
	if !strings.Contains(got, "git remote set-url origin") {
		t.Errorf("masking ate the command text:\n%s", got)
	}
}

// TestPtyHistoryMasksKeyBodyBeforeDisk: the log is written a row at a time,
// and a PEM private key spans many rows. Masking each row on its own masks the
// BEGIN line and writes the body — the key itself — to disk verbatim. Fixture
// assembled from split literals so the secret scan has no block to match.
func TestPtyHistoryMasksKeyBodyBeforeDisk(t *testing.T) {
	body := strings.Repeat("QUJD", 16)
	key := "-----BEGIN " + "OPENSSH PRIVATE KEY-----\r\n" + body + "\r\n" + body +
		"\r\n-----END " + "OPENSSH PRIVATE KEY-----\r\n"

	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write([]byte("$ cat ~/.ssh/id_ed25519\r\n" + key + "$ ls\r\n"))
	h.Flush()
	got := out.String()

	if strings.Contains(got, body) {
		t.Errorf("durable log kept private key material:\n%s", got)
	}
	if !strings.Contains(got, "$ cat ~/.ssh/id_ed25519") || !strings.Contains(got, "$ ls") {
		t.Errorf("masking ate the lines around the key:\n%s", got)
	}
}

// TestPtyHistoryFlushIsOnce verifies a second Flush (closeLog runs
// defensively after the reader EOF flush) cannot duplicate the screen.
func TestPtyHistoryFlushIsOnce(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write([]byte("only line\r\n"))
	h.Flush()
	h.Flush()
	if n := strings.Count(out.String(), "only line\n"); n != 1 {
		t.Errorf("flush wrote the screen %d times, want 1", n)
	}
}

// TestLogRecordsLifecycleEvents verifies the durable log carries structured
// lifecycle lines (charmbracelet/log) alongside the output history (#279): a
// harness that runs and exits leaves "state changed" and "exited" events in
// its log, so `harness logs` explains a dead harness without any PTY bytes.
func TestLogRecordsLifecycleEvents(t *testing.T) {
	dir := t.TempDir()
	h := shHarness("ephemeral", "echo DONE", 0)
	h.Restart = core.RestartNo
	s := New(h, Options{
		Policy: Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond},
		Bus:    NewBus(),
		LogCfg: LogConfig{Dir: dir},
	})
	s.Restore(true, false, 0, 0, time.Time{}, time.Time{}, IntentChange{})
	s.Start()
	waitState(t, s, core.StateStopped) // RestartNever: a clean exit lands stopped
	s.Shutdown()

	data, err := readUntilContains(filepath.Join(dir, "ephemeral.log"),
		[]string{"state changed", "exited", "DONE"}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"state changed", "exited", "DONE"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q; got:\n%s", want, got)
		}
	}
	if bytes.ContainsRune(data, 0x1b) {
		t.Errorf("log contains ESC bytes:\n%q", got)
	}
}

// readUntilContains polls path until every wanted substring has appeared (the
// PTY bytes land in the log asynchronously, so a single read after Shutdown
// can miss the guest's output) or the deadline passes, returning the last
// read.
func readUntilContains(path string, wanted []string, within time.Duration) ([]byte, error) {
	deadline := time.Now().Add(within)
	var data []byte
	for {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		data = b
		missing := false
		for _, want := range wanted {
			if !strings.Contains(string(data), want) {
				missing = true
				break
			}
		}
		if !missing || time.Now().After(deadline) {
			return data, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPtyHistoryQueriesDoNotBlock pins the sanitizer's terminal-query
// deadlock (#299): the sanitizer runs its own vt emulator, whose synthesized
// query replies (DA1, DECRQM, cursor position — everything a Bubble Tea or
// ink agent TUI probes at startup) go into an unbuffered pipe. With nothing
// draining that pipe, Write blocked forever on the guest's opening handshake
// and froze the whole PTY read loop, so the guest looked headless. The drain
// goroutine (drainReplies) must keep Write unblocked, whatever the guest
// queries.
func TestPtyHistoryQueriesDoNotBlock(t *testing.T) {
	var log bytes.Buffer
	h := newPtyHistory(&log, 80, 24)

	// The startup burst crush sends before its first frame: DA1, secondary
	// DA, a DECRQM probe, the kitty-keyboard query, an OSC 11 background
	// color query and a cursor position report.
	queries := "\x1b[c\x1b[>c\x1b[?2026$p\x1b[?u\x1b]11;?\x07\x1b[6n"

	done := make(chan struct{})
	go func() {
		_, _ = h.Write([]byte(queries))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ptyHistory.Write blocked on the guest's terminal queries")
	}
}

// TestPtyHistorySurvivesEmulatorPanic pins the recovery in feedLocked. A
// guest that sets a scroll region taller than the PTY and then scrolls
// (DECSTBM 1;30 on a 24-row screen, then SU) makes x/vt's ScrollUp index
// past ultraviolet's buffer: "index out of range [24] with length 24". That
// panic was on the PTY reader goroutine and took the daemon down; crush does
// exactly this when it renders a channel doorbell on the default 80×24.
//
// The reset-first recovery sends ESC[r to restore the default scroll region
// without creating a new emulator or leaking a goroutine.
func TestPtyHistorySurvivesEmulatorPanic(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	for i := 1; i <= 3; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("before %d\r\n", i)))
	}
	// Must not panic. The reset-first recovery heals the poisoned scroll region.
	_, _ = h.Write([]byte("\x1b[1;30r\x1b[9S\r\n"))
	if !strings.Contains(out.String(), "recovered from a terminal rendering panic") {
		t.Fatalf("expected the recovery marker in the log, got:\n%s", out.String())
	}
	// The sanitizer keeps working on the same emulator: enough lines to scroll
	// the 24-row screen must still land in the log.
	for i := 1; i <= 30; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("after %02d\r\n", i)))
	}
	if !strings.Contains(out.String(), "after 01") {
		t.Fatalf("scrolled lines after the recovery did not reach the log:\n%s", out.String())
	}
}

// TestPtyHistoryNoGoroutineLeakOnPanic verifies that repeated panics do not
// leak drainReplies goroutines. The reset-first recovery heals the state
// without creating new emulators, so the pump count stays constant.
func TestPtyHistoryNoGoroutineLeakOnPanic(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)

	// Baseline: one pump (the drainReplies started in newPtyHistory).
	before := runtime.NumGoroutine()

	// Induce 50 panics. Without reset-first, each creates a new pump.
	for i := 0; i < 50; i++ {
		_, _ = h.Write([]byte("\x1b[1;30r\x1b[9S\r\n"))
	}

	// Allow goroutines to settle.
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	// Allow some jitter, but not 50 leaked pumps.
	if after-before > 5 {
		t.Fatalf("goroutine count grew by %d (before=%d, after=%d); expected reset-first recovery to avoid leaks", after-before, before, after)
	}
}

// fillTo writes numbered filler lines, one per screen row, so the screen is
// full and every later newline scrolls exactly one row off the top.
func fillTo(h *ptyHistory, through int) {
	for i := 1; i <= through; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
}

// assertLinesApart fails when any of want landed in the log joined to anything:
// each must appear as a line of its own.
func assertLinesApart(t *testing.T, log string, want ...string) {
	t.Helper()
	lines := strings.Split(log, "\n")
	for _, w := range want {
		found := false
		for _, ln := range lines {
			if ln == w {
				found = true
			} else if strings.Contains(ln, w) {
				t.Errorf("%q landed glued to other text: %q", w, ln)
			}
		}
		if !found {
			t.Errorf("log has no line exactly %q; got:\n%s", w, log)
		}
	}
}

// TestPtyHistoryJoinsSoftWrappedRows is the #279 remainder: a line the program
// wrote as one line, which the terminal folded across rows because it reached
// the last column, must come back out of the sanitizer as one line — while the
// unwrapped lines around it stay separate.
func TestPtyHistoryJoinsSoftWrappedRows(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	fillTo(h, 25)
	long := "wrapped " + strings.Repeat("x", 300) // four rows at 80 columns
	_, _ = fmt.Fprintf(h, "%s\r\n", long)
	fillTo(h, 60)
	for i := 26; i <= 60; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
	h.Flush()
	got := out.String()
	if !strings.Contains(got, long+"\n") {
		t.Errorf("the folded line did not land as one line; got:\n%s", got)
	}
	for _, ln := range strings.Split(got, "\n") {
		if rest := strings.Trim(ln, "x"); ln != "" && rest == "" {
			t.Fatalf("a fold landed in the log as a line of its own: %q", ln)
		}
	}
	want := make([]string, 0, 60)
	for i := 1; i <= 60; i++ {
		want = append(want, fmt.Sprintf("fill %02d", i))
	}
	assertLinesApart(t, got, want...)
}

// TestPtyHistoryFullWidthNewlineStaysApart is the other half of the same call:
// a row that fills the width AND ends in a newline was written by the program
// as its own line, so nothing is glued to it. Full-width rule lines, table
// borders and 80-column log lines are the common case this protects.
func TestPtyHistoryFullWidthNewlineStaysApart(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	fillTo(h, 25)
	rule := strings.Repeat("=", 80)
	_, _ = fmt.Fprintf(h, "%s\r\n", rule)
	_, _ = fmt.Fprintf(h, "after the rule\r\n")
	for i := 26; i <= 60; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
	h.Flush()
	got := out.String()
	if strings.Contains(got, rule+"after the rule") {
		t.Errorf("a newline-terminated full-width row was joined to the next:\n%s", got)
	}
	assertLinesApart(t, got, rule, "after the rule")
}

// TestPtyHistoryFlushJoinsFoldedTail covers the fold that never scrolls: the
// stream ends with the rows still on the screen, and the flush has to rejoin
// them the same way the scroll path does.
func TestPtyHistoryFlushJoinsFoldedTail(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	long := "tail " + strings.Repeat("y", 200)
	_, _ = fmt.Fprintf(h, "%s", long) // no newline at all: it stays on screen
	h.Flush()
	if got := out.String(); got != long+"\n" {
		t.Fatalf("flushed fold did not land as one line, got:\n%q", got)
	}
}

// TestPtyHistoryJoinsStyledFold pins the same join for output that carries
// colour: the fold is the terminal's, so styling around it must not talk the
// sanitizer out of it.
func TestPtyHistoryJoinsStyledFold(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	fillTo(h, 25)
	body := "styled " + strings.Repeat("z", 200)
	_, _ = fmt.Fprintf(h, "\x1b[31m%s\x1b[0m\r\n", body)
	for i := 26; i <= 60; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
	h.Flush()
	got := out.String()
	if !strings.Contains(got, body+"\n") {
		t.Errorf("a styled folded line did not land as one line; got:\n%s", got)
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Errorf("log contains ESC bytes:\n%q", got)
	}
}

// TestPtyHistoryTabFoldStillJoins pins the control-byte hole in the
// short-piece skip: a tab is one byte but advances the cursor several columns,
// so a tab-carrying piece shorter than the room left on its row can still fold
// that row. The skip must not fire on it — if it did, the fold went
// unrecorded, the record queue lost its alignment, and the folded line landed
// in the log split.
func TestPtyHistoryTabFoldStillJoins(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	fillTo(h, 25)
	_, _ = fmt.Fprint(h, "aaaa")                       // 4 bytes, 4 columns: skipped
	_, _ = fmt.Fprint(h, "\t"+strings.Repeat("b", 73)) // 74 bytes, folds the row
	_, _ = fmt.Fprint(h, "\r\n")
	for i := 26; i <= 60; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
	h.Flush()
	got := out.String()
	want := "aaaa" + strings.Repeat(" ", 4) + strings.Repeat("b", 73)
	if !strings.Contains(got, want+"\n") {
		t.Errorf("the tab-folded line did not land as one line; got:\n%s", got)
	}
	assertLinesApart(t, got, "fill 26", "fill 27")
}

// TestPtyHistoryTabFoldCannotMisjoinRule pins the sharper half of the same
// hole: after an unrecorded fold shifts the record queue by one, each row's
// emission pops its successor's record — so a full-width row the program ended
// with a newline receives the fold record of the folded line printed right
// after it, and is glued to it: the wrong join the width test cannot catch.
// The rule must stay apart, and the folded line must still join itself.
func TestPtyHistoryTabFoldCannotMisjoinRule(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	fillTo(h, 25)
	_, _ = fmt.Fprint(h, "aaaa")
	_, _ = fmt.Fprint(h, "\t"+strings.Repeat("b", 73))
	_, _ = fmt.Fprint(h, "\r\n")
	for i := 26; i <= 60; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
	rule := strings.Repeat("=", 80)
	_, _ = fmt.Fprintf(h, "%s\r\n", rule)
	wrapped := "wrapped " + strings.Repeat("w", 300)
	_, _ = fmt.Fprintf(h, "%s\r\n", wrapped)
	for i := 61; i <= 100; i++ {
		_, _ = fmt.Fprintf(h, "fill %02d\r\n", i)
	}
	h.Flush()
	got := out.String()
	if strings.Contains(got, rule+"wrapped") {
		t.Errorf("a newline-terminated full-width rule was joined to the next line:\n%s", got)
	}
	if !strings.Contains(got, wrapped+"\n") {
		t.Errorf("the folded line after the rule did not land as one line:\n%s", got)
	}
	assertLinesApart(t, got, rule)
}

// TestPtyHistoryPendingSplitsPastCap pins the bound on pending: rows the
// terminal folded hold until their logical line ends, and folds can outrun
// the scrolls that drain them, so a stream of folded rows must not make the
// sanitizer hold all of them. Past a few screens the held line splits, and
// the content still adds up. Driven at emitLocked: the screen diff above it
// has its own ambiguity on periodic content, which is not what this pins.
func TestPtyHistoryPendingSplitsPastCap(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	const rows = 101
	for i := 0; i < rows; i++ {
		h.pushEnd(true) // this row folded onto the next
		h.emitLocked(strings.Repeat("b", 80))
	}
	h.pushEnd(false) // the last row ended by a newline
	h.emitLocked(strings.Repeat("b", 80))
	h.Flush()

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("a stream of %d folded rows did not split: %d line(s)", rows+1, len(lines))
	}
	if logged := len(strings.ReplaceAll(out.String(), "\n", "")); logged != (rows+1)*80 {
		t.Errorf("the split changed content: %d of %d bytes logged", logged, (rows+1)*80)
	}
	for _, ln := range lines {
		if len(ln) > 4*80 {
			t.Errorf("a line longer than the cap landed: %d bytes: %.40q", len(ln), ln)
		}
	}
}

// TestDurableLogKeepsFoldedLineOnOneLine is the end-to-end pin: a real child on
// a real 80×24 PTY writes one long line, and the durable log — which is what
// both `harness logs` and `harness logs --json` print — holds it as one line.
func TestDurableLogKeepsFoldedLineOnOneLine(t *testing.T) {
	dir := t.TempDir()
	long := "foldme " + strings.Repeat("p", 200)
	h := shHarness("ephemeral", "printf '%s\\n' '"+long+"'", 0)
	h.Restart = core.RestartNo
	s := New(h, Options{
		Policy: Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond},
		Bus:    NewBus(),
		LogCfg: LogConfig{Dir: dir},
	})
	s.Restore(true, false, 0, 0, time.Time{}, time.Time{}, IntentChange{})
	s.Start()
	waitState(t, s, core.StateStopped)
	s.Shutdown()

	data, err := readUntilContains(filepath.Join(dir, "ephemeral.log"), []string{long}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(data), "\n") {
		if rest := strings.Trim(ln, "p"); ln != "" && rest == "" {
			t.Fatalf("a fold of the child's line landed as its own log line: %q", ln)
		}
	}
}

// TestDurableLogLandsFinalScreenBeforeExited pins the ordering of the two
// things that write the end of a run: the final screenful and the lifecycle
// line that says the agent exited. A reader tailing the log must see the
// program's last output first.
func TestDurableLogLandsFinalScreenBeforeExited(t *testing.T) {
	dir := t.TempDir()
	h := shHarness("ephemeral", "printf 'the last screenful'", 0)
	h.Restart = core.RestartNo
	s := New(h, Options{
		Policy: Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond},
		Bus:    NewBus(),
		LogCfg: LogConfig{Dir: dir},
	})
	s.Restore(true, false, 0, 0, time.Time{}, time.Time{}, IntentChange{})
	s.Start()
	waitState(t, s, core.StateStopped)
	s.Shutdown()

	data, err := readUntilContains(filepath.Join(dir, "ephemeral.log"),
		[]string{"the last screenful", "exited"}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	out, exit := strings.Index(got, "the last screenful"), strings.Index(got, "exited")
	if out > exit {
		t.Fatalf("the final screenful landed after the exited line:\n%s", got)
	}
}
