package runtrace

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/agent-trace/tail"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

// TestExcerptOnAFailingCallIsMaskedAndBounded is issue #914's acceptance: the
// post-mortem says why a tool call failed without the operator opening the
// transcript, and the reason it says never carries the credential the command
// printed. The token is generated here rather than written out, the convention
// the redact package sets, so no fixture holds a live-looking secret.
func TestExcerptOnAFailingCallIsMaskedAndBounded(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	token := "ghp_" + strings.Repeat("a1B9", 8)
	at := func(s int) time.Time { return spawn.Add(time.Duration(s) * time.Second) }

	failing := "auth failed: the token was rejected\nGITEA_TOKEN=" + token + "\n" +
		strings.Repeat("retrying the request against the next page\n", 60) +
		"Exit code 1\n\n<cwd>" + work + "</cwd>"
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("s1", at(2),
			[]rt.CrushMessage{
				{Role: "assistant", At: at(3), Parts: rt.ToolCall("c1", "bash", map[string]any{"command": "tea issues list"})},
				{Role: "tool", At: at(4), Parts: rt.ToolResult("c1", failing)},
				{Role: "assistant", At: at(5), Parts: rt.ToolCall("c2", "bash", map[string]any{"command": "git status"})},
				{Role: "tool", At: at(6), Parts: rt.ToolResult("c2", "On branch main\n\n<cwd>"+work+"</cwd>")},
			},
		))

	a := attribute(t, crushHarness(t, "sweep", work, nil), Window{Start: spawn, End: spawn.Add(time.Minute)})
	entries, errs := Events(context.Background(), a, false, spawn.Add(time.Hour))
	if len(errs) != 0 {
		t.Fatal(errs)
	}

	var failed, ok []Entry
	for _, e := range entries {
		if e.Kind != KindTool {
			continue
		}
		if e.Error {
			failed = append(failed, e)
		} else {
			ok = append(ok, e)
		}
	}
	if len(failed) != 1 || len(ok) != 1 {
		t.Fatalf("entries = %+v, want one failing tool call and one that succeeded", entries)
	}

	ex := failed[0].ErrorExcerpt
	if ex == "" {
		t.Fatal("a failing call carries no excerpt, so the post-mortem still says only that it failed")
	}
	if len(ex) > ErrorExcerptBytes {
		t.Errorf("excerpt is %d bytes, want at most %d", len(ex), ErrorExcerptBytes)
	}
	if strings.Contains(ex, token) {
		t.Errorf("excerpt carries the credential the command printed: %q", ex)
	}
	if !strings.Contains(ex, redact.Mask) {
		t.Errorf("excerpt %q holds no %s, so the masker never ran on it", ex, redact.Mask)
	}
	if !strings.Contains(ex, "auth failed") {
		t.Errorf("excerpt %q lost the failure itself, so it answers nothing", ex)
	}
	if ok[0].ErrorExcerpt != "" {
		t.Errorf("a successful call carries excerpt %q; only failures keep one", ok[0].ErrorExcerpt)
	}
}

// TestExcerptOptionsKeepWhatTheyReplace: the two excerpt fields are set on an
// Options struct that replaces the one an adapter would otherwise fall back to,
// so the three fields that decide which targets an event reports have to be
// filled in as well, and Redact has to be set or the excerpt reaches the view
// unmasked.
func TestExcerptOptionsKeepWhatTheyReplace(t *testing.T) {
	o := excerptOptions()
	if o.ErrorExcerptBytes != ErrorExcerptBytes {
		t.Errorf("ErrorExcerptBytes = %d, want %d", o.ErrorExcerptBytes, ErrorExcerptBytes)
	}
	if o.Redact == nil {
		t.Error("Redact is nil: the excerpt would be cut from the raw result")
	}
	if o.FileExists == nil {
		t.Error("FileExists is nil: every missing weak target would start counting as touched")
	}
	if o.HomeDir == "" || o.TmpDir == "" {
		t.Errorf("HomeDir %q and TmpDir %q: outside-scope classification would lose its anchors", o.HomeDir, o.TmpDir)
	}
}

// TestSourcesAdaptersAcceptTheOptions is the wiring the post-mortem depends on:
// the adapters Sources hands out are the ones whose Parse builds the events the
// activity view prints, so an adapter that accepted no options would produce a
// post-mortem with no excerpts and nothing else would say so.
func TestSourcesAdaptersAcceptTheOptions(t *testing.T) {
	work := t.TempDir()
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("s1", spawn, read("c1", "m.go", spawn)))
	adapters, err := Sources(crushHarness(t, "sweep", work, nil))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	if len(adapters) == 0 {
		t.Fatal("Sources returned no adapters")
	}
	for _, a := range adapters {
		if _, ok := a.(tail.OptionsSetter); !ok {
			t.Errorf("%T accepts no classify options, so its events can carry no excerpt", a)
		}
	}
}
