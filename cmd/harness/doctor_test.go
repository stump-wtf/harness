package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
)

// stringErr is a minimal error for testing isMissing without pulling in os.
type stringErr struct{ s string }

func (e *stringErr) Error() string { return e.s }

func TestRunDoctorNoConfigNoDaemon(t *testing.T) {
	// With a non-existent config and a non-existent daemon socket, doctor
	// should run both checks, render the table, and exit non-zero.
	// Force plain-text mode to keep test output deterministic.
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })

	o := verbOpts{
		configPath: "/tmp/harness-doctor-test-does-not-exist.toml",
		socket:     "/tmp/harness-doctor-test-no-daemon.sock",
	}
	if code := runDoctor(o); code != 1 {
		t.Errorf("runDoctor = %d, want 1", code)
	}
}

func TestRunDoctorWithValidConfigNoDaemon(t *testing.T) {
	// A valid config but no daemon: config row passes, daemon row fails,
	// doctor exits non-zero.
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })

	dir := t.TempDir()
	cfgPath := dir + "/harness.toml"
	if err := writeMinimalConfig(cfgPath); err != nil {
		t.Fatal(err)
	}

	o := verbOpts{
		configPath: cfgPath,
		socket:     "/tmp/harness-doctor-test-no-daemon-2.sock",
	}
	if code := runDoctor(o); code != 1 {
		t.Errorf("runDoctor = %d, want 1 (daemon missing)", code)
	}
}

// --- table-render unit tests -----------------------------------------------

func TestPrintDoctorTableRendersAllRows(t *testing.T) {
	t.Parallel()
	rows := []check{
		{name: "config", level: cliui.LevelSuccess, detail: "ok — 2 harnesses"},
		{name: "daemon", level: cliui.LevelSuccess, detail: "listening at /tmp/x.sock"},
		{name: "version", level: cliui.LevelWarn, detail: "client v1 vs daemon v2",
			hint: "restart the daemon"},
		{name: "harnesses", level: cliui.LevelError, detail: "1/2 failed: demo",
			hint: "harness restart demo"},
	}
	var buf bytes.Buffer
	printDoctorTable(&buf, rows)
	out := buf.String()

	for _, want := range []string{"CHECK", "STATUS", "DETAIL", "config", "daemon", "version", "harnesses", "summary"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Hint lines must appear.
	if !strings.Contains(out, "→ restart the daemon") {
		t.Errorf("missing version hint in:\n%s", out)
	}
	if !strings.Contains(out, "→ harness restart demo") {
		t.Errorf("missing harnesses hint in:\n%s", out)
	}
	// Summary tally reflects the worst level.
	if !strings.Contains(out, "2 passed") || !strings.Contains(out, "1 warning(s)") || !strings.Contains(out, "1 failed") {
		t.Errorf("summary tally wrong in:\n%s", out)
	}
	// Separator rules present (header underline + above summary).
	if strings.Count(out, strings.Repeat("─", 40)) < 2 {
		t.Errorf("expected at least 2 separator rules in:\n%s", out)
	}
}

// TestPrintDoctorTableHintAlignsUnderDetail pins the column-alignment fix
// for hint rows. Hints are continuation rows of the DETAIL column: they must
// be indented to the DETAIL column's left edge (after CHECK + STATUS +
// separators), not start at column 0. DETAIL offset = CHECK(12) + colSep(2)
// + STATUS(10) + colSep(2) = 26 in the shared table renderer.
func TestPrintDoctorTableHintAlignsUnderDetail(t *testing.T) {
	t.Parallel()
	rows := []check{
		{name: "daemon", level: cliui.LevelError, detail: "unreachable",
			hint: "start it with: harness daemon"},
	}
	var buf bytes.Buffer
	printDoctorTable(&buf, rows)
	out := buf.String()

	// Find the hint line and assert it is indented to the DETAIL column.
	const detailOffset = 26 // CHECK(12) + sep(2) + STATUS(10) + sep(2)
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "→ start it with") {
			continue
		}
		if strings.HasPrefix(line, "→") {
			t.Errorf("hint line starts at column 0 (must align under DETAIL):\n%s", out)
		}
		// The arrow must land at exactly detailOffset. Everything before it
		// must be spaces.
		arrow := strings.Index(line, "→")
		if arrow != detailOffset {
			t.Errorf("hint arrow at column %d, want %d (DETAIL edge):\n%s", arrow, detailOffset, out)
		}
		// Sanity: the same offset as the detail cell on the row above.
		for _, other := range strings.Split(out, "\n") {
			if strings.Contains(other, "unreachable") {
				detailCell := strings.Index(other, "unreachable")
				if detailCell != detailOffset {
					t.Errorf("detail cell at %d, expected %d:\n%s", detailCell, detailOffset, out)
				}
				break
			}
		}
		return
	}
	t.Errorf("hint line not found in:\n%s", out)
}

func TestPrintDoctorTablePlainWhenNotColored(t *testing.T) {
	t.Parallel()
	// When useColor is false (no TTY / JSON), the output must not contain
	// ANSI escapes. printDoctorTable decides via cliui.IsTTY + cliui.JSON(),
	// which in a test runner both resolve to "no color".
	rows := []check{
		{name: "x", level: cliui.LevelSuccess, detail: "y"},
	}
	var buf bytes.Buffer
	printDoctorTable(&buf, rows)
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("expected no ANSI in plain mode, got %q", buf.String())
	}
}

// --- JSON output unit tests ------------------------------------------------

func TestEmitDoctorJSONShape(t *testing.T) {
	t.Parallel()
	rows := []check{
		{name: "config", level: cliui.LevelSuccess, detail: "ok — 1 harness"},
		{name: "daemon", level: cliui.LevelSuccess, detail: "listening"},
		{name: "version", level: cliui.LevelWarn, detail: "skew", hint: "restart"},
		{name: "harnesses", level: cliui.LevelError, detail: "1 failed", hint: "restart x"},
	}
	var buf bytes.Buffer
	emitDoctorJSON(&buf, rows)

	var res doctorResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, buf.String())
	}
	if res.Config.Status != "ok" {
		t.Errorf("config status = %q", res.Config.Status)
	}
	if res.Version == nil || res.Version.Status != "warn" {
		t.Errorf("version row missing or wrong: %+v", res.Version)
	}
	if res.Harness == nil || res.Harness.Status != "error" {
		t.Errorf("harness row missing or wrong: %+v", res.Harness)
	}
	if res.Summary.Passed != 2 || res.Summary.Warned != 1 || res.Summary.Failed != 1 {
		t.Errorf("summary = %+v", res.Summary)
	}
}

func TestEmitDoctorJSONAllPassed(t *testing.T) {
	t.Parallel()
	rows := []check{
		{name: "config", level: cliui.LevelSuccess, detail: "ok"},
		{name: "daemon", level: cliui.LevelSuccess, detail: "ok"},
		{name: "version", level: cliui.LevelSuccess, detail: "ok"},
		{name: "harnesses", level: cliui.LevelSuccess, detail: "all good"},
	}
	var buf bytes.Buffer
	emitDoctorJSON(&buf, rows)

	var res doctorResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Summary.Passed != 4 {
		t.Errorf("passed = %d, want 4", res.Summary.Passed)
	}
	if res.Summary.Failed != 0 {
		t.Errorf("failed = %d, want 0", res.Summary.Failed)
	}
}

func writeMinimalConfig(path string) error {
	return os.WriteFile(path, []byte("[harness.demo]\ncmd = \"echo hi\"\nenabled = false\n"), 0o644)
}
