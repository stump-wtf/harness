package buildinfo

// Governing: stump.wtf/harness#181 — the daemon is deliberately long-lived
// while the client is rebuilt from main constantly, and proto version can
// never detect build drift (the wire format genuinely didn't change across 57
// commits). This is the one place that decides what client/daemon build skew
// is worth saying, so doctor, daemon-info, and the TUI banner all speak in one
// voice. Deliberately advisory only: the halves are wire-compatible within a
// proto major, and nothing anywhere gates a connection on it.

import (
	"fmt"
	"strconv"
	"strings"
)

// commitCountOf parses the "commits since tag" component of a git-describe
// version ("v0.1.0-153-g3652d01" → tag "v0.1.0", 153). ok is false for
// formats without one ("dev", "test", "v1.2.3") — those carry no ordering.
func commitCountOf(v string) (tag string, n int, ok bool) {
	rest := strings.TrimPrefix(v, "v")
	parts := strings.Split(rest, "-")
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "g") {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, false
	}
	return "v" + parts[0], n, true
}

// SkewNotice returns a human notice about client/daemon build skew, or ""
// when there is nothing worth saying: either side unversioned ("dev"/empty —
// the normal dev-workflow build), or the same version. The daemon's version
// comes first because "the daemon is behind" is the common and actionable
// case: the daemon owns PTYs, spawning, sizing and supervision, and only
// picks up fixes on restart.
func SkewNotice(daemon, client string) string {
	if daemon == "" || client == "" || daemon == "dev" || client == "dev" || daemon == client {
		return ""
	}
	dTag, dN, dOK := commitCountOf(daemon)
	cTag, cN, cOK := commitCountOf(client)
	if dOK && cOK && dTag == cTag && dN != cN {
		if dN < cN {
			return fmt.Sprintf("daemon %s is %d commits behind client %s — restart the daemon to pick up fixes", daemon, cN-dN, client)
		}
		return fmt.Sprintf("daemon %s is newer than client %s — reinstall the client to align", daemon, client)
	}
	return fmt.Sprintf("client %s and daemon %s are different builds — restart the daemon (or reinstall the client) to align", client, daemon)
}
