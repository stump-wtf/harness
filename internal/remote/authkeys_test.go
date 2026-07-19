package remote

// Governing: ADR-0008 — allowlist resolution and per-key read-only scoping.
// Table-driven over the parse + authorize contract.

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// newKey mints an ed25519 SSH keypair, returning the wire public key and its
// authorized_keys line (no trailing newline).
func newKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	line := string(gossh.MarshalAuthorizedKey(sshPub))
	// MarshalAuthorizedKey appends a newline; the config path trims, keep raw.
	return sshPub, line
}

func TestLoadAllowlistInlineKeys(t *testing.T) {
	rwPub, rwLine := newKey(t)
	roPub, roLine := newKey(t)
	strangerPub, _ := newKey(t) // a key NOT on the list

	al, err := loadAllowlist([]core.AuthorizedKey{
		{Line: rwLine, ReadOnly: false},
		{Line: roLine, ReadOnly: true},
	}, "")
	if err != nil {
		t.Fatalf("loadAllowlist: %v", err)
	}
	if len(al) != 2 {
		t.Fatalf("want 2 entries, got %d", len(al))
	}

	tests := []struct {
		name   string
		key    ssh.PublicKey
		wantOK bool
		wantRO bool
	}{
		{name: "read-write key authorized", key: rwPub, wantOK: true, wantRO: false},
		{name: "read-only key authorized read-only", key: roPub, wantOK: true, wantRO: true},
		{name: "unlisted key refused", key: strangerPub, wantOK: false, wantRO: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ro, ok := al.authorize(tt.key)
			if ok != tt.wantOK {
				t.Fatalf("authorize ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && ro != tt.wantRO {
				t.Fatalf("authorize readOnly = %v, want %v", ro, tt.wantRO)
			}
		})
	}
}

func TestParseAuthorizedKeysBlobReadOnlyAnnotation(t *testing.T) {
	rwPub, rwLine := newKey(t)
	roPub, roLine := newKey(t)

	// Build a file body: one plain entry, one prefixed with the read-only
	// option, plus a comment and a blank line to exercise skipping.
	blob := "# harnessd authorized_keys\n" +
		rwLine + "\n" +
		"\n" +
		"read-only " + roLine + "\n"

	al, err := parseAuthorizedKeysBlob([]byte(blob))
	if err != nil {
		t.Fatalf("parse blob: %v", err)
	}
	if len(al) != 2 {
		t.Fatalf("want 2 keys, got %d", len(al))
	}
	if ro, ok := al.authorize(rwPub); !ok || ro {
		t.Fatalf("plain entry: ok=%v ro=%v, want ok=true ro=false", ok, ro)
	}
	if ro, ok := al.authorize(roPub); !ok || !ro {
		t.Fatalf("read-only entry: ok=%v ro=%v, want ok=true ro=true", ok, ro)
	}
}

func TestLoadAllowlistBadKey(t *testing.T) {
	_, err := loadAllowlist([]core.AuthorizedKey{{Line: "not-a-real-key"}}, "")
	if err == nil {
		t.Fatal("expected error for malformed authorized key")
	}
}
