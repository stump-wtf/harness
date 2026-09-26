package config

// Notify Table
//
// Parses and validates the global [notify] table into core.NotifyConfig.
// Everything is checked at load with the offending key's line, so a hook that
// could never run is a refused config rather than an alert that silently never
// arrives: argv[0] must be an absolute path (the daemon runs under init with
// whatever PATH that gives it, and the hook is exec'd without a shell), every
// event must be one the daemon emits, and the durations must be sane. Whether
// the file exists is left to `harness doctor`: the config is also parsed on
// machines that will never run it.
//
// The table is global-only. A project file is refused it where it is refused
// [telemetry] and [mergetrain]: a cloned repository does not get to choose a
// program the daemon runs.
//
// Governing: SPEC-0003 REQ "Operator Notification"; issue #725.

import (
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// rawNotify mirrors [notify] before validation. Events and the durations are
// pointers so an absent key takes its default and an explicit empty list is
// refused rather than read as "notify about nothing".
type rawNotify struct {
	Command  []string  `toml:"command"`
	Events   *[]string `toml:"events"`
	Timeout  *string   `toml:"timeout"`
	Cooldown *string   `toml:"cooldown"`
}

// buildNotify validates rn into a core.NotifyConfig.
func buildNotify(filename string, data []byte, line int, rn rawNotify) (core.NotifyConfig, error) {
	keyLine := func(key string) int {
		if l := lineOfKeyInTable(data, "notify", key); l > 0 {
			return l
		}
		return line
	}
	fail := func(key, format string, args ...any) (core.NotifyConfig, error) {
		return core.NotifyConfig{}, newError(filename, keyLine(key), "[notify] %q: "+format, append([]any{key}, args...)...)
	}

	nc := core.NotifyConfig{
		Events:   slices.Clone(core.DefaultNotifyEvents),
		Timeout:  core.DefaultNotifyTimeout,
		Cooldown: core.DefaultNotifyCooldown,
	}

	switch {
	case len(rn.Command) == 0:
		return fail("command", "is required: the hook's argv, e.g. [\"/home/me/.config/harness/notify.sh\"] (remove the [notify] table to turn notification off)")
	case strings.TrimSpace(rn.Command[0]) == "":
		return fail("command", "argv[0] must not be blank (it is the executable)")
	case !filepath.IsAbs(rn.Command[0]):
		return fail("command", "argv[0] must be an absolute path (got %q): the hook is exec'd without a shell, from the daemon's environment", rn.Command[0])
	}
	nc.Command = slices.Clone(rn.Command)

	if rn.Events != nil {
		if len(*rn.Events) == 0 {
			return fail("events", "must list at least one of: %s (omit the key for the default set)", strings.Join(core.NotifyEvents, ", "))
		}
		nc.Events = nil
		for _, e := range *rn.Events {
			e = strings.TrimSpace(e)
			if !slices.Contains(core.NotifyEvents, e) {
				return fail("events", "unknown event %q (want any of: %s)", e, strings.Join(core.NotifyEvents, ", "))
			}
			if slices.Contains(nc.Events, e) {
				return fail("events", "lists %q twice", e)
			}
			nc.Events = append(nc.Events, e)
		}
	}

	if rn.Timeout != nil {
		v, err := time.ParseDuration(strings.TrimSpace(*rn.Timeout))
		if err != nil || v < core.MinNotifyTimeout || v > core.MaxNotifyTimeout {
			return fail("timeout", "must be a duration between %s and %s (got %q)", core.MinNotifyTimeout, core.MaxNotifyTimeout, *rn.Timeout)
		}
		nc.Timeout = v
	}
	if rn.Cooldown != nil {
		v, err := time.ParseDuration(strings.TrimSpace(*rn.Cooldown))
		if err != nil || v < 0 || v > core.MaxNotifyCooldown {
			return fail("cooldown", "must be a duration between 0s and %s (got %q; 0s turns de-duplication off)", core.MaxNotifyCooldown, *rn.Cooldown)
		}
		nc.Cooldown = v
	}
	return nc, nil
}
