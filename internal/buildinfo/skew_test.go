package buildinfo

// Governing: stump.wtf/harness#181 — the daemon is deliberately long-lived
// while the client is rebuilt constantly, and proto version can never detect
// build drift. SkewNotice is the single voice for doctor, daemon-info, and the
// TUI banner; these pin what it decides is worth saying.

import (
	"strings"
	"testing"
)

func TestSkewNotice(t *testing.T) {
	tests := []struct {
		name         string
		daemon       string
		client       string
		wantEmpty    bool
		wantContains string
		notContains  string
	}{
		{
			name:         "daemon behind — the case from the issue",
			daemon:       "v0.1.0-90-gb9addf9",
			client:       "v0.1.0-147-g775e92f",
			wantContains: "57 commits behind",
		},
		{
			name:         "daemon ahead",
			daemon:       "v0.1.0-200-gabc1234",
			client:       "v0.1.0-147-g775e92f",
			wantContains: "newer than client",
		},
		{
			name:      "same build",
			daemon:    "v0.1.0-147-g775e92f",
			client:    "v0.1.0-147-g775e92f",
			wantEmpty: true,
		},
		{
			name:      "dev client next to any daemon is the dev workflow, not skew",
			daemon:    "v0.1.0-147-g775e92f",
			client:    "dev",
			wantEmpty: true,
		},
		{
			name:      "dev daemon next to any client likewise",
			daemon:    "dev",
			client:    "v0.1.0-147-g775e92f",
			wantEmpty: true,
		},
		{
			name:      "unversioned sides stay silent",
			daemon:    "",
			client:    "v0.1.0-147-g775e92f",
			wantEmpty: true,
		},
		{
			name:         "different release lines have no ordering — generic differ",
			daemon:       "v0.1.0-90-gb9addf9",
			client:       "v0.2.0-3-geeeeee0",
			wantContains: "different builds",
			notContains:  "commits behind",
		},
		{
			name:         "non-describe versions fall back to generic differ",
			daemon:       "v1.2.3",
			client:       "v1.2.4",
			wantContains: "different builds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SkewNotice(tt.daemon, tt.client)
			if tt.wantEmpty {
				if got != "" {
					t.Errorf("SkewNotice(%q, %q) = %q, want silence", tt.daemon, tt.client, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("SkewNotice(%q, %q) is empty, want %q", tt.daemon, tt.client, tt.wantContains)
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("SkewNotice(%q, %q) = %q, want it to mention %q", tt.daemon, tt.client, got, tt.wantContains)
			}
			if tt.notContains != "" && strings.Contains(got, tt.notContains) {
				t.Errorf("SkewNotice(%q, %q) = %q, must not mention %q", tt.daemon, tt.client, got, tt.notContains)
			}
		})
	}
}
