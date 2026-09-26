package client

// Governing tests: SPEC-0003 REQ "Operator Notification" — ProtoMinor 15
// added DaemonInfo.Notify and notify_test. A 1.14 daemon (13 is the persona
// keys, #718; 14 the env_file list, #719) knows neither, so the client must
// not treat it as notify-capable.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// The refusal happens before any request is written: the Client here has no
// connection, so reaching the wire would panic rather than pass.
func TestNotifyTestRefusesAnOlderDaemon(t *testing.T) {
	for _, v := range []string{"1.14", "1.13", "1", ""} {
		c := &Client{daemon: protocol.Hello{ProtoVersion: v}}
		if c.SupportsNotify() {
			t.Errorf("proto %q reported notify-capable", v)
		}
		if _, err := c.NotifyTest(); err == nil || !strings.Contains(err.Error(), "predates notify (needs 1.15)") {
			t.Errorf("proto %q: err = %v, want a refusal", v, err)
		}
	}
	for _, v := range []string{"1.15", protocol.ProtoVersion} {
		if c := (&Client{daemon: protocol.Hello{ProtoVersion: v}}); !c.SupportsNotify() {
			t.Errorf("proto %q not reported notify-capable", v)
		}
	}
}
