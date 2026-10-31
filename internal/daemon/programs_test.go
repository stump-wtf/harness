package daemon

// Programs On The Wire
//
// The logs op is what `harness logs --json` and the MCP logs tool answer from,
// so the field has to survive the runtrace-to-wire conversion or the whole
// surface stays silent while the daemon still counts the calls correctly
// (issue #913).
//
// @joestump-agent 10/31/2026 - Added for stump.wtf/harness#913.

import (
	"reflect"
	"testing"

	"github.com/stump-wtf/harness/internal/runtrace"
)

func TestAgentEntryCarriesPrograms(t *testing.T) {
	shell := runtrace.Entry{ID: "s1/tool/1", Kind: runtrace.KindTool, Action: "exec",
		Tool: "bash", Summary: "tea pr list | jq -r .title", Programs: []string{"tea", "jq"}}
	got := agentEntry(shell)
	if want := []string{"tea", "jq"}; !reflect.DeepEqual(got.Programs, want) {
		t.Errorf("wire entry programs = %q, want %q", got.Programs, want)
	}

	read := runtrace.Entry{ID: "s1/tool/2", Kind: runtrace.KindTool, Action: "read", Tool: "view"}
	if len(agentEntry(read).Programs) != 0 {
		t.Errorf("a read carries programs: %q", agentEntry(read).Programs)
	}

	mark := runtrace.Entry{ID: "s1/mark/0", Kind: runtrace.KindMark, Action: "user-message", Programs: []string{"sh"}}
	if got := agentEntry(mark); len(got.Programs) != 0 {
		t.Errorf("a mark carried a program list: %q", got.Programs)
	}
}
