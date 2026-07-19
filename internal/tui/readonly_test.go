package tui

// Governing: ADR-0008 (read-only attach), SPEC-0002. Closes the flagged gap
// where attachTo hardcoded AttachRW: a Model built with Options.ReadOnly (a
// read-only remote SSH session, or a --read-only authorized-keys annotation)
// must open every attach as protocol.AttachRO so the daemon drops its input.

import "testing"

func TestReadOnlyOptionThreadsAttachRO(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		wantRO   bool
	}{
		{name: "read-only session opens RO", readOnly: true, wantRO: true},
		{name: "default session opens RW", readOnly: false, wantRO: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeController{harnesses: sampleHarnesses()}
			fa := &fakeAttach{}
			m := New(Options{ReadOnly: tt.readOnly})
			m.ctrl, m.attach = fc, fa
			m.harnesses = fc.harnesses
			m.w, m.h = 80, 24

			drain(m.attachTo(m.harnesses[0], 0))

			if m.att == nil {
				t.Fatal("attachTo should have opened an attach state")
			}
			if got := m.att.readOnly(); got != tt.wantRO {
				t.Fatalf("attach readOnly = %v, want %v (Options.ReadOnly=%v)", got, tt.wantRO, tt.readOnly)
			}
			if len(fa.opens) != 1 {
				t.Fatalf("expected one AttachOpen, got opens=%v", fa.opens)
			}
		})
	}
}
