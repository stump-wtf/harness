package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadPromptFile covers the shared reader both the config parser's eager
// check and the supervisor's spawn-time read go through. Sharing it is what
// keeps the two from drifting: a file the load accepts must be one the spawn
// can run. Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
func TestReadPromptFile(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("reads and trims", func(t *testing.T) {
		p := write("ok.md", "\n  sweep the fleet  \n\n")
		got, err := ReadPromptFile(p)
		if err != nil {
			t.Fatalf("ReadPromptFile: %v", err)
		}
		if got != "sweep the fleet" {
			t.Errorf("got %q, want the trimmed body", got)
		}
	})

	t.Run("multi-line bodies survive intact", func(t *testing.T) {
		// The whole point of the feature: a specification too long for one
		// TOML line. Only the surrounding whitespace is trimmed.
		body := "# Sweep\n\nDo the thing.\n\n- and the other thing\n"
		p := write("long.md", body)
		got, err := ReadPromptFile(p)
		if err != nil {
			t.Fatalf("ReadPromptFile: %v", err)
		}
		if got != strings.TrimSpace(body) {
			t.Errorf("got %q, want the body with only surrounding whitespace trimmed", got)
		}
	})

	errCases := []struct {
		name    string
		path    string
		wantSub string
	}{
		{"missing", filepath.Join(dir, "nope.md"), "does not exist"},
		{"directory", dir, "is a directory"},
		{"empty", write("empty.md", ""), "is empty"},
		{"whitespace only", write("blank.md", " \n\t\n"), "is empty"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadPromptFile(tc.path)
			if err == nil {
				t.Fatalf("ReadPromptFile(%q) succeeded, want an error", tc.path)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error %q does not name the path", err)
			}
		})
	}
}

// TestIsAgent: either prompt source marks an agent one-shot. A bare
// Prompt != "" check would misclassify a prompt_file harness as a cmd harness
// and hand it the always-restart default.
func TestIsAgent(t *testing.T) {
	tests := []struct {
		name string
		h    Harness
		want bool
	}{
		{"inline prompt", Harness{Prompt: "sweep"}, true},
		{"prompt file", Harness{PromptFile: "/tmp/sweep.md"}, true},
		{"cmd harness", Harness{Args: []string{"-i"}}, false},
		{"bare harness", Harness{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.h.IsAgent(); got != tt.want {
				t.Errorf("IsAgent() = %v, want %v", got, tt.want)
			}
		})
	}
}
