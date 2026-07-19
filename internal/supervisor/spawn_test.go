package supervisor

// Governing tests: ADR-0005 (spawn under a PTY in workdir with env_file
// loaded, {workdir} arg expansion); ADR-0008 (env_file is where secrets stay).

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	content := "" +
		"# a comment\n" +
		"\n" +
		"FOO=bar\n" +
		"export BAZ=qux\n" +
		"QUOTED=\"has spaces\"\n" +
		"SINGLE='single quoted'\n" +
		"NOEQUALS\n" +
		"EMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"FOO=bar":              "",
		"BAZ=qux":              "",
		"QUOTED=has spaces":    "",
		"SINGLE=single quoted": "",
		"EMPTY=":               "",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries %v, want %d", len(got), got, len(want))
	}
	for _, kv := range got {
		if _, ok := want[kv]; !ok {
			t.Errorf("unexpected entry %q", kv)
		}
	}
}

func TestParseEnvFileMissingIsNotError(t *testing.T) {
	got, err := parseEnvFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing env_file should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if g, err := parseEnvFile(""); err != nil || g != nil {
		t.Fatalf("empty path: got %v err %v", g, err)
	}
}

func TestExpandArgsWorkdir(t *testing.T) {
	got := expandArgs([]string{"--data-dir", "{workdir}", "--flag"}, "/home/x")
	want := []string{"--data-dir", "/home/x", "--flag"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
	if expandArgs(nil, "/x") != nil {
		t.Error("nil args should expand to nil")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/foo"); got != filepath.Join(home, "foo") {
		t.Errorf("expandHome(~/foo) = %q", got)
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q", got)
	}
	if got := expandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path changed: %q", got)
	}
}
