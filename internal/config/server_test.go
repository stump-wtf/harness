package config

// Governing: ADR-0004/0008 — the optional [server] remote-access table. These
// exercise the parse contract: inline keys default read-write, [[server.key]]
// sub-tables carry read_only, an enabled server with no key source is rejected
// (no unauthenticated remote access), and a disabled/absent table is inert.
// Tilde expansion of authorized_keys_file/host_key is covered here too, since
// an unexpanded path is what silently disables the SSH front door.
//
// @joestump-agent 08/19/2026 - Made the tilde-expansion table assert on a named
// field instead of gating the comparison on a non-empty want. The old shape
// meant the "" case asserted nothing, so a regression folding "" into the bare
// ~ branch would pass CI while defeating the ADR-0008 no-key-source guard.

import (
	"os"
	"path/filepath"
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

func TestServerTildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}

	// field names which parsed path the case asserts on, so every case —
	// including the empty-path one — makes a real assertion. Gating the
	// comparison on a non-empty want silently skipped the "" case.
	tests := []struct {
		name   string
		config string
		field  string // "authorized_keys_file" or "host_key"
		want   string
	}{
		{
			name: "tilde-slash in authorized_keys_file",
			config: `
[server]
enabled = true
authorized_keys_file = "~/.ssh/harness_authorized_keys"
authorized_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@host"]
`,
			field: "authorized_keys_file",
			want:  filepath.Join(home, ".ssh/harness_authorized_keys"),
		},
		{
			name: "tilde-slash in host_key",
			config: `
[server]
enabled = true
host_key = "~/harness_hostkey"
authorized_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@host"]
`,
			field: "host_key",
			want:  filepath.Join(home, "harness_hostkey"),
		},
		{
			name: "bare tilde in authorized_keys_file",
			config: `
[server]
enabled = true
authorized_keys_file = "~"
authorized_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@host"]
`,
			field: "authorized_keys_file",
			want:  home,
		},
		{
			name: "absolute path unchanged",
			config: `
[server]
enabled = true
authorized_keys_file = "/etc/ssh/authorized_keys"
authorized_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@host"]
`,
			field: "authorized_keys_file",
			want:  "/etc/ssh/authorized_keys",
		},
		{
			name: "unset authorized_keys_file stays empty",
			config: `
[server]
enabled = true
authorized_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@host"]
`,
			field: "authorized_keys_file",
			want:  "",
		},
		{
			name: "unset host_key stays empty",
			config: `
[server]
enabled = true
authorized_keys = ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest test@host"]
`,
			field: "host_key",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.config), "test.toml")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got string
			switch tc.field {
			case "authorized_keys_file":
				got = cfg.Server.AuthorizedKeysFile
			case "host_key":
				got = cfg.Server.HostKeyPath
			default:
				t.Fatalf("unknown field %q", tc.field)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}

	tests := []struct {
		input string
		want  string
	}{
		{"~/foo", filepath.Join(home, "foo")},
		{"~", home},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
		{"~user/foo", "~user/foo"}, // only ~/ and bare ~ are expanded
	}

	for _, tc := range tests {
		got := expandHome(tc.input)
		if got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
