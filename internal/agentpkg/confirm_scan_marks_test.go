package agentpkg

// The confirmation's file list: every file that lands in the pin store, each
// marked with whether the content scan read it (issue #956). A list drawn
// from the scanned subset, or one that marks a copied file without saying so,
// is what this rules out.
//
// Governing: ADR-0044, SPEC-0026 REQ-5.

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderReportMarksScanScope(t *testing.T) {
	in := ReportInput{
		Ref:          "spike2/qwatch",
		ManifestRaw:  []byte("[package]\nname = \"qwatch\"\n"),
		Man:          &Manifest{},
		BundledFiles: []string{"README.md", "assets/table.csv", "package.toml", "watch.sh"},
		Scanned:      []string{"README.md", "package.toml"},
	}
	var buf bytes.Buffer
	RenderReport(&buf, in)
	out := buf.String()
	for _, want := range []string{
		"bundled files:\n",
		"  README.md  (" + ScannedMark + ")\n",
		"  package.toml  (" + ScannedMark + ")\n",
		"  assets/table.csv  (" + UnscannedMark + ")\n",
		"  watch.sh  (" + UnscannedMark + ")\n",
		"note: not scanned: assets/table.csv, watch.sh.",
		ScanScopeCovers,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report must contain %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(out), NoGuarantee) {
		t.Errorf("report must still end with the no-guarantee statement:\n%s", out)
	}
}

// Everything scanned: no note, and no file left looking like it was skipped.
func TestRenderReportAllScannedLeavesNoteOut(t *testing.T) {
	in := ReportInput{
		Ref:          "spike2/reviewer",
		ManifestRaw:  []byte("[package]\nname = \"reviewer\"\n"),
		Man:          &Manifest{},
		BundledFiles: []string{"README.md", "package.toml"},
		Scanned:      []string{"README.md", "package.toml"},
	}
	var buf bytes.Buffer
	RenderReport(&buf, in)
	out := buf.String()
	if strings.Contains(out, "note:") {
		t.Errorf("nothing was skipped, so no note belongs in the report:\n%s", out)
	}
	if !strings.Contains(out, "  package.toml  ("+ScannedMark+")\n") {
		t.Errorf("every file must still carry its mark:\n%s", out)
	}
}

// A report built without a scan scope lists every file and claims no scope it
// was not given.
func TestRenderBundledFilesWithoutScanScope(t *testing.T) {
	var buf bytes.Buffer
	RenderBundledFiles(&buf, []string{"package.toml", "watch.sh"}, nil)
	if got := buf.String(); got != "  package.toml\n  watch.sh\n" {
		t.Fatalf("unmarked list wrong: %q", got)
	}
}
