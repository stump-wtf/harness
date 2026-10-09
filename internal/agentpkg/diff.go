// Pin diffing: what changed between the installed pin and the upgrade
// candidate, rendered for the upgrade confirmation (SPEC-0026 REQ-8):
// changed manifest values key by key, a unified diff per bundled text file
// under 64 KiB, byte counts for larger or non-text files, and added or
// removed files.
//
// There is no diff library in go.mod, and this tree is a security-sensitive
// code path: rather than vetting and carrying a third-party dependency, the
// line differ here is a small Myers O(ND) implementation with a bounded edit
// distance — beyond the bound the whole file renders as one replacement
// hunk, which stays honest and reviewable instead of blowing up.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-8 (upgrade).
//
// @joestump-agent 10/02/2026 - Added for harness#814.
package agentpkg

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// DiffTextLimit is the byte limit under which two bundled files are diffed
// as text (REQ-8): 64 KiB.
const DiffTextLimit = 64 * 1024

// maxEditDistance bounds the Myers search. Two files further apart than
// this render as a whole-file replacement hunk instead.
const maxEditDistance = 4096

// PinDiff renders the upgrade diff between the installed pin's directory
// and the candidate's: manifest value changes first, then every bundled
// file. It never reads anything outside the two directories.
func PinDiff(oldDir, newDir string, oldMan, newMan *Manifest) ([]string, error) {
	var lines []string
	lines = append(lines, ManifestChanges(oldMan, newMan)...)

	oldFiles, err := BundledFilesIn(oldDir)
	if err != nil {
		return nil, err
	}
	newFiles, err := BundledFilesIn(newDir)
	if err != nil {
		return nil, err
	}
	oldSet := map[string]bool{}
	for _, f := range oldFiles {
		oldSet[f] = true
	}
	newSet := map[string]bool{}
	for _, f := range newFiles {
		newSet[f] = true
	}
	for _, f := range oldFiles {
		switch {
		case !newSet[f]:
			lines = append(lines, "removed "+f)
		default:
			fileLines, err := fileDiff(f, filepath.Join(oldDir, f), filepath.Join(newDir, f))
			if err != nil {
				return nil, err
			}
			lines = append(lines, fileLines...)
		}
	}
	for _, f := range newFiles {
		if !oldSet[f] {
			lines = append(lines, "added "+f)
		}
	}
	return lines, nil
}

// fileDiff renders one bundled file's change: a unified diff for a text
// file under DiffTextLimit, a byte-count line otherwise.
func fileDiff(rel, oldPath, newPath string) ([]string, error) {
	oldRaw, err := os.ReadFile(oldPath)
	if err != nil {
		return nil, fmt.Errorf("agentpkg: diff %s: %w", rel, err)
	}
	newRaw, err := os.ReadFile(newPath)
	if err != nil {
		return nil, fmt.Errorf("agentpkg: diff %s: %w", rel, err)
	}
	if bytes.Equal(oldRaw, newRaw) {
		return nil, nil
	}
	if len(oldRaw) > DiffTextLimit || len(newRaw) > DiffTextLimit || !isText(oldRaw) || !isText(newRaw) {
		return []string{fmt.Sprintf("changed %s (%d -> %d bytes; not shown as text)", rel, len(oldRaw), len(newRaw))}, nil
	}
	diff := UnifiedDiff("a/"+rel, "b/"+rel, splitLines(oldRaw), splitLines(newRaw))
	return []string{strings.TrimRight(diff, "\n")}, nil
}

// isText reports whether data plausibly renders as text: valid UTF-8 with
// no NUL byte.
func isText(data []byte) bool {
	return utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}

func splitLines(data []byte) []string {
	s := string(data)
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ManifestChanges renders every changed manifest value key by key (REQ-8),
// as `<key>: <old> -> <new>` lines. Unset renders as `unset`.
func ManifestChanges(oldMan, newMan *Manifest) []string {
	var lines []string
	var om, nm Manifest
	if oldMan != nil {
		om = *oldMan
	}
	if newMan != nil {
		nm = *newMan
	}
	add := func(key string, oldVal, newVal any) {
		if fmt.Sprint(oldVal) != fmt.Sprint(newVal) {
			lines = append(lines, fmt.Sprintf("%s: %s -> %s", key, valOrUnset(oldVal), valOrUnset(newVal)))
		}
	}
	add("package.name", om.Package.Name, nm.Package.Name)
	add("package.version", om.Package.Version, nm.Package.Version)
	add("package.description", om.Package.Description, nm.Package.Description)
	add("package.author", om.Package.Author, nm.Package.Author)
	add("package.homepage", om.Package.Homepage, nm.Package.Homepage)
	// A license change is a terms change: its row says so rather than
	// reading like any other metadata edit (SPEC-0026 REQ-3, REQ-8).
	if om.Package.License != nm.Package.License {
		lines = append(lines, fmt.Sprintf("package.license: %s -> %s  (license change: the package's terms changed)",
			valOrUnset(strOrNil(om.Package.License)), valOrUnset(strOrNil(nm.Package.License))))
	}
	add("harness.harness", om.Harness.Harness, nm.Harness.Harness)
	add("harness.model", om.Harness.Model, nm.Harness.Model)
	add("harness.args", om.Harness.Args, nm.Harness.Args)
	add("harness.argv", om.Harness.Argv, nm.Harness.Argv)
	add("harness.auto_accept", boolPtr(om.Harness.AutoAccept), boolPtr(nm.Harness.AutoAccept))
	add("harness.max_turns", intPtr(om.Harness.MaxTurns), intPtr(nm.Harness.MaxTurns))
	add("harness.quiet", boolPtr(om.Harness.Quiet), boolPtr(nm.Harness.Quiet))
	add("harness.prompt", om.Harness.Prompt, nm.Harness.Prompt)
	add("harness.prompt_file", om.Harness.PromptFile, nm.Harness.PromptFile)
	add("harness.prompt_template", om.Harness.PromptTemplate, nm.Harness.PromptTemplate)
	add("harness.prompt_template_file", om.Harness.PromptTemplateFile, nm.Harness.PromptTemplateFile)
	add("harness.system_prompt_file", om.Harness.SystemPromptFile, nm.Harness.SystemPromptFile)
	add("harness.mcp_config", om.Harness.MCPConfig, nm.Harness.MCPConfig)
	add("harness.allowed_tools", om.Harness.AllowedTools, nm.Harness.AllowedTools)
	add("requests.skill_paths", boolPtr(om.Requests.SkillPaths), boolPtr(nm.Requests.SkillPaths))
	add("requests.mcp_allow", om.Requests.MCPAllow, nm.Requests.MCPAllow)
	add("requests.network", boolPtr(om.Requests.Network), boolPtr(nm.Requests.Network))
	return lines
}

func boolPtr(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func intPtr(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func valOrUnset(v any) string {
	if v == nil {
		return "unset"
	}
	if s, ok := v.(string); ok && s == "" {
		return "\"\""

	}
	return fmt.Sprintf("%v", v)
}

// UnifiedDiff renders a standard unified diff (3 context lines) turning
// oldLines into newLines. Files beyond maxEditDistance apart render as one
// whole-file replacement hunk.
func UnifiedDiff(oldLabel, newLabel string, oldLines, newLines []string) string {
	edits, ok := editScript(oldLines, newLines)
	if !ok {
		edits = wholeFileEdits(oldLines, newLines)
	}
	edits = compactEdits(edits)
	hunks := renderHunks(edits, 3)
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldLabel, newLabel)
	for _, h := range hunks {
		b.WriteString(h)
	}
	return b.String()
}

// edit is one line of an edit script: '=' keep, '-' delete, '+' insert.
type edit struct {
	kind byte
	line string
}

// editScript runs the greedy Myers algorithm and backtracks the edit path.
// ok is false when the edit distance exceeds maxEditDistance.
func editScript(a, b []string) ([]edit, bool) {
	n, m := len(a), len(b)
	if n+m > maxEditDistance {
		return nil, false
	}
	v := map[int]int{1: 0}
	var trace []map[int]int
	for d := 0; d <= n+m; d++ {
		trace = append(trace, snapshotV(v))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1] < v[k+1]) {
				x = v[k+1]
			} else {
				x = v[k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, d), true
			}
		}
	}
	return nil, false
}

func snapshotV(v map[int]int) map[int]int {
	out := make(map[int]int, len(v))
	for k, x := range v {
		out[k] = x
	}
	return out
}

// backtrack walks the trace backwards from (n, m), emitting the edit
// script in reverse, then reverses it.
func backtrack(trace []map[int]int, a, b []string, d int) []edit {
	var rev []edit
	x, y := len(a), len(b)
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[k-1] < v[k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, edit{'=', a[x-1]})
			x--
			y--
		}
		if x == prevX {
			rev = append(rev, edit{'+', b[y-1]})
			y--
		} else {
			rev = append(rev, edit{'-', a[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		rev = append(rev, edit{'=', a[x-1]})
		x--
		y--
	}
	out := make([]edit, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// compactEdits slides each pure change run as far up (earliest) as it can
// go, the same normalization xdiff applies so repeated lines do not anchor
// a change at the file's end. A pure deletion run may slide over any
// context line (the kept text is identical either way); a pure insertion
// run only over a context line equal to its own last line. Mixed
// delete+insert runs are left in place: sliding them is not generally valid.
func compactEdits(edits []edit) []edit {
	out := append([]edit(nil), edits...)
	for changed := true; changed; {
		changed = false
		i := 0
		for i < len(out) {
			if out[i].kind == '=' {
				i++
				continue
			}
			j := i
			for j < len(out) && out[j].kind != '=' {
				j++
			}
			pure := true
			for k := i; k < j; k++ {
				if out[k].kind != out[i].kind {
					pure = false
					break
				}
			}
			if pure && i > 0 && out[i-1].kind == '=' &&
				(out[i].kind == '-' || out[i-1].line == out[j-1].line) {
				c := out[i-1]
				copy(out[i-1:j-1], out[i:j])
				out[j-1] = c
				changed = true
				i--
				continue
			}
			i = j
		}
	}
	return out
}

// wholeFileEdits replaces the entire file in one hunk.
func wholeFileEdits(oldLines, newLines []string) []edit {
	var out []edit
	for _, l := range oldLines {
		out = append(out, edit{'-', l})
	}
	for _, l := range newLines {
		out = append(out, edit{'+', l})
	}
	return out
}

// renderHunks groups an edit script into hunks with the given context
// width, in standard `@@ -l,s +l,s @@` form, tracking old- and new-file
// line positions as it walks.
func renderHunks(edits []edit, context int) []string {
	// Mark which edits sit in a hunk of change (not pure context).
	inHunk := make([]bool, len(edits))
	for i, e := range edits {
		if e.kind != '=' {
			for j := i - context; j <= i+context; j++ {
				if j >= 0 && j < len(edits) {
					inHunk[j] = true
				}
			}
		}
	}
	var hunks []string
	old, new := 0, 0
	i := 0
	for i < len(edits) {
		if !inHunk[i] {
			if edits[i].kind != '+' {
				old++
			}
			if edits[i].kind != '-' {
				new++
			}
			i++
			continue
		}
		start := i
		for i < len(edits) && inHunk[i] {
			if edits[i].kind != '+' {
				old++
			}
			if edits[i].kind != '-' {
				new++
			}
			i++
		}
		hunks = append(hunks, formatHunk(edits[start:i], old, new))
	}
	return hunks
}

// formatHunk renders one hunk, given the old- and new-file line counts at
// its END (the hunk's start lines follow from its own counts).
func formatHunk(hunk []edit, oldEnd, newEnd int) string {
	oldCount, newCount := 0, 0
	for _, e := range hunk {
		if e.kind != '+' {
			oldCount++
		}
		if e.kind != '-' {
			newCount++
		}
	}
	oldStart := oldEnd - oldCount + 1
	newStart := newEnd - newCount + 1
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(oldStart, oldCount), hunkRange(newStart, newCount))
	for _, e := range hunk {
		if e.kind == '=' {
			b.WriteString(" ")
		} else {
			b.WriteByte(e.kind)
		}
		b.WriteString(e.line)
		b.WriteString("\n")
	}
	return b.String()
}

func hunkRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
