package config

// Merge Train Table Tests
//
// Governing tests: SPEC-0025 REQ-1 and REQ-12; #604 — the section parses,
// defaults apply when it is absent, enabled defaults to false, enabled with no
// repos is an error, the table carries no credential, and neither a project
// file nor a harness_d drop-in may carry it. SPEC-0025 REQ-17: `batch` is
// accepted (at least 1) ahead of the batching itself, and every [mergetrain]
// example the docs publish loads as written.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestMergeTrainAbsentTakesDefaults(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.MergeTrain
	if mc.Enabled {
		t.Fatal("merge train enabled with no [mergetrain] table")
	}
	want := core.DefaultMergeTrainConfig()
	if mc.Mode != want.Mode || mc.BaseBranch != "main" || mc.PollInterval != 60*time.Second || mc.CITimeout != 30*time.Minute || mc.Batch != 1 {
		t.Fatalf("defaults = %+v", mc)
	}
}

func TestMergeTrainParses(t *testing.T) {
	src := `
[mergetrain]
enabled = true
mode = "merge"
repos = ["stump.wtf/harness", "stump.wtf/switchboard"]
base_branch = "trunk"
poll_interval = "90s"
ci_timeout = "45m"
batch = 3
sequence_paths = ["docs/adrs", "migrations/"]
forge_base_url = "https://gitea.stump.rocks/"
forge_token_env = "HARNESS_MERGETRAIN_TOKEN"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	mc := cfg.MergeTrain
	if !mc.Enabled || mc.Mode != "merge" || mc.BaseBranch != "trunk" ||
		!slices.Equal(mc.Repos, []string{"stump.wtf/harness", "stump.wtf/switchboard"}) ||
		mc.PollInterval != 90*time.Second || mc.CITimeout != 45*time.Minute || mc.Batch != 3 ||
		!slices.Equal(mc.SequencePaths, []string{"docs/adrs/", "migrations/"}) ||
		mc.ForgeBaseURL != "https://gitea.stump.rocks" || mc.ForgeTokenEnv != "HARNESS_MERGETRAIN_TOKEN" {
		t.Fatalf("parsed = %+v", mc)
	}
}

// SPEC-0025 REQ-18: an explicit empty list disables the sequence check, and an
// absent key keeps the watched defaults. That difference is why the key is a
// pointer, like the other tuning keys.
func TestMergeTrainSequencePathsEmptyDisables(t *testing.T) {
	cfg, err := Parse([]byte("[mergetrain]\nsequence_paths = []\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MergeTrain.SequencePaths) != 0 {
		t.Fatalf("sequence_paths = [] watches %v, want the check disabled", cfg.MergeTrain.SequencePaths)
	}
	absent, err := Parse([]byte("[mergetrain]\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(absent.MergeTrain.SequencePaths, core.DefaultMergeTrainSequencePaths) {
		t.Fatalf("absent key watches %v, want the defaults", absent.MergeTrain.SequencePaths)
	}
}

func TestMergeTrainEnabledDefaultsFalseAndModeReport(t *testing.T) {
	cfg, err := Parse([]byte("[mergetrain]\nrepos = [\"a/b\"]\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MergeTrain.Enabled || cfg.MergeTrain.Mode != "report" {
		t.Fatalf("enabled = %v, mode = %q; want false, report", cfg.MergeTrain.Enabled, cfg.MergeTrain.Mode)
	}
}

func TestMergeTrainRejects(t *testing.T) {
	enabled := "[mergetrain]\nenabled = true\nforge_base_url = \"https://g.example\"\nforge_token_env = \"T\"\n"
	cases := []struct {
		name, src, want string
	}{
		{"enabled, no repos", enabled, `"repos": must list at least one`},
		{"enabled, empty repos", enabled + "repos = []\n", `"repos": must list at least one`},
		{"enabled, no url", "[mergetrain]\nenabled = true\nrepos = [\"a/b\"]\nforge_token_env = \"T\"\n", `"forge_base_url": is required`},
		{"enabled, no token env", "[mergetrain]\nenabled = true\nrepos = [\"a/b\"]\nforge_base_url = \"https://g.example\"\n", `"forge_token_env": is required`},
		{"token key", "[mergetrain]\ntoken = \"abc\"\n", `"token": is not allowed`},
		{"token in token_env", "[mergetrain]\nforge_token_env = \"3f9a1c0de2b7-some-token\"\n", "must be the NAME"},
		{"userinfo in url", "[mergetrain]\nforge_base_url = \"https://u:p@g.example\"\n", "must not carry userinfo"},
		{"relative url", "[mergetrain]\nforge_base_url = \"gitea.stump.rocks\"\n", "absolute http or https"},
		{"bad mode", "[mergetrain]\nmode = \"yolo\"\n", `must be "report" or "merge"`},
		{"bad repo", "[mergetrain]\nrepos = [\"harness\"]\n", `"owner/name"`},
		{"dup repo", "[mergetrain]\nrepos = [\"a/b\", \"a/b\"]\n", "twice"},
		{"fast poll", "[mergetrain]\npoll_interval = \"1s\"\n", "at least 5s"},
		{"bad timeout", "[mergetrain]\nci_timeout = \"soon\"\n", `"ci_timeout"`},
		{"train base", "[mergetrain]\nbase_branch = \"train/1\"\n", "train/"},
		{"zero batch", "[mergetrain]\nbatch = 0\n", `"batch": must be a whole number of at least 1`},
		{"negative batch", "[mergetrain]\nbatch = -2\n", `"batch": must be a whole number of at least 1`},
		{"string batch", "[mergetrain]\nbatch = \"2\"\n", "batch"},
		{"float batch", "[mergetrain]\nbatch = 2.5\n", "batch"},
		{"absolute sequence path", "[mergetrain]\nsequence_paths = [\"/etc\"]\n", "sequence_paths"},
		{"parent hop in sequence path", "[mergetrain]\nsequence_paths = [\"docs/../secrets\"]\n", "sequence_paths"},
		{"empty sequence path", "[mergetrain]\nsequence_paths = [\"  \"]\n", "sequence_paths"},
		{"dup sequence path", "[mergetrain]\nsequence_paths = [\"docs/adrs\", \"docs/adrs/\"]\n", "twice"},
		{"unknown key", "[mergetrain]\nenabeld = true\n", "enabeld"},
		{"duplicate table", "[mergetrain]\n[mergetrain]\n", "already been defined"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.src), "t.toml")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "3f9a1c0de2b7") || strings.Contains(err.Error(), "u:p@")) {
			t.Errorf("%s: error echoes a credential: %v", tc.name, err)
		}
	}
}

func TestMergeTrainErrorNamesItsLine(t *testing.T) {
	_, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n\n[mergetrain]\nenabled = true\nmode = \"yolo\"\n"), "t.toml")
	if err == nil || !strings.Contains(err.Error(), "t.toml:6") {
		t.Fatalf("err = %v, want it located at t.toml:6", err)
	}
}

func TestMergeTrainNotInProjectFile(t *testing.T) {
	_, err := ParseProject([]byte("[mergetrain]\nenabled = true\nrepos = [\"a/b\"]\n"), "harness.toml")
	if err == nil || !strings.Contains(err.Error(), "mergetrain") {
		t.Fatalf("project [mergetrain] err = %v, want a refusal", err)
	}
}

func TestMergeTrainNotInDropIn(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "harness.toml")
	dropins := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("[server]\nharness_d = \"harness.d\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropins, "a.toml"), []byte("[mergetrain]\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(main); err == nil || !strings.Contains(err.Error(), "[mergetrain]") {
		t.Fatalf("drop-in [mergetrain] err = %v, want a refusal", err)
	}
}

// TestMergeTrainDocumentedTablesLoad parses every [mergetrain] example the docs
// publish. The spec's configuration block carried `batch = 1` while the loader
// refused the key, so an operator who copied it lost the whole config — every
// harness, not just the train. A new example that the loader refuses fails
// here instead.
func TestMergeTrainDocumentedTablesLoad(t *testing.T) {
	// Tests run in the package directory; the docs are two levels up.
	root := filepath.Join("..", "..")
	docs := []string{
		"docs/usage/merge-train.md",
		"docs/usage/configuration.md",
		"docs/guides/merge-train.md",
		"docs/openspec/specs/merge-train/design.md",
	}
	sawBatch := false
	for _, doc := range docs {
		data, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		blocks := tomlBlocksWith(string(data), "[mergetrain]")
		if len(blocks) == 0 {
			t.Errorf("%s: no [mergetrain] example found; update this test if it moved", doc)
		}
		for i, b := range blocks {
			if strings.Contains(b, "batch") {
				sawBatch = true
			}
			if _, err := Parse([]byte(b), doc); err != nil {
				t.Errorf("%s: example %d does not load: %v\n%s", doc, i+1, err, b)
			}
		}
	}
	// The check exists for the batch key; prove it still reaches one.
	if !sawBatch {
		t.Error("no documented [mergetrain] example sets batch, so this test no longer covers it")
	}
}

// tomlBlocksWith returns the body of each ```toml fence in md that contains
// marker.
func tomlBlocksWith(md, marker string) []string {
	var out []string
	var b strings.Builder
	in := false
	for _, line := range strings.Split(md, "\n") {
		switch {
		case !in && strings.HasPrefix(line, "```toml"):
			in = true
			b.Reset()
		case in && strings.HasPrefix(line, "```"):
			in = false
			if strings.Contains(b.String(), marker) {
				out = append(out, b.String())
			}
		case in:
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return out
}
