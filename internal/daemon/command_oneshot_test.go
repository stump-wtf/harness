package daemon

// Command one-shot template skips over the protocol: a run whose argv template
// lacked a required value reaches `harness runs` (and --json) as a skip with
// its reason and the path's name.
//
// Governing: ADR-0023; SPEC-0017 REQ-11 "Rendering", REQ-17 (the SPEC-0008
// and SPEC-0014 skip-reason amendment).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestTemplateUnresolvedSkipOnTheWire(t *testing.T) {
	// A scheduled command harness with a required {{run.source}} is refused
	// at load; the hand-built config here is what makes a manual trigger
	// (which has no source) reach the render step.
	h := core.Harness{
		Name:      "report",
		Adapter:   core.AdapterCommand,
		Argv:      []string{"/bin/echo", "{{run.source}}"},
		Backend:   core.BackendNative,
		Workdir:   t.TempDir(),
		Restart:   core.RestartNo,
		Schedule:  "0 3 * * *",
		Timeout:   45 * time.Minute,
		OnOverlap: core.OverlapSkip,
		KeepRuns:  core.DefaultKeepRuns,
	}
	td, _, _ := newJobsDaemon(t, h)
	c := td.dial(t, nil)

	if _, err := c.Trigger("report"); err != nil {
		t.Fatal(err)
	}
	runs := waitRunsOver(t, c, "report", finishedN(1))
	r := runs[0]
	if r.Outcome != "skipped" || r.Reason != "template_unresolved" || r.MissingPath != "run.source" {
		t.Fatalf("run on the wire = %+v, want skipped / template_unresolved / run.source", r)
	}
	if r.ExitCode != nil {
		t.Errorf("a skipped run carries exit code %d", *r.ExitCode)
	}
	rd, err := c.Runs("report", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rd)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reason":"template_unresolved"`, `"missing_path":"run.source"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("runs --json payload lacks %s: %s", want, b)
		}
	}
}
