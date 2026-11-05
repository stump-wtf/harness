// Summary rendering for get_skill: the four sections a caller reads before
// deciding whether to pull the whole file, capped at summary_max_chars
// (SPEC-0007 REQ "Search And Retrieval Tools").
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos";
// ADR-0030 (summaries are what keeps retrieval context-cheap).
package skillserve

import (
	"strings"
)

// SummarySections are the body sections the summary render keeps, in the order
// it renders them (SPEC-0007 REQ "Search And Retrieval Tools"). A skill repo's
// artifact format also carries *Failure modes* and *Evidence*; those are for a
// reviewer of a proposal, not for a caller deciding what to read.
var SummarySections = []string{"When to use", "Steps", "Invariants", "Do not"}

// truncationMarker marks a summary cut short by summary_max_chars, so a caller
// can tell a cap from a short skill.
const truncationMarker = "\n\n…(truncated)"

// skillBody returns a skill file's markdown body, dropping its frontmatter.
// Content without a frontmatter block is returned as is: the index already
// refused the malformed files, so this only guards a direct read.
func skillBody(content string) string {
	rest := strings.TrimPrefix(content, "---\n")
	if rest == content {
		return content
	}
	for _, delim := range []string{"\n---\n", "\n---\r\n"} {
		if idx := strings.Index(rest, delim); idx >= 0 {
			return rest[idx+len(delim):]
		}
	}
	return content
}

// Summary renders the summary view of a skill file's body: each
// SummarySections heading that exists, in canonical order, cut to maxChars.
// A body carrying none of them is returned capped as is, because an empty
// summary would tell the caller nothing.
func Summary(body string, maxChars int) string {
	blocks := summaryBlocks(body)
	var kept []string
	for _, want := range SummarySections {
		text, ok := blocks[want]
		if !ok {
			continue
		}
		kept = append(kept, "## "+want+"\n\n"+text+"\n")
	}
	out := strings.TrimSpace(strings.Join(kept, "\n"))
	if out == "" {
		out = strings.TrimSpace(body)
	}
	if out != "" {
		out += "\n"
	}
	return capChars(out, maxChars)
}

// summaryBlocks splits a body on its level-2 headings and keys each block by
// its trimmed title.
func summaryBlocks(body string) map[string]string {
	blocks := map[string]string{}
	title := ""
	var cur []string
	for _, line := range strings.Split(body, "\n") {
		if t, ok := level2Heading(line); ok {
			if title != "" {
				blocks[title] = strings.TrimSpace(strings.Join(cur, "\n"))
			}
			title, cur = t, nil
			continue
		}
		if title != "" {
			cur = append(cur, line)
		}
	}
	if title != "" {
		blocks[title] = strings.TrimSpace(strings.Join(cur, "\n"))
	}
	return blocks
}

// level2Heading reports the title of a "## Title" line.
func level2Heading(line string) (string, bool) {
	trimmed := strings.TrimRight(line, " \t\r")
	if !strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")), true
}

// capChars cuts s to at most maxChars, appending a marker when it cut. A
// maxChars at or below the marker's length still returns at most maxChars
// characters.
func capChars(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	if maxChars <= len(truncationMarker) {
		return s[:maxChars]
	}
	cut := maxChars - len(truncationMarker)
	for cut > 0 && !utf8Start(s, cut) {
		cut--
	}
	return s[:cut] + truncationMarker
}

func utf8Start(s string, i int) bool { return s[i]&0xC0 != 0x80 }
