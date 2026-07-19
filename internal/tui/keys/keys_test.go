package keys

import (
	"reflect"
	"testing"

	"github.com/charmbracelet/bubbles/key"
)

// hasKey reports whether a binding is triggered by key string s.
func hasKey(b key.Binding, s string) bool {
	for _, k := range b.Keys() {
		if k == s {
			return true
		}
	}
	return false
}

// TestFullHelpIsExhaustive verifies SPEC-0001 REQ "Keybinding Registry": the
// help view renders the full keymap from the registry. Every exported
// key.Binding field of KeyMap must appear in FullHelp() — so pressing `?`
// always shows a complete, non-drifting keymap.
func TestFullHelpIsExhaustive(t *testing.T) {
	km := Default()

	inHelp := map[string]bool{}
	for _, row := range km.FullHelp() {
		for _, b := range row {
			inHelp[bindingID(b)] = true
		}
	}

	v := reflect.ValueOf(km)
	tp := v.Type()
	for i := 0; i < v.NumField(); i++ {
		if tp.Field(i).Type != reflect.TypeOf(key.Binding{}) {
			continue
		}
		b := v.Field(i).Interface().(key.Binding)
		if !inHelp[bindingID(b)] {
			t.Errorf("binding %q (field %s) is missing from FullHelp — help would be incomplete",
				b.Help().Key, tp.Field(i).Name)
		}
	}
}

// TestEscAlwaysBound verifies "Esc always works" (SPEC-0001): the Back binding
// includes esc, and q/esc leaves scrollback — so a user never needs a tmux
// chord to unwind.
func TestEscAlwaysBound(t *testing.T) {
	km := Default()
	if !hasKey(km.Back, "esc") {
		t.Error("esc must match Back")
	}
	if !hasKey(km.Live, "esc") {
		t.Error("esc must leave scrollback (Live)")
	}
}

// TestDefaultVerbKeys checks the SPEC-0001 default single-key dashboard verbs
// and everywhere bindings.
func TestDefaultVerbKeys(t *testing.T) {
	km := Default()
	cases := []struct {
		name string
		k    string
		b    key.Binding
	}{
		{"start", "s", km.Start},
		{"stop", "x", km.Stop},
		{"restart", "r", km.Restart},
		{"edit", "e", km.Edit},
		{"new", "n", km.New},
		{"profile", "p", km.Profile},
		{"search", "/", km.Search},
		{"help", "?", km.Help},
		{"palette-ctrlk", "ctrl+k", km.Palette},
		{"palette-colon", ":", km.Palette},
		{"hop-next", "]", km.HopNext},
		{"hop-prev", "[", km.HopPrev},
	}
	for _, c := range cases {
		if !hasKey(c.b, c.k) {
			t.Errorf("%s: key %q not bound", c.name, c.k)
		}
	}
}

// TestRebindDetach verifies the detach chord is rebindable and the help label
// follows the remap (SPEC-0001 "rebindable detach chord").
func TestRebindDetach(t *testing.T) {
	km := Default()
	if !hasKey(km.Detach, "ctrl+b d") {
		t.Fatal("default detach chord should be ctrl+b d")
	}
	km.RebindDetach("ctrl+g", "detach")
	if hasKey(km.Detach, "ctrl+b d") {
		t.Error("old chord should no longer match after rebind")
	}
	if !hasKey(km.Detach, "ctrl+g") {
		t.Error("new chord ctrl+g should match after rebind")
	}
	if km.Detach.Help().Key != "ctrl+g" {
		t.Errorf("help label should follow rebind, got %q", km.Detach.Help().Key)
	}
}

// bindingID identifies a binding by its help key + desc for set membership.
func bindingID(b key.Binding) string { return b.Help().Key + "|" + b.Help().Desc }
