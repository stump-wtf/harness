package logview

import (
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// TestFailedEntryCarriesItsExcerpt: the failure note used to say "(failed)" and
// nothing else, which left "why" a question for the raw transcript. The excerpt
// rides in the suffix, so the detail column keeps naming the file or command the
// entry was about.
func TestFailedEntryCarriesItsExcerpt(t *testing.T) {
	line := FormatEntry(protocol.LogEntry{
		ID: "s/tool/1", Kind: protocol.LogEntryTool, Action: "exec", Tool: "bash",
		Summary: "tea issues list", Error: true,
		ErrorExcerpt: "auth failed: the token was rejected",
	})
	if !strings.Contains(line, "(failed)") {
		t.Errorf("line %q lost the failure marker", line)
	}
	if !strings.Contains(line, "auth failed: the token was rejected") {
		t.Errorf("line %q does not say why the call failed", line)
	}
	if !strings.Contains(line, "tea issues list") {
		t.Errorf("line %q lost the command the entry is about", line)
	}

	if got := FormatEntry(protocol.LogEntry{
		ID: "s/tool/2", Kind: protocol.LogEntryTool, Action: "exec", Tool: "bash",
		Summary: "git status", Error: true,
	}); !strings.HasSuffix(strings.TrimRight(got, " "), ")") {
		t.Errorf("an entry with no excerpt grew something it should not: %q", got)
	}
}

// TestExcerptIsClippedForTheTerminal: the excerpt is bounded upstream at 512
// bytes, which is still wider than a failure note should be. The full value
// stays one --json away, the way a long summary is.
func TestExcerptIsClippedForTheTerminal(t *testing.T) {
	line := FormatEntry(protocol.LogEntry{
		ID: "s/tool/1", Kind: protocol.LogEntryTool, Action: "exec", Tool: "bash",
		Summary: "go test ./...", Error: true,
		ErrorExcerpt: strings.Repeat("f", ExcerptWidth+200),
	})
	if strings.Contains(line, strings.Repeat("f", ExcerptWidth+1)) {
		t.Errorf("line %q prints the whole excerpt", line)
	}
	if !strings.Contains(line, "…") {
		t.Errorf("line %q shows no cut marker", line)
	}
}
