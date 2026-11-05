// Command modcheck fails when the checked-out go.mod lowers the version of
// a requirement the base ref still compiles.
//
// The shape it exists for (harness#1047, msgbrowse#568): a Renovate major
// update of a /vN Go module (go.yaml.in/yaml/v3 -> v4) rewrites go.mod
// against the old imports, silently lowering the version of the module the
// code actually compiles while adding a requirement for the new major that
// no source file imports. CI passes wherever the repo has no tidy gate.
//
// A violation is a module that is a direct require in BOTH the base and the
// head go.mod with a lower version in head. Direct means the module's
// packages import it, so the lower version is what gets compiled. A module
// that drops from direct to // indirect is not a violation: the code no
// longer compiles it, and the remaining indirect line is the MVS floor set
// by a transitive dependency (the shape cairn#492 landed when its imports
// moved to v4). The same reasoning applies to a module that is indirect in
// base: its floor is a transitive dependency's, and MVS takes the maximum,
// so the compiled version cannot go down.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// Violation is a module whose direct requirement was lowered.
type Violation struct {
	Path    string
	BaseVer string
	HeadVer string
}

// Downgrades reports the modules that are a direct require in both go.mod
// files and whose version went down from base to head. baseContent and
// headContent are the raw go.mod files.
func Downgrades(baseContent, headContent string) ([]Violation, error) {
	base, err := modfile.Parse("base go.mod", []byte(baseContent), nil)
	if err != nil {
		return nil, fmt.Errorf("parse base go.mod: %w", err)
	}
	head, err := modfile.Parse("head go.mod", []byte(headContent), nil)
	if err != nil {
		return nil, fmt.Errorf("parse head go.mod: %w", err)
	}
	baseByPath := make(map[string]*modfile.Require, len(base.Require))
	for _, r := range base.Require {
		baseByPath[r.Mod.Path] = r
	}
	var out []Violation
	for _, h := range head.Require {
		if h.Indirect {
			continue
		}
		b, ok := baseByPath[h.Mod.Path]
		if !ok || b.Indirect {
			continue
		}
		if semver.Compare(ensureV(h.Mod.Version), ensureV(b.Mod.Version)) < 0 {
			out = append(out, Violation{Path: h.Mod.Path, BaseVer: b.Mod.Version, HeadVer: h.Mod.Version})
		}
	}
	return out, nil
}

// ensureV returns the semver form of a go.mod version, which is written
// without the leading v.
func ensureV(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: modcheck <base-ref|''> <head-go.mod>")
		os.Exit(2)
	}
	baseRef, headPath := os.Args[1], os.Args[2]
	if baseRef == "" {
		fmt.Fprintln(os.Stderr, "modcheck: base ref empty, skipping")
		return
	}
	base, err := exec.Command("git", "show", baseRef+":go.mod").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "modcheck: cannot read %s:go.mod: %v\n", baseRef, err)
		fmt.Fprintln(os.Stderr, "modcheck: fetch the base ref first (git fetch origin), or skip with an empty base ref")
		os.Exit(2)
	}
	head, err := os.ReadFile(headPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "modcheck: %v\n", err)
		os.Exit(2)
	}
	violations, err := Downgrades(string(base), string(head))
	if err != nil {
		fmt.Fprintf(os.Stderr, "modcheck: %v\n", err)
		os.Exit(2)
	}
	if len(violations) == 0 {
		return
	}
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "go.mod downgrade: %s %s -> %s\n", v.Path, v.BaseVer, v.HeadVer)
	}
	fmt.Fprintln(os.Stderr, "modcheck: a requirement the base branch compiles was lowered")
	fmt.Fprintln(os.Stderr, "modcheck: restore the version, or make the downgrade deliberate and re-run with an empty base ref")
	os.Exit(1)
}
