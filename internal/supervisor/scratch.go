package supervisor

// Governing: ADR-0017 (ephemeral scratchpad harnesses), SPEC-0011 REQ "Scratchpad
// Creation", REQ "Name Minting", REQ "Ephemerality (No Persistence)".
//
// A scratchpad is the third registration class alongside global (config-owned,
// durable) and project (repo-owned, durable until down/rm): operator-owned and
// throwaway. It is registered by ScratchRun with provenance "scratch", gets a
// full supervisor, and is NEVER written to state.json — ephemerality is
// structural (Save skips scratch provenance; the supervisor gets no OnChange
// hook), not a per-callsite discipline. Teardown is RemoveHarness (the same op
// `harness rm` uses for project members).

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// ProvenanceScratch is the provenance value marking a scratchpad harness. It
// is projected into HarnessInfo.Project so clients can badge scratchpads
// without a protocol change (SPEC-0011 REQ "Supervisor Parity").
const ProvenanceScratch = "scratch"

// slugMax caps the slug half of a minted name so long invocations produce
// readable names, not walls of text.
const slugMax = 40

// slugRe collapses everything that is not [a-z0-9-].
var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify builds the deterministic half of a scratchpad name from the
// invocation words: lowercase, non-alphanumerics to "-", collapsed and
// trimmed, capped at slugMax.
func slugify(words []string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(strings.Join(words, "-")), "-")
	s = strings.Trim(s, "-")
	if len(s) > slugMax {
		s = s[:slugMax]
		s = strings.TrimRight(s, "-")
	}
	return s
}

// suffixMint returns one random 4-character base36 suffix (SPEC-0011 REQ "Name
// Minting"). crypto/rand, not math/rand: the name space is user-visible and
// collision resistance should not depend on seeding.
func suffixMint() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 4)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("scratchpad: mint suffix: %w", err)
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// ScratchRun registers one ad-hoc scratchpad and starts it, returning the
// minted name (SPEC-0011 REQ "Scratchpad Creation"). The name is
// <slug>-<suffix>; minting retries on collision with any registered name under
// the registry lock, so concurrent runs can never collide. Validation reuses
// the project-definition rules (the wire is the same shape); an empty slug or
// definition is ErrInvalidProjectDef. The restart policy defaults to "no" —
// session semantics: an exited scratchpad stays registered for inspection
// until rm, never respawned.
func (m *Manager) ScratchRun(h core.Harness, slug string) (string, error) {
	if strings.TrimSpace(h.Name) != "" {
		slug = h.Name
	}
	slug = slugify([]string{slug})
	if slug == "" {
		return "", fmt.Errorf("scratchpad: %w: empty name", ErrInvalidProjectDef)
	}
	// Session semantics unless the definition explicitly chose a policy
	// (empty means the parsers' default, which ScratchRun overrides to no).
	if h.Restart == "" {
		h.Restart = core.RestartNo
	}
	if h.Backend == "" {
		h.Backend = core.BackendNative
	}
	h.Enabled = true
	// Validation runs against the slug as a stand-in name: the rules are
	// shape rules (kind, prompt/args exclusivity, backend, restart), and the
	// real name does not exist until it is minted under the lock below.
	h.Name = slug
	if err := validateHarnessDef("scratchpad", h); err != nil {
		return "", err
	}

	m.mu.Lock()
	var name string
	for range 8 {
		suffix, err := suffixMint()
		if err != nil {
			m.mu.Unlock()
			return "", err
		}
		candidate := slug + "-" + suffix
		if _, taken := m.supervisors[candidate]; taken {
			continue
		}
		name = candidate
		break
	}
	if name == "" {
		m.mu.Unlock()
		return "", fmt.Errorf("scratchpad: %w: could not mint a unique name", ErrInvalidProjectDef)
	}
	h.Name = name
	m.addEphemeralSupervisorLocked(h)
	s := m.supervisors[name]
	m.order = append(m.order, name)
	m.provenance[name] = ProvenanceScratch
	m.scratchDefs[name] = h
	m.mu.Unlock()

	// Start outside the lock (the supervisor's actor loop is independently
	// synchronized); s was captured under the lock, not re-read from the map.
	s.Start()
	return name, nil
}
