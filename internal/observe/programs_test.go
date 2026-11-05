package observe

// Programs On Tool Events
//
// agent-trace names the executables a shell tool call runs on
// classify.Event.Programs; the observer is what carries that field to the
// Prometheus collector and the activity view, so this is where a regression
// would show up first: a struct copied field by field, or a redaction that
// drops the slice, and the whole per-program view goes quiet with no error
// anywhere (issue #913).
//
// @joestump-agent 10/31/2026 - Added for stump.wtf/harness#913.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

func TestToolEventsCarryTheirPrograms(t *testing.T) {
	f := newFixture(t, nil)
	f.src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: f.work}, running(start.Add(-time.Hour)))
	var history []rt.CrushMessage
	history = append(history, userSays("check the queue", start.Add(-time.Hour)))
	history = append(history, crushRead("c1", start.Add(-time.Hour))...)
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{
		ID: "sess-1", Created: start.Add(-2 * time.Hour), Updated: start.Add(-time.Hour), Messages: history,
	})

	ch, cancel := f.obs.Subscribe("test", 64)
	defer cancel()
	f.tick(start.Add(5 * time.Second))
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("history delivered at first scan: %v", describe(got))
	}

	const token = "ghp_SecretTokenValue1234567890abcdef"
	rt.AppendCrushMessages(t, f.crushDB(), "sess-1",
		rt.CrushMessage{Role: "assistant", At: start.Add(20 * time.Second),
			Parts: rt.ToolCall("b1", "bash", map[string]any{
				"command": "cd work && GITEA_TOKEN=" + token + " tea pr list | jq -r .title",
			})},
		rt.CrushMessage{Role: "tool", At: start.Add(21 * time.Second), Parts: rt.ToolResult("b1", "no open pull requests")})
	f.tick(start.Add(25 * time.Second))

	got := drain(ch)
	if want := []string{"worker:tool:bash@1"}; !equal(describe(got), want) {
		t.Fatalf("delivered %v, want %v", describe(got), want)
	}
	var shell *Event
	for i := range got {
		if got[i].Tool.Tool == "bash" {
			shell = &got[i]
		}
	}
	if shell == nil {
		t.Fatal("no bash tool event delivered")
	}
	// A builtin (cd) and the env assignment in front of the command are not
	// programs; the two executables the pipeline runs are, in first-seen order.
	if want := []string{"tea", "jq"}; !equal(shell.Tool.Programs, want) {
		t.Errorf("Programs = %q, want %q", shell.Tool.Programs, want)
	}
	for _, p := range shell.Tool.Programs {
		if strings.ContainsAny(p, "=/ ") {
			t.Errorf("program %q is not a bare executable name", p)
		}
		if strings.Contains(p, token) {
			t.Errorf("program %q carries the command's credential", p)
		}
	}
	// The summary keeps going through redact like every other transcript
	// string; the programs must not be the one field that leaks it.
	if strings.Contains(shell.Tool.Summary, token) {
		t.Errorf("summary carries the credential unredacted: %q", shell.Tool.Summary)
	}
}
