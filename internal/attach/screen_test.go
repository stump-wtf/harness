package attach

// Governing: SPEC-0002 REQ "Attach Session" — the screen snapshot is a repaint
// of the current x/vt screen; a client that writes it ends up with the harness
// screen. This asserts the renderer reproduces written content.

import (
	"bytes"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func TestRenderScreenReproducesContent(t *testing.T) {
	term := vt.NewTerminal(20, 5)
	_, _ = term.Write([]byte("first line\r\nsecond line"))

	out := renderScreen(term)
	if !bytes.HasPrefix(out, snapshotPrefix) {
		t.Errorf("snapshot missing clear+home prefix: %q", out[:min(len(out), 12)])
	}
	for _, want := range []string{"first line", "second line"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("snapshot missing %q; got %q", want, out)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
