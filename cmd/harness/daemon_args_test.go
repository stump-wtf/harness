package main

import (
	"testing"
)

func TestParseDaemonArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantSub    string
		wantDAArgs []string
	}{
		{
			name:       "bare daemon means start",
			args:       nil,
			wantSub:    "",
			wantDAArgs: nil,
		},
		{
			name:       "explicit start subcommand",
			args:       []string{"start"},
			wantSub:    "start",
			wantDAArgs: nil,
		},
		{
			name:       "flags before subcommand treated as daemon args with implicit start",
			args:       []string{"--socket", "/tmp/h.sock"},
			wantSub:    "",
			wantDAArgs: []string{"--socket", "/tmp/h.sock"},
		},
		{
			name:       "multiple flags before subcommand",
			args:       []string{"--socket", "/tmp/h.sock", "--config", "/tmp/harness.toml"},
			wantSub:    "",
			wantDAArgs: []string{"--socket", "/tmp/h.sock", "--config", "/tmp/harness.toml"},
		},
		{
			name:       "subcommand followed by flags",
			args:       []string{"start", "--socket", "/tmp/h.sock"},
			wantSub:    "start",
			wantDAArgs: []string{"--socket", "/tmp/h.sock"},
		},
		{
			name:       "stop subcommand",
			args:       []string{"stop"},
			wantSub:    "stop",
			wantDAArgs: nil,
		},
		{
			name:       "flags then stop subcommand",
			args:       []string{"--socket", "/tmp/h.sock", "stop"},
			wantSub:    "stop",
			wantDAArgs: []string{"--socket", "/tmp/h.sock"},
		},
		{
			name:       "status subcommand",
			args:       []string{"status"},
			wantSub:    "status",
			wantDAArgs: nil,
		},
		{
			name:       "help long flag recognized as subcommand",
			args:       []string{"--help"},
			wantSub:    "--help",
			wantDAArgs: nil,
		},
		{
			name:       "unknown subcommand preserved for error reporting",
			args:       []string{"frobnicate"},
			wantSub:    "frobnicate",
			wantDAArgs: nil,
		},
		{
			name:       "short help flag recognized as subcommand",
			args:       []string{"-h"},
			wantSub:    "-h",
			wantDAArgs: nil,
		},
		{
			name:       "flag value equal to subcommand name not misread as verb",
			args:       []string{"--log-file", "stop"},
			wantSub:    "",
			wantDAArgs: []string{"--log-file", "stop"},
		},
		{
			name:       "flag value equal to subcommand name with equals form",
			args:       []string{"--log-file=stop"},
			wantSub:    "",
			wantDAArgs: []string{"--log-file=stop"},
		},
		{
			name:       "flag with value then real subcommand",
			args:       []string{"--config", "/tmp/h.toml", "stop"},
			wantSub:    "stop",
			wantDAArgs: []string{"--config", "/tmp/h.toml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSub, gotArgs := parseDaemonArgs(tt.args)
			if gotSub != tt.wantSub {
				t.Errorf("parseDaemonArgs(%v) sub = %q, want %q", tt.args, gotSub, tt.wantSub)
			}
			if len(gotArgs) != len(tt.wantDAArgs) {
				t.Errorf("parseDaemonArgs(%v) args = %v, want %v", tt.args, gotArgs, tt.wantDAArgs)
				return
			}
			for i := range gotArgs {
				if gotArgs[i] != tt.wantDAArgs[i] {
					t.Errorf("parseDaemonArgs(%v) args[%d] = %q, want %q", tt.args, i, gotArgs[i], tt.wantDAArgs[i])
				}
			}
		})
	}
}
