package keys

// Governing: SPEC-0001 (the footer help strip is the discoverability surface —
// every binding the operator can reach should be findable without leaving the
// cockpit).
//
// TestFullHelpIsExhaustive already guarantees the *full* help lists every
// binding, via reflection, so a newly-added KeyMap field cannot hide. The two
// SHORT help strips had no coverage at all: they are hand-curated slices, so a
// binding removed from one — or a strip that drifts to show attached bindings
// on the dashboard — fails no test while quietly making a feature
// undiscoverable.

import (
	"testing"

	"charm.land/bubbles/v2/key"
)

// helpIDs renders a binding slice to its help keys for comparison.
func helpIDs(bs []key.Binding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Help().Key)
	}
	return out
}

// TestShortHelpIsNonEmptyAndEnabled pins that the dashboard strip actually has
// content and that every entry is a real, enabled binding. A disabled binding
// renders as help text for a key that does nothing.
func TestShortHelpIsNonEmpty(t *testing.T) {
	k := Default()
	short := k.ShortHelp()
	if len(short) == 0 {
		t.Fatal("ShortHelp() is empty; the dashboard footer would render no hints")
	}
	for i, b := range short {
		if !b.Enabled() {
			t.Errorf("ShortHelp()[%d] (%q) is disabled but shown in the footer", i, b.Help().Key)
		}
		if b.Help().Key == "" {
			t.Errorf("ShortHelp()[%d] has an empty help key", i)
		}
		if b.Help().Desc == "" {
			t.Errorf("ShortHelp()[%d] (%q) has no description", i, b.Help().Key)
		}
	}
}

// TestAttachedShortHelpIsNonEmpty pins the same for the attached strip.
func TestAttachedShortHelpIsNonEmpty(t *testing.T) {
	k := Default()
	short := k.AttachedShortHelp()
	if len(short) == 0 {
		t.Fatal("AttachedShortHelp() is empty; the attached ribbon would render no hints")
	}
	for i, b := range short {
		if !b.Enabled() {
			t.Errorf("AttachedShortHelp()[%d] (%q) is disabled but shown", i, b.Help().Key)
		}
		if b.Help().Key == "" {
			t.Errorf("AttachedShortHelp()[%d] has an empty help key", i)
		}
	}
}

// TestShortHelpStripsDiffer pins that the two strips are actually
// context-specific. If they ever became identical it would mean one surface is
// advertising the other's bindings — attached mode showing dashboard keys that
// are being forwarded to the agent instead, which is actively misleading.
func TestShortHelpStripsDiffer(t *testing.T) {
	k := Default()
	dash := helpIDs(k.ShortHelp())
	att := helpIDs(k.AttachedShortHelp())

	if len(dash) == len(att) {
		same := true
		for i := range dash {
			if dash[i] != att[i] {
				same = false
				break
			}
		}
		if same {
			t.Error("ShortHelp() and AttachedShortHelp() are identical; the two modes have different bindings and should advertise different ones")
		}
	}
}

// TestShortHelpEntriesAppearInFullHelp pins the containment relationship: the
// short strip is a curated subset of the full help, so an entry in the strip
// that is absent from the full list means the two have drifted apart and the
// operator sees a key in the footer that the `?` overlay does not explain.
func TestShortHelpEntriesAppearInFullHelp(t *testing.T) {
	k := Default()

	full := map[string]bool{}
	for _, group := range k.FullHelp() {
		for _, b := range group {
			full[b.Help().Key] = true
		}
	}

	for _, strip := range [][]key.Binding{k.ShortHelp(), k.AttachedShortHelp()} {
		for _, b := range strip {
			if id := b.Help().Key; id != "" && !full[id] {
				t.Errorf("short-help entry %q is not present in FullHelp(); the ? overlay would not explain it", id)
			}
		}
	}
}

// TestRebindDetachFlowsIntoAttachedHelp pins that a rebind is reflected in the
// strip the operator actually reads while attached. RebindDetach already has a
// test for the binding itself; this covers the help surface, which is where a
// stale label would mislead someone into pressing the old chord.
func TestRebindDetachFlowsIntoAttachedHelp(t *testing.T) {
	k := Default()
	before := helpIDs(k.AttachedShortHelp())

	k.RebindDetach("ctrl+q", "detach")
	after := helpIDs(k.AttachedShortHelp())

	if len(before) != len(after) {
		t.Fatalf("rebind changed the strip length: %d -> %d", len(before), len(after))
	}
	changed := false
	for i := range before {
		if before[i] != after[i] {
			changed = true
			break
		}
	}
	if !changed {
		t.Error("rebinding detach did not change the attached help strip; the footer would advertise the old chord")
	}
}
