package main

// Every file that lands in the pin store is listed, marked scanned or not
// scanned (issue #956).
//
// Install copies the whole package tree into the content-addressed store,
// while the content scan reads only the manifest, every .md and .txt file and
// every file the manifest names as a prompt. So the confirmation and
// `agent info` must list every copied file, not the scanned subset, and say
// which of them nobody read. The property here is the store itself: after
// `install`, every file under the pin directory has to appear in the printed
// list, which is what a list built from the scan's own file set would fail to
// show.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-5.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// scriptPkg is the report's repro: a package whose payload is a script beside
// its manifest, plus text the scan does read.
const scriptPkg = `[package]
name = "qwatch"
description = "watches a directory"
license = "MIT"

[harness]
harness = "claude-code"
prompt_file = "prompts/watch.tmpl"
`

func scriptPkgFiles() map[string]string {
	return map[string]string{
		"packages/qwatch/package.toml":           scriptPkg,
		"packages/qwatch/watch.sh":               "#!/bin/sh\necho watching\n",
		"packages/qwatch/README.md":              "# qwatch\n\nWatches a directory.\n",
		"packages/qwatch/prompts/watch.md":       "Watch the directory and report.\n",
		"packages/qwatch/prompts/watch.tmpl":     "Watch {{dir}} and report.\n",
		"packages/qwatch/notes/release.txt":      "Nothing to say.\n",
		"packages/qwatch/skills/qwatch/SKILL.md": "Check the diff first.\n",
		"packages/qwatch/assets/table.csv":       "a,b\n1,2\n",
	}
}

// listedBundledFiles parses the "bundled files:" block of an info or
// confirmation report into the paths it names and their markers.
func listedBundledFiles(t *testing.T, out string) map[string]string {
	t.Helper()
	files := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		if line == "bundled files:" {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			inBlock = false
			continue
		}
		path, mark, found := strings.Cut(strings.TrimPrefix(line, "  "), "  ")
		if !found {
			t.Fatalf("bundled file %q carries no scan marker:\n%s", line, out)
		}
		mark = strings.Trim(mark, "()")
		if mark != agentpkg.ScannedMark && mark != agentpkg.UnscannedMark {
			t.Fatalf("bundled file %q has marker %q, want %q or %q", path, mark, agentpkg.ScannedMark, agentpkg.UnscannedMark)
		}
		files[path] = mark
	}
	if len(files) == 0 {
		t.Fatalf("no bundled files block in output:\n%s", out)
	}
	return files
}

func storeFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestBundledFilesListedWithScanMarkers drives the real command tree: info
// over the clone, install into the store, then the store itself.
func TestBundledFilesListedWithScanMarkers(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, scriptPkgFiles())
	if _, _, err := e.run("agent", "stable", "add", "spike2", remote); err != nil {
		t.Fatal(err)
	}

	info, _, err := e.run("agent", "info", "spike2/qwatch")
	if err != nil {
		t.Fatalf("info: %v\n%s", err, info)
	}
	listed := listedBundledFiles(t, info)
	for _, want := range []struct{ path, mark string }{
		{"package.toml", agentpkg.ScannedMark},
		{"README.md", agentpkg.ScannedMark},
		{"notes/release.txt", agentpkg.ScannedMark},
		{"prompts/watch.md", agentpkg.ScannedMark},
		{"prompts/watch.tmpl", agentpkg.ScannedMark}, // named prompt: scanned whatever its extension
		{"skills/qwatch/SKILL.md", agentpkg.ScannedMark},
		{"assets/table.csv", agentpkg.UnscannedMark},
		{"watch.sh", agentpkg.UnscannedMark},
	} {
		if got := listed[want.path]; got != want.mark {
			t.Errorf("info marked %s as %q, want %q (all: %v)", want.path, got, want.mark, listed)
		}
	}
	note := "note: not scanned: assets/table.csv, watch.sh."
	if !strings.Contains(info, note) {
		t.Errorf("info is missing the unscanned note %q:\n%s", note, info)
	}
	if !strings.Contains(info, "the scan covers the manifest, every .md and .txt file, and every file the manifest names as a prompt") {
		t.Errorf("info does not say what the scan covers:\n%s", info)
	}

	out, _, err := e.run("agent", "install", "spike2/qwatch", "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	confirmed := listedBundledFiles(t, out)
	if !strings.Contains(out, note) {
		t.Errorf("install confirmation is missing the unscanned note %q:\n%s", note, out)
	}

	// The property: every file the store holds was confirmed, with the same
	// marker info gave. A list drawn from the scanned subset would drop
	// watch.sh and assets/table.csv here.
	pinRoot := filepath.Join(agentpkg.InstalledRoot(), "spike2", "qwatch")
	entries, err := os.ReadDir(pinRoot)
	if err != nil {
		t.Fatal(err)
	}
	var pins []string
	for _, d := range entries {
		if d.IsDir() {
			pins = append(pins, filepath.Join(pinRoot, d.Name()))
		}
	}
	if len(pins) != 1 {
		t.Fatalf("want exactly one qwatch pin directory, got %v", pins)
	}
	stored := storeFiles(t, pins[0])
	for _, rel := range stored {
		mark, ok := confirmed[rel]
		if !ok {
			t.Errorf("%s was copied into the pin store but never confirmed; store holds %v", rel, stored)
			continue
		}
		if infoMark := listed[rel]; infoMark != mark {
			t.Errorf("%s is marked %q by install and %q by info", rel, mark, infoMark)
		}
	}
	if len(stored) != len(confirmed) {
		t.Errorf("the store holds %d files but the confirmation listed %d: store %v, listed %v", len(stored), len(confirmed), stored, confirmed)
	}
}
