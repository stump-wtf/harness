// Package stablelint validates a stable checkout — a directory with a
// packages/ tree — offline, with Harness's own manifest parser and content
// scanner: the engine behind `harness agent stable lint` and `stable check`.
// A stable's CI runs it so a package Harness would refuse at install fails
// there first, and the two can never disagree: lint calls the same
// agentpkg.LoadManifest and scan.Scan install does, and check loads each
// package through the same config.ParseWith path a sourced harness table
// takes. Nothing here dials the daemon, runs git, or touches the network.
//
// Governing: ADR-0044 (agent package stables: install and stable lint share
// one parser and scanner), SPEC-0026 REQ-3 (manifest schema), REQ-5 (content
// scan and severity), REQ-7 (config-load resolution), REQ-12 (CLI
// visibility), Error Handling Standards.
//
// @joestump-agent 10/09/2026 - Added for harness#933.
package stablelint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/agentpkg/scan"
	"github.com/stump-wtf/harness/internal/config"
)

// Finding IDs lint produces itself. Scan findings keep their pattern id
// prefixed with "scan.", so a CI annotation names the same pattern an
// install's blocked error does.
const (
	IDManifest     = "manifest.schema"
	IDBadName      = "package.bad-name"
	IDNameMismatch = "package.name-mismatch"
	IDMissingFile  = "package.missing-file"
	IDEmptyFile    = "package.empty-file"
	IDSymlink      = "package.symlink"
	IDNoReadme     = "package.no-readme"
	IDNoLicense    = "package.no-license"
	IDLoad         = "check.load"
)

// promoted names the low-severity findings lint reports as errors. Install
// shows them and proceeds; a stable that publishes a package without them
// fails its own CI, because the operator reading the install confirmation is
// the one who pays for the gap. The README finding is produced here; the
// license finding arrives with [package].license (#932) and is promoted the
// moment a producer emits it.
var promoted = map[string]bool{
	IDNoReadme:  true,
	IDNoLicense: true,
}

// Issue is one lint or check result. It never carries scanned text: a scan
// finding names only the file, the line and the pattern id (SPEC-0026 Error
// Handling Standards).
type Issue struct {
	ID      string `json:"id"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// Report is one package's result. Errors fail the run; warnings are shown.
// Both are always non-nil so --json emits [] rather than null.
type Report struct {
	Package  string  `json:"package"`
	Errors   []Issue `json:"errors"`
	Warnings []Issue `json:"warnings"`

	// dir is the package directory, man its manifest when it parsed, and
	// pathErr whether a path key failed — check skips the load test then,
	// since config load would only restate the same missing file.
	dir     string
	man     *agentpkg.Manifest
	pathErr bool
}

// Failed reports whether any package carries an error.
func Failed(reports []Report) bool {
	for _, r := range reports {
		if len(r.Errors) > 0 {
			return true
		}
	}
	return false
}

// add files a finding as an error or a warning: high severity and every
// promoted id are errors, the rest warnings.
func (r *Report) add(sev agentpkg.Severity, is Issue) {
	if sev == agentpkg.SeverityHigh || promoted[is.ID] {
		r.Errors = append(r.Errors, is)
		return
	}
	r.Warnings = append(r.Warnings, is)
}

// ErrNotStable is returned when path holds no packages/ directory, or one
// with no packages in it: a CI job pointed at the wrong directory must fail
// rather than report a clean run over nothing.
var ErrNotStable = errors.New("not a stable checkout")

// Lint validates every package under root/packages. The returned error is
// for a root that cannot be read as a stable at all; per-package problems
// are in the reports.
func Lint(root string) ([]Report, error) {
	pkgsDir := filepath.Join(root, "packages")
	entries, err := os.ReadDir(pkgsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s has no packages/ directory", ErrNotStable, root)
		}
		return nil, fmt.Errorf("stablelint: read %s: %w", pkgsDir, err)
	}
	var reports []Report
	for _, e := range entries {
		// Discovery (agentpkg.ListPackages) skips plain files in packages/;
		// lint sees the same packages an operator's search does.
		if !e.IsDir() {
			continue
		}
		reports = append(reports, lintPackage(filepath.Join(pkgsDir, e.Name()), e.Name()))
	}
	if len(reports) == 0 {
		return nil, fmt.Errorf("%w: %s holds no packages", ErrNotStable, pkgsDir)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Package < reports[j].Package })
	return reports, nil
}

func lintPackage(dir, name string) Report {
	r := Report{Package: name, Errors: []Issue{}, Warnings: []Issue{}, dir: dir}
	if !agentpkg.NamePattern.MatchString(name) {
		r.add(agentpkg.SeverityHigh, Issue{ID: IDBadName,
			Message: fmt.Sprintf("package directory %q must match %s", name, agentpkg.NamePattern)})
	}

	// The manifest, through the exact loader install uses. It stops at the
	// first violation, so a manifest with two is fixed one at a time — the
	// same order install would refuse it in.
	manPath := filepath.Join(dir, scan.ManifestFile)
	man, err := agentpkg.LoadManifest(manPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add(agentpkg.SeverityHigh, Issue{ID: IDManifest, File: scan.ManifestFile,
			Message: "no package.toml: every package directory needs one"})
	case err != nil:
		// The issue already names package.toml; drop the loader's path
		// prefix so the message reads the same wherever the checkout is.
		msg := err.Error()
		if i := strings.Index(msg, manPath+": "); i >= 0 {
			msg = msg[i+len(manPath)+2:]
		}
		r.add(agentpkg.SeverityHigh, Issue{ID: IDManifest, File: scan.ManifestFile, Message: msg})
	default:
		r.man = man
		if man.Package.Name != name {
			r.add(agentpkg.SeverityHigh, Issue{ID: IDNameMismatch, File: scan.ManifestFile,
				Message: fmt.Sprintf("[package].name %q does not match its directory %q", man.Package.Name, name)})
		}
		r.lintPaths()
	}

	// Install materializes with git archive and refuses a symlink outright:
	// it is a path out of the pin.
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			r.add(agentpkg.SeverityHigh, Issue{ID: IDSymlink, File: rel(dir, path),
				Message: "packages must not carry symlinks; install refuses them"})
		}
		return nil
	})

	if fi, err := os.Stat(filepath.Join(dir, "README.md")); err != nil || !fi.Mode().IsRegular() {
		r.add(agentpkg.SeverityLow, Issue{ID: IDNoReadme, File: "README.md",
			Message: "no README.md: the operator installing this package has nothing to read about it"})
	}

	// The scan, exactly as install runs it. A manifest that failed to parse
	// is still scanned (with no manifest), so one run reports both.
	findings, err := scan.Scan(dir, r.man)
	if err != nil {
		r.add(agentpkg.SeverityHigh, Issue{ID: "scan.error", Message: err.Error()})
	}
	for _, f := range findings {
		r.add(f.Severity, Issue{ID: "scan." + f.PatternID, File: f.File, Line: f.Line,
			Message: fmt.Sprintf("%s-severity scan finding %s", f.Severity, f.PatternID)})
	}
	return r
}

// lintPaths checks that every path key names something the package ships:
// a file that exists and is not empty, or for skill_paths a directory.
// ParseManifest has already proven each one stays inside the package.
func (r *Report) lintPaths() {
	hv := r.man.Harness
	for _, pv := range []struct{ key, val string }{
		{"prompt_file", hv.PromptFile},
		{"prompt_template_file", hv.PromptTemplateFile},
		{"system_prompt_file", hv.SystemPromptFile},
		{"mcp_config", hv.MCPConfig},
	} {
		if pv.val == "" {
			continue
		}
		fi, err := os.Stat(filepath.Join(r.dir, pv.val))
		switch {
		case err != nil:
			r.pathErr = true
			r.add(agentpkg.SeverityHigh, Issue{ID: IDMissingFile, File: scan.ManifestFile,
				Message: fmt.Sprintf("[harness].%s %q does not exist in the package", pv.key, pv.val)})
		case !fi.Mode().IsRegular():
			r.pathErr = true
			r.add(agentpkg.SeverityHigh, Issue{ID: IDMissingFile, File: scan.ManifestFile,
				Message: fmt.Sprintf("[harness].%s %q is not a regular file", pv.key, pv.val)})
		case fi.Size() == 0:
			r.pathErr = true
			r.add(agentpkg.SeverityHigh, Issue{ID: IDEmptyFile, File: scan.ManifestFile,
				Message: fmt.Sprintf("[harness].%s %q is empty", pv.key, pv.val)})
		}
	}
	for _, sp := range hv.SkillPaths {
		if fi, err := os.Stat(filepath.Join(r.dir, sp)); err != nil || !fi.IsDir() {
			r.pathErr = true
			r.add(agentpkg.SeverityHigh, Issue{ID: IDMissingFile, File: scan.ManifestFile,
				Message: fmt.Sprintf("[harness].skill_paths %q is not a directory in the package", sp)})
		}
	}
}

// checkStable and checkSHA form the synthetic source each package is
// load-tested under. They are never installed or fetched: ParseOptions.PinDir
// maps the source straight to the checkout's package directory.
const (
	checkStable = "checkout"
	checkSHA    = "0000000000000000000000000000000000000000"
	// checkSchedule makes a one-shot package's synthetic table a scheduled
	// one, so a prompt template is validated against the context a
	// scheduled firing supplies.
	checkSchedule = "@daily"
)

// Check runs Lint, then loads every package whose manifest parsed through
// the real config path: a synthetic [harness.<name>] table carrying only
// `source` — plus `schedule` when the package supplies a prompt, as an
// operator wiring a one-shot would — is parsed with config.ParseWith, so an
// unknown adapter, a template referencing context its firing cannot supply,
// or any other load error is reported exactly as `harness reload` would.
func Check(root string) ([]Report, error) {
	reports, err := Lint(root)
	if err != nil {
		return nil, err
	}
	for i := range reports {
		r := &reports[i]
		if r.man == nil || r.pathErr {
			continue
		}
		if err := loadTest(r); err != nil {
			r.add(agentpkg.SeverityHigh, Issue{ID: IDLoad, Message: err.Error()})
		}
	}
	return reports, nil
}

func loadTest(r *Report) error {
	dir, err := filepath.Abs(r.dir)
	if err != nil {
		return err
	}
	name := r.man.Package.Name
	hv := r.man.Harness
	var b strings.Builder
	fmt.Fprintf(&b, "[harness.%s]\nsource = %q\n", name, checkStable+"/"+name+"@"+checkSHA)
	if hv.Prompt != "" || hv.PromptFile != "" || hv.PromptTemplate != "" || hv.PromptTemplateFile != "" {
		fmt.Fprintf(&b, "schedule = %q\n", checkSchedule)
	}
	_, err = config.ParseWith([]byte(b.String()), filepath.Join(dir, "check.toml"), config.ParseOptions{
		PinDir: func(agentpkg.Source) string { return dir },
	})
	if err == nil {
		return nil
	}
	// The synthetic table's file:line prefix points at nothing the author
	// can open; the message alone names the key and the package path.
	var ce *config.Error
	if errors.As(err, &ce) {
		return errors.New(ce.Msg)
	}
	return err
}

func rel(dir, path string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return filepath.ToSlash(r)
	}
	return path
}
