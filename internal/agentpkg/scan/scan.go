// The scan runner: walks one package directory (a stable clone's package
// checkout or an installed pin), linting the manifest text, every bundled
// .md/.txt file and every prompt file the manifest names, line by line. Findings carry file, line, pattern id and
// severity, never matched text.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-5, Error
// Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#812.
package scan

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// ManifestFile is the package-relative path of the manifest every package
// has exactly one of.
const ManifestFile = "package.toml"

// Scan lints one package directory's manifest text, every bundled .md or
// .txt file, and every prompt file the manifest names (SPEC-0026 REQ-5). Manifest string values are covered by
// scanning package.toml's text; the line numbers are the file's real ones.
// man supplies the declared homepage for the foreign-domain link check.
// Files are visited in sorted order so findings are deterministic. A
// package with no README.md draws the low package.no-readme finding.
func Scan(pkgDir string, man *agentpkg.Manifest) ([]agentpkg.Finding, error) {
	var findings []agentpkg.Finding

	manPath := filepath.Join(pkgDir, ManifestFile)
	if raw, err := os.ReadFile(manPath); err == nil {
		findings = append(findings, scanLines(ManifestFile, string(raw), homepageHost(man))...)
		findings = append(findings, licenseFindings(string(raw), man)...)
	} else if !isNotExist(err) {
		return nil, fmt.Errorf("scan: read %s: %w", ManifestFile, err)
	}

	if _, err := os.ReadDir(pkgDir); err != nil {
		if isNotExist(err) {
			return nil, fmt.Errorf("scan: package directory %s does not exist", pkgDir)
		}
		return nil, fmt.Errorf("scan: read %s: %w", pkgDir, err)
	}
	// A file the manifest names as an instruction is natural language the
	// agent reads, whatever its extension: a prompt_template_file is often
	// .tmpl, and a system prompt need not end in .md.
	named := map[string]bool{}
	if man != nil {
		for _, p := range []string{man.Harness.PromptFile, man.Harness.PromptTemplateFile, man.Harness.SystemPromptFile} {
			if p != "" && !filepath.IsAbs(p) {
				named[filepath.Join(pkgDir, p)] = true
			}
		}
	}
	var files []string
	err := filepath.WalkDir(pkgDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == manPath {
			return nil
		}
		if ext := filepath.Ext(d.Name()); ext == ".md" || ext == ".txt" || named[path] {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan: walk %s: %w", pkgDir, err)
	}
	sort.Strings(files)
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("scan: read %s: %w", relPath(pkgDir, path), err)
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, pkgDir+string(filepath.Separator)))
		findings = append(findings, scanLines(rel, string(raw), homepageHost(man))...)
	}
	readme, err := readmeFindings(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	findings = append(findings, readme...)
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// scanLines lints one file's text, recording findings at 1-based lines. The
// matched text never leaves this function.
func scanLines(file, text, homepage string) []agentpkg.Finding {
	var findings []agentpkg.Finding
	for i, line := range strings.Split(text, "\n") {
		for _, p := range patterns {
			if p.re.MatchString(line) {
				findings = append(findings, agentpkg.Finding{
					File:      file,
					Line:      i + 1,
					PatternID: p.ID,
					Severity:  p.Severity,
					LineHash:  agentpkg.HashLine(line),
				})
			}
		}
		findings = append(findings, foreignDomainFindings(file, i+1, line, homepage)...)
	}
	return findings
}

// foreignDomainFindings flags links whose domain is not the package's
// declared homepage domain (or its subdomain). A package that declares no
// homepage has no trusted domain, so every link is foreign. LOW severity:
// shown, never blocking.
func foreignDomainFindings(file string, line int, text, homepage string) []agentpkg.Finding {
	var findings []agentpkg.Finding
	for _, m := range linkPattern.FindAllStringSubmatch(text, -1) {
		link := m[1]
		if link == "" {
			link = m[2]
		}
		host := linkHost(link)
		if host == "" {
			continue
		}
		if host == homepage || strings.HasSuffix(host, "."+homepage) {
			continue
		}
		findings = append(findings, agentpkg.Finding{
			File:      file,
			Line:      line,
			PatternID: "link.foreign-domain",
			Severity:  agentpkg.SeverityLow,
			LineHash:  agentpkg.HashLine(text),
		})
	}
	return findings
}

// relPath renders a package-relative path for errors.
func relPath(pkgDir, path string) string {
	return filepath.ToSlash(strings.TrimPrefix(path, pkgDir+string(filepath.Separator)))
}

// homepageHost extracts the host of the manifest's declared homepage, ""
// when absent or unparseable.
func homepageHost(man *agentpkg.Manifest) string {
	if man == nil || man.Package.Homepage == "" {
		return ""
	}
	return linkHost(man.Package.Homepage)
}

// linkHost extracts an http(s) URL's host.
func linkHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

func isNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}
