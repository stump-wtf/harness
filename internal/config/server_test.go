package config

// Governing: ADR-0004/0008 — the optional [server] remote-access table. These
// exercise the parse contract: inline keys default read-write, [[server.key]]
// sub-tables carry read_only, an enabled server with no key source is rejected
// (no unauthenticated remote access), and a disabled/absent table is inert.

import (
	"strings"
	"testing"
)

func TestParseServerTable(t *testing.T) {
	const edKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample joe@laptop"

	tests := []struct {
		name        string
		body        string
		wantErr     bool
		errContains string
	}{
		{
			name: "enabled with inline key defaults read-write",
			body: `
[server]
enabled = true
listen = "127.0.0.1:2200"
authorized_keys = ["` + edKey + `"]
`,
		},
		{
			name: "per-key read_only sub-table",
			body: `
[server]
enabled = true
authorized_keys = ["` + edKey + `"]

[[server.key]]
key = "` + edKey + `"
read_only = true
`,
		},
		{
			name: "authorized_keys_file alone satisfies enable",
			body: `
[server]
enabled = true
authorized_keys_file = "/etc/harness/authorized_keys"
`,
		},
		{
			name:        "enabled with no keys is rejected",
			body:        "[server]\nenabled = true\n",
			wantErr:     true,
			errContains: "authorized_keys",
		},
		{
			name: "disabled server needs no keys",
			body: "[server]\nenabled = false\n",
		},
		{
			// A duplicate [server] table is rejected — the TOML decoder catches
			// it as a redefined key before the parser's own guard.
			name:        "duplicate server table",
			body:        "[server]\nenabled = false\n[server]\nenabled = false\n",
			wantErr:     true,
			errContains: "server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.body), "test.toml")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", cfg)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestServerValues verifies the parsed values land on core.ServerConfig with
// the right read-only flags and normalization.
func TestServerValues(t *testing.T) {
	const rwKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIRWkey rw@host"
	const roKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIROkey ro@host"
	body := `
[server]
enabled = true
listen = "0.0.0.0:2222"
host_key = "/var/lib/harness/hostkey"
authorized_keys = ["` + rwKey + `"]

[[server.key]]
key = "` + roKey + `"
read_only = true
`
	cfg, err := Parse([]byte(body), "test.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sc := cfg.Server
	if !sc.Enabled {
		t.Fatal("Enabled should be true")
	}
	if sc.Listen != "0.0.0.0:2222" {
		t.Fatalf("Listen = %q", sc.Listen)
	}
	if sc.HostKeyPath != "/var/lib/harness/hostkey" {
		t.Fatalf("HostKeyPath = %q", sc.HostKeyPath)
	}
	if len(sc.AuthorizedKeys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(sc.AuthorizedKeys))
	}
	if sc.AuthorizedKeys[0].ReadOnly {
		t.Error("inline authorized_keys entry should default to read-write")
	}
	if !sc.AuthorizedKeys[1].ReadOnly {
		t.Error("[[server.key]] read_only=true should be read-only")
	}
}
