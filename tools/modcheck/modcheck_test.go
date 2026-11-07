package main

import (
	"reflect"
	"strings"
	"testing"
)

const baseGoMod = `module github.com/stump-wtf/harness

go 1.26

require (
	github.com/foo/bar v1.2.3
	go.yaml.in/yaml/v3 v3.0.6
	golang.org/x/exp v0.0.0-20260112195520-a5071408f32f
	golang.org/x/term v0.46.0
)

require (
	golang.org/x/mod v0.41.0 // indirect
)
`

// The exact shape of harness#1046 and msgbrowse#568: the v3 requirement the
// code still imports is lowered and an unused v4 requirement is added.
const pr1046GoMod = `module github.com/stump-wtf/harness

go 1.26

require (
	github.com/foo/bar v1.2.3
	go.yaml.in/yaml/v3 v3.0.5
	go.yaml.in/yaml/v4 v4.0.0
	golang.org/x/exp v0.0.0-20260112195520-a5071408f32f
	golang.org/x/term v0.46.0
)

require (
	golang.org/x/mod v0.41.0 // indirect
)
`

// The intended shape (cairn#492): the imports moved to v4 in the same
// commit, so v3 is only the MVS floor of a transitive dependency and is
// marked // indirect.
const migratedGoMod = `module github.com/stump-wtf/harness

go 1.26

require (
	github.com/foo/bar v1.2.3
	go.yaml.in/yaml/v4 v4.0.0
	golang.org/x/exp v0.0.0-20260112195520-a5071408f32f
	golang.org/x/term v0.46.0
)

require (
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/mod v0.41.0 // indirect
)
`

func mustDowngrades(t *testing.T, base, head string) []Violation {
	t.Helper()
	got, err := Downgrades(base, head)
	if err != nil {
		t.Fatalf("Downgrades: %v", err)
	}
	return got
}

func paths(v []Violation) []string {
	if len(v) == 0 {
		return nil
	}
	out := make([]string, len(v))
	for i, x := range v {
		out[i] = x.Path
	}
	return out
}

func TestDowngrades(t *testing.T) {
	tests := []struct {
		name string
		head string
		want []string
	}{
		{"unchanged", baseGoMod, nil},
		{"pr1046 downgrade shape", pr1046GoMod, []string{"go.yaml.in/yaml/v3"}},
		{"msgbrowse downgrade shape", strings.Replace(pr1046GoMod, "v3.0.5", "v3.0.4", 1), []string{"go.yaml.in/yaml/v3"}},
		{"migration to v4 is not a downgrade", migratedGoMod, nil},
		{"patch bump is not a downgrade", strings.Replace(baseGoMod, "go.yaml.in/yaml/v3 v3.0.6", "go.yaml.in/yaml/v3 v3.0.7", 1), nil},
		{"requirement removal is not a downgrade", strings.Replace(baseGoMod, "\tgo.yaml.in/yaml/v3 v3.0.6\n", "", 1), nil},
		{"pseudo-version downgrade", strings.Replace(baseGoMod, "v0.0.0-20260112195520-a5071408f32f", "v0.0.0-20251201000000-abcdef123456", 1), []string{"golang.org/x/exp"}},
		{"indirect floor moving is not a downgrade", strings.Replace(baseGoMod, "golang.org/x/mod v0.41.0 // indirect", "golang.org/x/mod v0.40.0 // indirect", 1), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paths(mustDowngrades(t, baseGoMod, tt.head)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Downgrades = %v, want %v", got, tt.want)
			}
		})
	}
}

// A module indirect in base is floored by a transitive dependency, and MVS
// takes the maximum, so the version the code compiles cannot go down even if
// the head adds a lower direct line.
func TestDowngradesBaseIndirectNotFlagged(t *testing.T) {
	base := `module m

go 1.26

require (
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
`
	head := `module m

go 1.26

require go.yaml.in/yaml/v3 v3.0.4
`
	if got := paths(mustDowngrades(t, base, head)); len(got) != 0 {
		t.Errorf("Downgrades = %v, want none", got)
	}
}

func TestDowngradesReportsVersions(t *testing.T) {
	got := mustDowngrades(t, baseGoMod, pr1046GoMod)
	want := Violation{Path: "go.yaml.in/yaml/v3", BaseVer: "v3.0.6", HeadVer: "v3.0.5"}
	if !reflect.DeepEqual(got, []Violation{want}) {
		t.Errorf("Downgrades = %v, want [%v]", got, want)
	}
}
