// Package stack owns `harness stack`: the pinned Compose bundle installer.
// The manifest is the contract — every image pinned by tag AND digest from a
// file embedded in each release, so the installer installs only what CI
// tested together (SPEC-0018 REQ-16; ADR-0024).
package stack

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed manifest.toml
var rawManifest string

// Sentinels (SPEC-0018 REQ-29).
var (
	ErrManifestInvalid = errors.New("stack: manifest invalid")
	// ErrOverrideNoDigest marks an --image override without a sha256 digest:
	// the flag half lives in `stack init` (#442), the validator half here.
	ErrOverrideNoDigest = errors.New("stack: image override without a sha256 digest")
)

// Component is one pinned service image.
type Component struct {
	Image   string `toml:"image"`
	Tag     string `toml:"tag"`
	Digest  string `toml:"digest"`
	Minimum string `toml:"minimum"`
	Major   int    `toml:"major"` // Postgres only: the pinned data-directory major
}

// Plugin pins one skills plugin (SPEC-0018 REQ-14, amended): a release tag
// where the repository has one, a commit SHA otherwise — never a branch.
type Plugin struct {
	Repo string `toml:"repo"` // the PUBLIC GitHub mirror, stump-wtf/*
	Ref  string `toml:"ref"`
}

// Note is one upgrade note carried between manifest versions (REQ-23).
type Note struct {
	Component string `toml:"component"`
	Version   string `toml:"version"`
	Breaking  bool   `toml:"breaking"`
	Text      string `toml:"text"`
}

// Manifest is the parsed, validated document.
type Manifest struct {
	Version    int                   `toml:"version"`
	Postgres   Component             `toml:"postgres"`
	Switch     Component             `toml:"switchboard"`
	Cairn      Component             `toml:"cairn"`
	ObjStore   Component             `toml:"objectstore"`
	Caddy      Component             `toml:"caddy"`
	Plugins    map[string]Plugin     `toml:"plugin"`
	Notes      []Note                `toml:"note"`
	components map[string]*Component // name -> component, for notes/overrides
}

// Load parses and validates the embedded manifest. It never reaches the
// network: the digest trust anchor is this file, reviewed in PRs and bumped
// by Renovate.
func Load() (*Manifest, error) {
	return Parse(rawManifest)
}

// Parse validates a manifest document; Load is Parse over the embedded copy.
// A `latest` tag, a missing digest, a minimum newer than the tag, a plugin
// ref that names a branch, or a note naming an unknown component is an error
// naming the component (the unit test turns each into a build failure).
func Parse(doc string) (*Manifest, error) {
	m := &Manifest{}
	meta, err := toml.Decode(doc, m)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestInvalid, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			names = append(names, k.String())
		}
		return nil, fmt.Errorf("%w: unknown keys: %s", ErrManifestInvalid, strings.Join(names, ", "))
	}

	m.components = map[string]*Component{
		"postgres":    &m.Postgres,
		"switchboard": &m.Switch,
		"cairn":       &m.Cairn,
		"objectstore": &m.ObjStore,
		"caddy":       &m.Caddy,
	}
	// A fixed order, so a manifest with several bad components reports the
	// same one on every run.
	for _, name := range componentNames {
		if err := validateComponent(name, m.components[name]); err != nil {
			return nil, err
		}
	}
	if m.Version < 1 {
		return nil, fmt.Errorf("%w: \"version\" must be set", ErrManifestInvalid)
	}
	if len(m.Plugins) == 0 {
		return nil, fmt.Errorf("%w: [plugin.*] pins are required (REQ-14)", ErrManifestInvalid)
	}
	pluginNames := make([]string, 0, len(m.Plugins))
	for name := range m.Plugins {
		pluginNames = append(pluginNames, name)
	}
	sort.Strings(pluginNames)
	for _, name := range pluginNames {
		if err := validatePlugin(name, m.Plugins[name]); err != nil {
			return nil, err
		}
	}
	for _, n := range m.Notes {
		if _, ok := m.components[n.Component]; !ok {
			return nil, fmt.Errorf("%w: [[note]] references unknown component %q", ErrManifestInvalid, n.Component)
		}
		if strings.TrimSpace(n.Text) == "" {
			return nil, fmt.Errorf("%w: [[note]] for %q has no text", ErrManifestInvalid, n.Component)
		}
	}
	return m, nil
}

// componentNames is the validation order of the five pinned components.
var componentNames = []string{"postgres", "switchboard", "cairn", "objectstore", "caddy"}

var (
	digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// A commit SHA is 7–40 hex; a release tag is dotted numbers with an
	// optional leading v and an optional -pre/+build suffix (v0.1.0, 1.2,
	// v2.0.0-rc.1). Everything else — main, develop, release/*, 1.x — is a
	// branch, and a branch never pins a skill install (REQ-14). A bare "v2"
	// stays ambiguous (tag or branch) and is refused: pin v2.0.0 or a SHA.
	shaRe    = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	semverRe = regexp.MustCompile(`^v?\d+(\.\d+)+([-+][0-9A-Za-z.-]+)?$`)
)

func validateComponent(name string, c *Component) error {
	tag := strings.TrimSpace(c.Tag)
	switch {
	case tag == "":
		return fmt.Errorf("%w: %s: \"tag\" is empty", ErrManifestInvalid, name)
	case strings.EqualFold(tag, "latest"), strings.EqualFold(tag, "stable"):
		return fmt.Errorf("%w: %s: tag %q is forbidden — pin a version (REQ-16)", ErrManifestInvalid, name, tag)
	}
	if !digestRe.MatchString(c.Digest) {
		return fmt.Errorf("%w: %s: \"digest\" must be sha256:<64 hex> (got %.20q…)", ErrManifestInvalid, name, c.Digest)
	}
	if c.Minimum != "" && compareLoose(c.Minimum, tag) > 0 {
		return fmt.Errorf("%w: %s: \"minimum\" %s exceeds \"tag\" %s", ErrManifestInvalid, name, c.Minimum, tag)
	}
	// The data-directory major must follow the tag: Renovate rewrites tag and
	// digest only, so a 17 -> 18 bump that forgot `major` fails here instead
	// of pointing an 18.x server at a 17 data directory.
	if c.Major != 0 {
		if tagMajor := splitNumeric(strings.TrimPrefix(tag, "v"))[0]; tagMajor != c.Major {
			return fmt.Errorf("%w: %s: \"major\" %d does not match tag %s (major %d)", ErrManifestInvalid, name, c.Major, tag, tagMajor)
		}
	}
	return nil
}

func validatePlugin(name string, p Plugin) error {
	if p.Repo == "" || !strings.HasPrefix(p.Repo, "stump-wtf/") {
		return fmt.Errorf("%w: plugin %q: \"repo\" must be a public stump-wtf/* mirror (fetches never touch the private Gitea host)", ErrManifestInvalid, name)
	}
	ref := strings.TrimSpace(p.Ref)
	if ref == "" {
		return fmt.Errorf("%w: plugin %q: \"ref\" is required", ErrManifestInvalid, name)
	}
	if shaRe.MatchString(ref) || semverRe.MatchString(ref) {
		return nil
	}
	return fmt.Errorf("%w: plugin %q: ref %q looks like a branch — pin a release tag, or a commit SHA until the tag exists (REQ-14)", ErrManifestInvalid, name, ref)
}

// ImageRef renders repo:tag@digest, the only image form the bundle writes.
func (c Component) ImageRef() string {
	return fmt.Sprintf("%s:%s@%s", c.Image, c.Tag, c.Digest)
}

// ValidateOverride enforces the flag half's contract (REQ-16 "An override
// without a digest"): an --image override MUST carry repo:tag@sha256:….
func ValidateOverride(ref string) error {
	at := strings.LastIndex(ref, "@")
	if at < 0 || !digestRe.MatchString(ref[at+1:]) {
		return fmt.Errorf("%w: %q (want repo:tag@sha256:<64 hex>)", ErrOverrideNoDigest, ref)
	}
	return nil
}

// compareLoose orders v1 vs v2 over dotted numeric prefixes ("v0.4.0" vs
// "17.6"), stripping a leading v. Missing trailing parts count as zero, so
// "17" == "17.0". It is deliberately approximate: exact semver ordering is
// Renovate's job, this only enforces minimum <= tag.
func compareLoose(v1, v2 string) int {
	p1 := splitNumeric(strings.TrimPrefix(v1, "v"))
	p2 := splitNumeric(strings.TrimPrefix(v2, "v"))
	for i := 0; i < len(p1) || i < len(p2); i++ {
		a, b := part(p1, i), part(p2, i)
		if a != b {
			if a > b {
				return 1
			}
			return -1
		}
	}
	return 0
}

func part(p []int, i int) int {
	if i < len(p) {
		return p[i]
	}
	return 0
}

func splitNumeric(v string) []int {
	var out []int
	cur := 0
	digits := false
	for _, r := range v {
		if r >= '0' && r <= '9' {
			cur = cur*10 + int(r-'0')
			digits = true
			continue
		}
		if r == '.' {
			out = append(out, cur)
			cur, digits = 0, false
			continue
		}
		if digits { // non-numeric suffix (e.g. -rc1): everything before wins
			break
		}
	}
	out = append(out, cur)
	return out
}
