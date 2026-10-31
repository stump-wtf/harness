package runtrace

// Error Excerpts
//
// A failed tool call reached the post-mortem view as "this call failed", with
// the why left in the raw transcript. agent-trace can keep a bounded excerpt of
// an errored result on the classified event (classify.Options.ErrorExcerptBytes)
// and runs a caller-supplied Redact over the whole cleaned result before the cut
// is made, so a secret straddling the elision point is still whole when it is
// matched. Harness set neither field, so nothing was ever kept.
//
// The options below mirror the ones an agent-trace adapter falls back to when it
// carries none. FileExists, HomeDir and TmpDir decide which weak targets count
// as touched, so leaving them out would silently keep every missing path and
// change what the activity view lists as a target — the same trap agent-trace
// closed in its own #116. Only the two excerpt fields are harness's to choose.
//
// The budget is a constant, not configuration: no [observer] table exists in
// harness.toml, and inventing one to hold a single number is a larger decision
// than this issue is.
//
// Governing: ADR-0033 (agent-trace is the source of run facts), ADR-0008
// (secrets stay out of harness output), issue #914.
//
// @joestump-agent 10/31/2026 - Added for harness#914.

import (
	"os"
	"path/filepath"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/agent-trace/tail"
)

// ErrorExcerptBytes is how much of a failed tool call's output an entry keeps.
// 512 bytes covers the leading error line and the trailing summary of a failed
// test run, which is the shape of the answer an operator is after, and it is
// small enough to ride along with every failing entry a post-mortem lists.
const ErrorExcerptBytes = 512

// excerptOptions is the classify configuration every adapter harness parses with
// runs under.
func excerptOptions() *classify.Options {
	home, _ := os.UserHomeDir()
	return &classify.Options{
		FileExists:        pathExists,
		HomeDir:           home,
		TmpDir:            os.TempDir(),
		ErrorExcerptBytes: ErrorExcerptBytes,
		Redact:            redact.Redact,
	}
}

// pathExists is the os.Stat check an agent-trace adapter applies when it is
// given no options at all, reproduced so turning the excerpt on cannot change
// which targets an event reports.
func pathExists(cwd, rel string) bool {
	if cwd == "" || rel == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(cwd, filepath.FromSlash(rel)))
	return err == nil
}

// applyExcerptOptions hands the excerpt settings to every adapter that accepts
// options. One Options value per adapter, the way agent-trace's own watcher does
// it: one shared pointer would let one adapter see another's copy.
func applyExcerptOptions(adapters []tail.Adapter) {
	for _, a := range adapters {
		if s, ok := a.(tail.OptionsSetter); ok {
			s.SetOptions(excerptOptions())
		}
	}
}
