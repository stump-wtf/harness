package config

// Docs Examples Parse
//
// The guides under docs/guides/ are the first config a new user copies, so a
// TOML block there that fails to load is a broken onboarding path, not a typo.
// This test loads every fenced ```toml block in the guides that declares a table
// through the real Load path, plus harness.toml.example as a whole file.
//
// A block with no table header (a one-line `schedule = ...` illustration) is a
// fragment, not a config, and is skipped. Files a block points at — a
// prompt_file, a harness_d directory — are created under a throwaway $HOME
// first, because Load checks both exist and the guides use ~ paths.
//
// @joestump-agent 09/11/2026 - Added alongside the 0-to-1 user guides.

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	docsTableHeaderRe = regexp.MustCompile(`(?m)^\s*\[\[?[A-Za-z]`)
	docsPromptFileRe  = regexp.MustCompile(`(?m)^\s*prompt_file\s*=\s*"([^"]+)"`)
	docsHarnessDRe    = regexp.MustCompile(`(?m)^\s*harness_d\s*=\s*"([^"]+)"`)
)

// docsTOMLBlock is one fenced toml block and the line its fence opens on.
type docsTOMLBlock struct {
	line int
	body string
}

// docsTOMLBlocks returns every ```toml fenced block in a Markdown file.
func docsTOMLBlocks(t *testing.T, path string) []docsTOMLBlock {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var (
		blocks []docsTOMLBlock
		cur    *docsTOMLBlock
		buf    strings.Builder
	)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case cur == nil && strings.HasPrefix(trimmed, "```toml"):
			cur = &docsTOMLBlock{line: n}
			buf.Reset()
		case cur != nil && trimmed == "```":
			cur.body = buf.String()
			blocks = append(blocks, *cur)
			cur = nil
		case cur != nil:
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return blocks
}

// loadDocsConfig writes body as harness.toml in a fresh home, creates what it
// references, and loads it.
func loadDocsConfig(t *testing.T, body string) error {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "harness")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(cfgDir, p)
	}
	for _, m := range docsPromptFileRe.FindAllStringSubmatch(body, -1) {
		p := resolve(m[1])
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("Do the sweep and report.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range docsHarnessDRe.FindAllStringSubmatch(body, -1) {
		if err := os.MkdirAll(resolve(m[1]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(cfgDir, "harness.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	return err
}

func TestDocsGuideTOMLExamplesLoad(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "guides", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no guides found under docs/guides — did they move? update this test")
	}
	checked := 0
	for _, f := range files {
		for _, b := range docsTOMLBlocks(t, f) {
			if !docsTableHeaderRe.MatchString(b.body) {
				continue
			}
			checked++
			name := filepath.Base(f) + ":" + itoa(b.line)
			t.Run(name, func(t *testing.T) {
				if err := loadDocsConfig(t, b.body); err != nil {
					t.Errorf("%s: example does not load: %v\n---\n%s", name, err, b.body)
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("no loadable toml examples found in docs/guides")
	}
}

func TestHarnessTOMLExampleLoads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "harness.toml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if err := loadDocsConfig(t, string(data)); err != nil {
		t.Errorf("harness.toml.example does not load: %v", err)
	}
}

// itoa avoids pulling strconv in for one subtest label.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
