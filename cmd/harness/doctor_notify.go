package main

// Doctor: Notify Hook
//
// Two rows. `notify` answers "will anyone hear about it when a harness
// fails?": whether [notify] is configured, whether its program is there and
// executable on this machine, whether the daemon is actually running with it
// (a table added since the daemon last loaded its config is not), and how the
// last delivery went. `notify_test`, only with --notify-test, has the daemon
// run the hook once with a `test` event and reports what happened — the one
// check that exercises the daemon's own environment, the script, and whatever
// the script talks to.
//
// Governing: SPEC-0003 REQ "Operator Notification"; SPEC-0001 REQ "Zero And
// Error States" (doctor is the one place every common breakage surfaces);
// issue #725.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

// notifyCheck builds the `notify` row. di is nil when daemon_info could not be
// fetched (the daemon is down, or did not answer); the row then reports the
// config alone. capable is whether the daemon speaks ProtoMinor 15, which
// added DaemonInfo.Notify: an older daemon omits it whatever it could do, so
// its silence is "unknown", never "off" or "not loaded".
func notifyCheck(cfg *core.Config, di *protocol.DaemonInfo, capable bool) check {
	row := check{name: "notify", level: cliui.LevelSuccess}
	if di != nil && !capable {
		if cfg == nil || !cfg.Notify.Enabled() {
			row.detail = "off — no [notify] table, so a failed harness reaches only the log"
			return row
		}
		row.level = cliui.LevelWarn
		row.detail = fmt.Sprintf("unknown — the daemon speaks proto %s, which predates notify (needs 1.15)", di.ProtoVersion)
		row.hint = "restart the daemon to pick up the new binary"
		return row
	}
	if cfg == nil || !cfg.Notify.Enabled() {
		if di != nil && di.Notify != nil {
			row.level = cliui.LevelWarn
			row.detail = fmt.Sprintf("the daemon is still running %s, which harness.toml no longer configures", di.Notify.Command)
			row.hint = "run `harness reload` to apply the config"
			return row
		}
		row.detail = "off — no [notify] table, so a failed harness reaches only the log"
		return row
	}
	nc := cfg.Notify
	if err := checkExecutable(nc.Command[0]); err != nil {
		row.level = cliui.LevelError
		row.detail = fmt.Sprintf("%s: %v", nc.Command[0], err)
		row.hint = "point [notify] command at an executable file, or chmod +x it"
		return row
	}
	row.detail = fmt.Sprintf("%s · %s", nc.Command[0], strings.Join(nc.Events, ", "))
	if di == nil {
		return row
	}
	if di.Notify == nil {
		row.level = cliui.LevelWarn
		row.detail = fmt.Sprintf("configured in harness.toml, but the daemon is not running it (%s)", nc.Command[0])
		row.hint = "run `harness reload` to apply the config"
		return row
	}
	if last := di.Notify.Last; last != nil {
		row.detail += " · last: " + describeDelivery(*last)
		if last.Result != "ok" {
			row.level = cliui.LevelWarn
			row.hint = "fix the hook, then prove it with `harness doctor --notify-test`"
		}
	} else {
		row.detail += " · no deliveries yet"
	}
	return row
}

// notifyTestCheck asks the daemon to run the hook once and reports the result
// as the `notify_test` row.
func notifyTestCheck(c *client.Client) check {
	row := check{name: "notify_test", level: cliui.LevelSuccess}
	d, err := c.NotifyTest()
	switch {
	case err != nil:
		row.level = cliui.LevelError
		row.detail = fmt.Sprintf("couldn't run: %v", err)
		row.hint = "restart the daemon to pick up a binary that supports notify"
	case d.Result != "ok":
		row.level = cliui.LevelError
		row.detail = describeDelivery(d)
		row.hint = "the daemon log has the hook's output (`notify: hook failed`)"
	default:
		row.detail = fmt.Sprintf("hook ran and exited 0 in %s", (time.Duration(d.DurationMs) * time.Millisecond).String())
	}
	return row
}

// describeDelivery renders one delivery for a doctor row.
func describeDelivery(d protocol.NotifyDelivery) string {
	var b strings.Builder
	b.WriteString(d.Result)
	if d.Event != "" {
		b.WriteString(" (" + d.Event)
		if d.Harness != "" {
			b.WriteString(" " + d.Harness)
		}
		b.WriteString(")")
	}
	if at, err := time.Parse(time.RFC3339, d.At); err == nil {
		b.WriteString(" " + time.Since(at).Round(time.Second).String() + " ago")
	}
	if d.Error != "" {
		b.WriteString(": " + d.Error)
	}
	return b.String()
}

// checkExecutable reports why path cannot be exec'd, or nil.
func checkExecutable(path string) error {
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Errorf("not found")
	case info.IsDir():
		return fmt.Errorf("is a directory")
	case info.Mode().Perm()&0o111 == 0:
		return fmt.Errorf("not executable")
	}
	return nil
}
