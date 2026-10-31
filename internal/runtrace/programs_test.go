package runtrace

// Programs In The Activity Record
//
// `harness logs` is the surface an operator reads when a run did something
// they cannot explain, so this is where the program list has to survive: the
// event's field onto the entry, redacted like every other string that comes
// out of a transcript, and never carrying a builtin, an env assignment or an
// argument (issue #913).
//
// @joestump-agent 10/31/2026 - Added for stump.wtf/harness#913.

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

func TestToolEntriesCarryTheirPrograms(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	const token = "ghp_SecretTokenValue1234567890abcdef"
	at := func(s int) time.Time { return spawn.Add(time.Duration(s) * time.Second) }
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("s1", at(2), []rt.CrushMessage{
			{Role: "assistant", At: at(3), Parts: rt.ToolCall("c1", "bash", map[string]any{
				"command": "cd web && NPM_TOKEN=" + token + " npm ci 2>&1 | tee install.log",
			})},
			{Role: "tool", At: at(4), Parts: rt.ToolResult("c1", "\n\n<cwd>"+work+"</cwd>")},
			{Role: "assistant", At: at(5), Parts: rt.ToolCall("c2", "view", map[string]any{"file_path": "README.md"})},
			{Role: "tool", At: at(6), Parts: rt.ToolResult("c2", "hi")},
		}))
	a := attribute(t, crushHarness(t, "sweep", work, nil), Window{Start: spawn, End: spawn.Add(time.Minute)})
	entries, errs := Events(context.Background(), a, false, spawn.Add(time.Hour))
	if len(errs) != 0 {
		t.Fatal(errs)
	}

	var shell, read *Entry
	for i := range entries {
		switch entries[i].Tool {
		case "bash":
			shell = &entries[i]
		case "view":
			read = &entries[i]
		}
	}
	if shell == nil || read == nil {
		t.Fatalf("entries = %+v, want a bash call and a read", entries)
	}
	// cd is a builtin and NPM_TOKEN=… is an env assignment: the two programs
	// the pipeline runs are npm and tee.
	if want := []string{"npm", "tee"}; !reflect.DeepEqual(shell.Programs, want) {
		t.Errorf("shell entry programs = %q, want %q", shell.Programs, want)
	}
	for _, p := range shell.Programs {
		if strings.Contains(p, token) || strings.ContainsAny(p, "=/ ") {
			t.Errorf("program %q is not a bare executable name", p)
		}
	}
	if len(read.Programs) != 0 {
		t.Errorf("a read reported programs: %q", read.Programs)
	}
}

// ProgramNames copies the list rather than aliasing the event's slice, so a
// consumer cannot be surprised by a later read of the same session, and drops a
// name that redaction left empty instead of printing a gap.
func TestProgramNamesCopiesAndDropsEmpty(t *testing.T) {
	in := []string{"tea", "", "jq"}
	got := ProgramNames(in)
	if want := []string{"tea", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProgramNames = %q, want %q", got, want)
	}
	in[0] = "mutated"
	if got[0] != "tea" {
		t.Error("ProgramNames aliases the caller's slice")
	}
	if ProgramNames(nil) != nil {
		t.Error("ProgramNames(nil) is not nil")
	}
}
