package daemon

import (
	"testing"

	"github.com/stump-wtf/harness/internal/runtrace"
)

// TestAgentEntryCarriesTheExcerpt: the wire form is the only copy a client
// ever sees, and a field dropped in this mapping is invisible to every renderer
// and to `harness logs --json` at once.
func TestAgentEntryCarriesTheExcerpt(t *testing.T) {
	failed := agentEntry(runtrace.Entry{
		ID: "s/tool/1", Kind: runtrace.KindTool, Action: "exec", Tool: "bash",
		Error: true, ErrorExcerpt: "auth failed: the token was rejected",
	})
	if !failed.Error {
		t.Error("a failing entry reached the wire as a success")
	}
	if failed.ErrorExcerpt != "auth failed: the token was rejected" {
		t.Errorf("wire excerpt = %q, want the excerpt the entry carried", failed.ErrorExcerpt)
	}

	if ok := agentEntry(runtrace.Entry{ID: "s/tool/2", Kind: runtrace.KindTool}); ok.ErrorExcerpt != "" {
		t.Errorf("a successful entry carries excerpt %q", ok.ErrorExcerpt)
	}
}
