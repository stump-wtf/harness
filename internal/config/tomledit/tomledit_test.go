// Governing: ADR-0044; SPEC-0026 REQ-1, REQ-6, REQ-8, REQ-9, REQ-11 (write
// path); issue #810.
package tomledit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const sample = `# Global harness config.

[daemon]
watch_config = true # keep

[harness.build]
harness = "generic"
args = [
  "make",
  "test", # the important one
]
# trailing comment belongs to build

[harness.deploy]
harness = "generic"
remote = "https://example.invalid/build.git"
`

func TestAddStablePreservesEverything(t *testing.T) {
	e := New([]byte(sample))
	if err := e.AddStable("acme", [][2]any{
		{"remote", "https://forge.example/acme/stable.git"},
		{"public", true},
	}); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "[stable.acme]\nremote = \"https://forge.example/acme/stable.git\"\npublic = true\n") {
		t.Errorf("stable table missing or malformed:\n%s", out)
	}
	for _, want := range []string{"# Global harness config.", "watch_config = true # keep", "\"test\", # the important one", "[harness.deploy]"} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %q", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "public = true") {
		t.Errorf("stable table not appended at end:\n%s", out)
	}
}

func TestAddStableRefusesDuplicate(t *testing.T) {
	e := New([]byte("[stable.acme]\nremote = \"x\"\n"))
	err := e.AddStable("acme", [][2]any{{"remote", "y"}})
	if !errors.Is(err, ErrDuplicateTable) {
		t.Fatalf("want ErrDuplicateTable, got %v", err)
	}
}

func TestRemoveHarnessHandlesMultiLineArrayAndComments(t *testing.T) {
	e := New([]byte(sample))
	if err := e.RemoveHarness("build"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Contains(out, "harness.build") || strings.Contains(out, "the important one") || strings.Contains(out, "trailing comment") {
		t.Errorf("build table not fully removed:\n%s", out)
	}
	if !strings.Contains(out, "[harness.deploy]") || !strings.Contains(out, "[daemon]") {
		t.Errorf("sibling tables lost:\n%s", out)
	}
}

func TestRemoveHarnessBareSpelling(t *testing.T) {
	e := New([]byte("[build]\nharness = \"generic\"\n\n[harness.deploy]\nharness = \"generic\"\n"))
	if err := e.RemoveHarness("build"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(e.Bytes()), "[build]") {
		t.Errorf("bare table not removed:\n%s", e.Bytes())
	}
}

func TestRemoveStableUnknown(t *testing.T) {
	e := New([]byte(sample))
	if err := e.RemoveStable("ghost"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("want ErrTableNotFound, got %v", err)
	}
}

func TestSetKeyReplacesInPlace(t *testing.T) {
	e := New([]byte(sample))
	if err := e.SetHarnessKey("deploy", "remote", "https://other.example/x.git"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "remote = \"https://other.example/x.git\"") {
		t.Errorf("value not replaced:\n%s", out)
	}
	if i, j := strings.Index(out, "remote = \"https://other"), strings.LastIndex(out, "[harness.deploy]"); j > i {
		t.Errorf("key not edited in place (moved to end?):\n%s", out)
	}
}

func TestSetKeyAppendsInsideTable(t *testing.T) {
	e := New([]byte(sample))
	if err := e.SetHarnessKey("deploy", "source", "acme/pkg@"+strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	src := "source = \"acme/pkg@" + strings.Repeat("0", 40) + "\""
	if !strings.Contains(out, src) {
		t.Fatalf("source key not written:\n%s", out)
	}
	// The appended key must sit INSIDE the deploy table, not after a later
	// section: deploy is the last table here, so it must be the final line.
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), src) {
		t.Errorf("key appended outside the table:\n%s", out)
	}
	// A second set replaces it in place and does not duplicate.
	if err := e.SetHarnessKey("deploy", "source", "acme/pkg@"+strings.Repeat("1", 40)); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(e.Bytes()), "source ="); n != 1 {
		t.Errorf("source appears %d times, want 1", n)
	}
}

func TestArrayTableRefused(t *testing.T) {
	e := New([]byte("[[stable.acme]]\nremote = \"x\"\n"))
	if err := e.RemoveStable("acme"); !errors.Is(err, ErrArrayTable) {
		t.Fatalf("want ErrArrayTable, got %v", err)
	}
}

func TestDuplicateTableRefused(t *testing.T) {
	e := New([]byte("[harness.build]\nharness = \"crush\"\n\n[harness.build]\nharness = \"crush\"\n"))
	if err := e.SetHarnessKey("build", "model", "opus"); !errors.Is(err, ErrDuplicateTable) {
		t.Fatalf("want ErrDuplicateTable, got %v", err)
	}
}

// TestSetKeyReplacesMultiLineValue: setting a key whose existing value is a
// multi-line array must consume the whole value — replacing only the key's
// first line would leave the continuation lines orphaned below the new
// single-line value, and the file would no longer parse.
func TestSetKeyReplacesMultiLineValue(t *testing.T) {
	e := New([]byte(sample))
	if err := e.SetHarnessKey("build", "args", []string{"make", "check"}); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Count(out, "\"make\"") != 1 || strings.Contains(out, "\"test\",") {
		t.Errorf("old array lines survived the replacement:\n%s", out)
	}
	var cfg struct {
		Harness map[string]struct {
			Args []string
		}
	}
	if _, err := toml.Decode(out, &cfg); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, out)
	}
	got := cfg.Harness["build"].Args
	if len(got) != 2 || got[0] != "make" || got[1] != "check" {
		t.Errorf("args = %v, want [make check]", got)
	}
}

// TestSetKeyKeepsTrailingComment: a comment after the value belongs to the
// line, not the value, and an in-place replacement preserves it.
func TestSetKeyKeepsTrailingComment(t *testing.T) {
	e := New([]byte("[stable.acme]\nremote = \"https://old.example/s.git\" # pinned mirror\n"))
	if err := e.SetStableKey("acme", "remote", "https://new.example/s.git"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "remote = \"https://new.example/s.git\" # pinned mirror") {
		t.Errorf("trailing comment lost:\n%s", out)
	}
}

// TestRemoveKeepsMultiLineStringBlankLines: the blank-line tidy after a
// removal touches only the seam the cut creates — never a whole-file
// replace, which would rewrite blank lines inside a multi-line string in a
// table the removal never went near.
func TestRemoveKeepsMultiLineStringBlankLines(t *testing.T) {
	doc := "[stable.one]\nremote = \"https://a.example/s.git\"\n\n" +
		"[harness.notes]\nharness = \"generic\"\nprompt = \"\"\"\npara one\n\n\npara three\n\"\"\"\n\n" +
		"[stable.two]\nremote = \"https://b.example/s.git\"\n"
	e := New([]byte(doc))
	if err := e.RemoveStable("one"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "para one\n\n\npara three") {
		t.Errorf("blank lines inside the multi-line string were rewritten:\n%s", out)
	}
	// The tidy still does its real job at the seam: no triple newline may
	// appear OUTSIDE the string.
	outside := strings.ReplaceAll(out, "para one\n\n\npara three", "")
	if strings.Contains(outside, "\n\n\n") {
		t.Errorf("seam left a doubled blank line:\n%s", outside)
	}
	var cfg struct {
		Stable map[string]struct{ Remote string }
	}
	if _, err := toml.Decode(out, &cfg); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, out)
	}
	if _, ok := cfg.Stable["two"]; !ok {
		t.Errorf("stable.two lost by the removal:\n%s", out)
	}
}

func TestMultiLineStringBracketIsNotHeader(t *testing.T) {
	// The `"""…[not a header]…"""` body must not end the build table early:
	// the whole table is removed, and a following table survives.
	e := New([]byte("[harness.build]\nharness = \"crush\"\nsystem_prompt_file = \"\"\"\n[not a header]\n\"\"\"\n\n[harness.deploy]\nharness = \"crush\"\n"))
	if err := e.RemoveHarness("build"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Contains(out, "[not a header]") || strings.Contains(out, "harness.build") {
		t.Errorf("build table not fully removed:\n%s", out)
	}
	if !strings.Contains(out, "[harness.deploy]") {
		t.Errorf("deploy table lost:\n%s", out)
	}
}

func TestValueEncoding(t *testing.T) {
	e := New([]byte(""))
	if err := e.AddHarness("x", [][2]any{
		{"str", `he said "hi"\ go`},
		{"list", []string{"a", "b c"}},
		{"flag", false},
		// An integer stays an integer: restart_delay is a count of seconds,
		// and quoting it would make the loader reject it.
		{"seconds", 30},
		{"wide", int64(60)},
	}); err != nil {
		t.Fatal(err)
	}
	want := "[harness.x]\nstr = \"he said \\\"hi\\\"\\\\ go\"\nlist = [\"a\", \"b c\"]\nflag = false\nseconds = 30\nwide = 60\n"
	if string(e.Bytes()) != want {
		t.Errorf("got:\n%s\nwant:\n%s", e.Bytes(), want)
	}
	if err := e.AddHarness("y", [][2]any{{"bad", 42.5}}); err == nil {
		t.Error("unsupported value type accepted")
	}
}

func TestSaveIsAtomicAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddStable("acme", [][2]any{{"remote", "https://x"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("[stable.acme]")) {
		t.Error("saved file lost the new table")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %d files, want 1", len(entries))
	}
}

// TestAddHarnessRefusesBareSpelling: the loader registers [harness.build]
// and bare [build] as the same harness (duplicate harness "build"), so an
// add that only checked the prefixed spelling could append a table the
// daemon refuses to load.
func TestAddHarnessRefusesBareSpelling(t *testing.T) {
	e := New([]byte("[build]\nharness = \"generic\"\n\n[daemon]\nwatch_config = true\n"))
	err := e.AddHarness("build", [][2]any{{"harness", "generic"}})
	if !errors.Is(err, ErrDuplicateTable) {
		t.Fatalf("want ErrDuplicateTable, got %v", err)
	}
	if string(e.Bytes()) != "[build]\nharness = \"generic\"\n\n[daemon]\nwatch_config = true\n" {
		t.Errorf("refused add still modified the file:\n%s", e.Bytes())
	}
}

// TestMultiLineNestedArrayIsNotHeader: a line starting with '[' inside a
// multi-line array — a nested array's element, an empty array — is shaped
// exactly like a table header. Without bracket-depth tracking the scanner
// took it for one, ended the table early, and an in-place set corrupted the
// value.
func TestMultiLineNestedArrayIsNotHeader(t *testing.T) {
	doc := "[harness.build]\nharness = \"generic\"\nmatrix = [\n  [\"a\", \"b\"],\n  [],\n]\n\n[harness.deploy]\nharness = \"generic\"\n"
	e := New([]byte(doc))
	if err := e.SetHarnessKey("build", "model", "opus"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if strings.Count(out, "[\"a\", \"b\"],") != 1 || strings.Count(out, "[],") != 1 {
		t.Errorf("matrix value damaged by the set:\n%s", out)
	}
	var cfg struct {
		Harness map[string]struct {
			Matrix [][]string
			Model  string
		}
	}
	if _, err := toml.Decode(out, &cfg); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, out)
	}
	if cfg.Harness["build"].Model != "opus" || len(cfg.Harness["build"].Matrix) != 2 {
		t.Errorf("build = %+v", cfg.Harness["build"])
	}
	if _, ok := cfg.Harness["deploy"]; !ok {
		t.Errorf("deploy table lost:\n%s", out)
	}
}

// TestSetKeyRefusesHeaderInjection: keys and table names are written into
// the file's bytes, so anything outside the name grammar is refused — a
// crafted key would otherwise smuggle a whole table (e.g. [mcp.*]) past the
// narrow-API guarantee of REQ-11.
func TestSetKeyRefusesHeaderInjection(t *testing.T) {
	e := New([]byte("[stable.acme]\nremote = \"x\"\n"))
	before := string(e.Bytes())
	for _, key := range []string{"remote = 1\n[mcp.server]", "a=b", "with space", ""} {
		if err := e.SetStableKey("acme", key, "y"); err == nil {
			t.Errorf("key %q accepted", key)
		}
	}
	if err := e.AddStable("evil]\n[mcp.x]", [][2]any{{"remote", "y"}}); err == nil {
		t.Error("table name with ']' accepted")
	}
	if err := e.AddStable("UPPER", [][2]any{{"remote", "y"}}); err == nil {
		t.Error("stable name outside the SPEC-0026 pattern accepted")
	}
	if string(e.Bytes()) != before {
		t.Errorf("refused writes modified the file:\n%s", e.Bytes())
	}
}

// TestSetKeyKeepsOriginalKeyText: the replacement reuses the file's own key
// text, so a quoted key keeps its quotes and the line keeps its indentation.
func TestSetKeyKeepsOriginalKeyText(t *testing.T) {
	e := New([]byte("[harness.a]\n  \"remote\" = \"old\" # keep\n"))
	if err := e.SetHarnessKey("a", "remote", "new"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	if !strings.Contains(out, "  \"remote\" = \"new\" # keep") {
		t.Errorf("quoted key or indentation rewritten:\n%s", out)
	}
	var cfg struct {
		Harness map[string]struct {
			Remote string
		}
	}
	if _, err := toml.Decode(out, &cfg); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, out)
	}
	if cfg.Harness["a"].Remote != "new" {
		t.Errorf("remote = %q", cfg.Harness["a"].Remote)
	}
}

// TestSetKeyAppendLandsBeforeTrailingComment: an appended key belongs with
// the table's key block, not after the comment lines that trail it.
func TestSetKeyAppendLandsBeforeTrailingComment(t *testing.T) {
	e := New([]byte("[harness.a]\nharness = \"generic\"\n\n# tuning notes\n"))
	if err := e.SetHarnessKey("a", "model", "opus"); err != nil {
		t.Fatal(err)
	}
	out := string(e.Bytes())
	model := strings.Index(out, "model = \"opus\"")
	notes := strings.Index(out, "# tuning notes")
	if model < 0 || notes < 0 || model > notes {
		t.Errorf("appended key did not land before the trailing comment:\n%s", out)
	}
	var cfg struct {
		Harness map[string]struct {
			Model string
		}
	}
	if _, err := toml.Decode(out, &cfg); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, out)
	}
}

// TestEncodeStringEscapesControlCharacters: bytes below 0x20 (and 0x7f) are
// illegal raw inside a TOML basic string; writing them unescaped would
// produce a file the loader refuses.
func TestEncodeStringEscapesControlCharacters(t *testing.T) {
	e := New([]byte(""))
	if err := e.AddHarness("x", [][2]any{{"label", "\x01b\bf\f"}}); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Harness map[string]struct {
			Label string
		}
	}
	if _, err := toml.Decode(string(e.Bytes()), &cfg); err != nil {
		t.Fatalf("encoded value not valid TOML: %v\n%s", err, e.Bytes())
	}
	if cfg.Harness["x"].Label != "\x01b\bf\f" {
		t.Errorf("label = %q", cfg.Harness["x"].Label)
	}
}

func TestEditedFileStillParses(t *testing.T) {
	// Whatever the editor does, the result must remain valid TOML with the
	// expected shape — the whole point of a write path the loader trusts.
	e := New([]byte(sample))
	_ = e.AddStable("acme", [][2]any{{"remote", "https://forge.example/acme.git"}, {"public", true}})
	_ = e.SetHarnessKey("build", "model", "opus")
	_ = e.RemoveHarness("deploy")
	_ = e.SetStableKey("acme", "public", false)
	var doc map[string]any
	if _, err := toml.Decode(string(e.Bytes()), &doc); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, e.Bytes())
	}
	h := doc["harness"].(map[string]any)["build"].(map[string]any)
	if h["model"] != "opus" {
		t.Errorf("build.model = %v", h["model"])
	}
	st := doc["stable"].(map[string]any)["acme"].(map[string]any)
	if st["public"] != false || st["remote"] != "https://forge.example/acme.git" {
		t.Errorf("stable.acme = %v", st)
	}
	if _, exists := doc["harness"].(map[string]any)["deploy"]; exists {
		t.Error("deploy still present")
	}
}

// TestSetKeySkipsValueContinuationLines: a multi-line value's continuation
// lines are never key lines, even when one reads like one once its quotes
// are stripped — `"model=opus",` inside an array, `model = "inside"` inside a
// multi-line string. Matching one would splice the new value into the middle
// of the array (key present further down) or of the string (key absent).
func TestSetKeySkipsValueContinuationLines(t *testing.T) {
	const values = `[harness.reviewer]
args = [
  "model=opus",
  "--verbose",
]
prompt = """
model = "inside"
"""
`
	for _, tc := range []struct {
		name, src, want string
	}{
		{
			name: "key present after the values",
			src:  values + "model = \"big\"\n",
			want: values + "model = \"small\"\n",
		},
		{
			name: "key absent",
			src:  values,
			want: values + "model = \"small\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New([]byte(tc.src))
			if err := e.SetHarnessKey("reviewer", "model", "small"); err != nil {
				t.Fatal(err)
			}
			out := string(e.Bytes())
			if out != tc.want {
				t.Fatalf("set touched more than the real key:\n%s\nwant:\n%s", out, tc.want)
			}
			var cfg struct {
				Harness map[string]struct {
					Args   []string
					Prompt string
					Model  string
				}
			}
			if _, err := toml.Decode(out, &cfg); err != nil {
				t.Fatalf("edited file no longer parses: %v\n%s", err, out)
			}
			got := cfg.Harness["reviewer"]
			if len(got.Args) != 2 || got.Args[0] != "model=opus" || got.Prompt != "model = \"inside\"\n" || got.Model != "small" {
				t.Errorf("reviewer = %+v", got)
			}
		})
	}
}

// RemoveHarnessKey deletes exactly one key line — a multi-line value goes
// whole — and everything else stays byte-identical.
func TestRemoveHarnessKey(t *testing.T) {
	src := `top = "kept"

[harness.reviewer]
model = "big"
args = [
  "a",
  "b",
]
enabled = true

[other]
x = 1
`
	ed := New([]byte(src))
	if err := ed.RemoveHarnessKey("reviewer", "args"); err != nil {
		t.Fatal(err)
	}
	want := `top = "kept"

[harness.reviewer]
model = "big"
enabled = true

[other]
x = 1
`
	if string(ed.Bytes()) != want {
		t.Fatalf("removal must take the whole multi-line value and nothing else:\n%s", ed.Bytes())
	}

	// A continuation line that reads like the key — `  "model=opus",` inside
	// the array — is not the key: the removal takes the real `model` line
	// below it, and the array survives whole.
	src2 := `top = "kept"

[harness.reviewer]
args = [
  "model=opus",
  "--verbose",
]
enabled = true
model = "big"

[other]
x = 1
`
	ed2 := New([]byte(src2))
	if err := ed2.RemoveHarnessKey("reviewer", "model"); err != nil {
		t.Fatal(err)
	}
	want2 := `top = "kept"

[harness.reviewer]
args = [
  "model=opus",
  "--verbose",
]
enabled = true

[other]
x = 1
`
	if string(ed2.Bytes()) != want2 {
		t.Fatalf("a value continuation line that reads like the key must not be removed; the real model line goes:\n%s", ed2.Bytes())
	}
	var doc2 map[string]any
	if _, err := toml.Decode(string(ed2.Bytes()), &doc2); err != nil {
		t.Fatalf("edited file no longer parses: %v\n%s", err, ed2.Bytes())
	}
	r2 := doc2["harness"].(map[string]any)["reviewer"].(map[string]any)
	if got, ok := r2["args"].([]any); !ok || len(got) != 2 {
		t.Errorf("args = %v", r2["args"])
	}
	if _, exists := r2["model"]; exists {
		t.Error("model still present")
	}

	// A trailing comment goes with its key — on a one-line value and on a
	// multi-line value's closing line — and the next key is untouched.
	ed3 := New([]byte("[harness.r]\nmodel = \"x\"  # mine\nargs = [\n  \"a\", # first\n]  # list\nquiet = true\n"))
	for _, k := range []string{"model", "args"} {
		if err := ed3.RemoveHarnessKey("r", k); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(ed3.Bytes()); got != "[harness.r]\nquiet = true\n" {
		t.Fatalf("a removed key's trailing comment must go with it: %q", got)
	}

	// A missing key fails closed.
	if err := ed.RemoveHarnessKey("reviewer", "nope"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound, got %v", err)
	}

	// A bare-spelled table resolves too.
	bare := New([]byte("[reviewer]\nmodel = \"x\"\n"))
	if err := bare.RemoveHarnessKey("reviewer", "model"); err != nil {
		t.Fatal(err)
	}
	// The emptied table keeps its header, which is still valid TOML.
	if string(bare.Bytes()) != "[reviewer]\n" {
		t.Fatalf("bare table removal shape: %q", bare.Bytes())
	}

	// The edited file still parses.
	if _, err := toml.Decode(string(ed.Bytes()), &map[string]any{}); err != nil {
		t.Fatalf("edited file no longer parses: %v", err)
	}
}
