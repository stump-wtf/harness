// Chatroom text fitting
//
// The chatroom's layout is line-based: each event contributes a known number of
// rows and the view clips to that count, so anything that can smuggle an extra
// row or a broken rune into a summary is a frame-overflow bug.
//
// @joestump 08/21/2026 - Added alongside the truncation fixes in PR #248.
// @joestump-agent 08/21/2026 - The Init/Stop lifecycle tests that shared this
// file went with the chatroom's watcher; the TUI owns the only one now and
// internal/tui/watcher_test.go covers it.

package chatroom

import (
	"testing"
)

// truncateShort clips by rune, not by byte: transcript summaries carry paths,
// quoted output and box drawing, and a byte slice lands mid-rune.
func TestTruncateShortIsRuneSafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"ascii under limit", "hello", 10, "hello"},
		{"ascii over limit", "hello world", 8, "hello w…"},
		{"trims first", "  spaced  ", 10, "spaced"},
		{"multibyte under limit", "héllo wörld", 20, "héllo wörld"},
		{"multibyte over limit", "héllo wörld ünïcode", 8, "héllo w…"},
		{"cjk over limit", "编辑文件内容示例", 4, "编辑文…"},
		{"zero width", "anything", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateShort(tc.in, tc.n)
			if got != tc.want {
				t.Fatalf("truncateShort(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			for _, r := range got {
				if r == '�' {
					t.Fatalf("truncateShort(%q, %d) = %q — split a multi-byte rune", tc.in, tc.n, got)
				}
			}
		})
	}
}
