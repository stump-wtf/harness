package source

// Source reconciliation after a config reload.
//
// This is the load-bearing half of the reload story, and the reason is
// concrete rather than theoretical: chezmoi (and czu on its timer) rewrite
// harness.toml periodically, byte-identically. A listener that reconnected on
// every rewrite would drop doorbells and could never hold a stream — the same
// failure ADR-0013's rebuild-on-reload scheduler had, where an `@every 6h` job
// never fired because every reload restarted its countdown.
//
// So reconciliation is INCREMENTAL and identity-based. A source whose
// normalized URL, resolved-header fingerprint and `enabled` are unchanged, and
// which is still bound, keeps its session and its open stream untouched —
// however its set of bound harnesses changed, because that set is re-read per
// firing rather than baked into the session.
//
// Governing: ADR-0021; SPEC-0014 REQ "Source Reconciliation On Reload", REQ
// "Channel Catch-Up".
//
// @joestump 09/23/2026 - Introduced with SPEC-0014 channel reconnection (#474).

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

// identity is what decides whether a live session still serves a source.
//
// Deliberately NOT the whole ChannelSource: `description` is prose, and a
// reload that only reworded it must not drop a stream. Deliberately not just
// the URL either — a rotated token is a different session, and keeping the old
// one would leave the daemon authenticated with a credential the operator has
// revoked.
type identity struct {
	// endpoint is the normalized URL, so a cosmetic rewrite (a trailing
	// slash, an explicit :443, a capitalized host) is not an identity change.
	endpoint string
	// headers is a fingerprint of the RESOLVED header values, never the
	// values. It changes when a token rotates, which is what makes REQ
	// "Source Reconciliation On Reload"'s token-rotation scenario work.
	headers string
	// enabled is the source's own switch.
	enabled bool
}

// identityOf computes a source's identity.
func identityOf(src core.ChannelSource) identity {
	return identity{
		endpoint: core.NormalizeEndpoint(src.URL),
		headers:  headerFingerprint(src.Headers),
		enabled:  src.Enabled,
	}
}

// headerFingerprint hashes a source's resolved headers.
//
// A hash rather than the values, because this is held in memory for the life
// of the daemon and compared on every reload; a struct full of live
// credentials would be one careless `%+v` from a log line. The name is
// included alongside the value so renaming a header to one carrying the same
// value is still a change.
func headerFingerprint(headers map[string]core.Secret) string {
	if len(headers) == 0 {
		return ""
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		// Length-prefixed, so {"AB": "C"} and {"A": "BC"} cannot collide.
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(headers[name].Reveal()))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Reconcile applies a reloaded config to the live sources.
//
// It is called only after a SUCCESSFUL reload: a parse error keeps the
// last-good config, and a failed reload must change nothing (REQ "Source
// Reconciliation On Reload"). The daemon wires it onto the same hook the
// scheduler's re-apply uses, so there is one definition of "the config
// changed".
func (m *Manager) Reconcile(cfg *core.Config) {
	if cfg == nil {
		return
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	m.mu.Lock()
	if m.closed || m.ctx == nil {
		m.mu.Unlock()
		return
	}
	ctx := m.ctx

	// What the new config wants, keyed by reference.
	want := map[string]core.ChannelSource{}
	for _, src := range cfg.OrderedChannels() {
		want[core.SourceKindChannel+"."+src.Name] = src
	}

	// Sessions to end, and sources to start, collected under the lock and
	// acted on after it: ending a session waits for its goroutine, which
	// takes the lock itself.
	var toEnd []*sourceState
	// Sessions being replaced, so each replacement inherits its session's
	// own history once that session has ended.
	replaced := map[string]*sourceState{}
	for ref, st := range m.sources {
		if st.Kind() != core.SourceKindChannel {
			continue
		}
		src, still := want[ref]
		bound := len(cfg.BoundHarnesses(ref)) > 0
		switch {
		case !still:
			// Removed from the config.
			if st.cancel != nil {
				toEnd = append(toEnd, st)
			}
			delete(m.sources, ref)
		case !src.Enabled, !bound:
			// Disabled, or nothing binds it any more. A live session ends;
			// a record that never had one stays, so the pass below reports
			// a change only if the state really changed.
			if st.cancel != nil {
				toEnd = append(toEnd, st)
				delete(m.sources, ref)
			}
		case st.cancel == nil:
			// Was disabled or unbound, and now is neither: start it below
			// as a fresh source.
			delete(m.sources, ref)
		case st.identity != identityOf(src):
			// The endpoint or the credential changed: this is a different
			// session, not the same one with a new label.
			if st.cancel != nil {
				toEnd = append(toEnd, st)
				replaced[ref] = st
			}
			delete(m.sources, ref)
		default:
			// Unchanged and still bound: keep the session and the open
			// stream untouched. This branch is the whole point of the file —
			// it is what a byte-identical rewrite must reach.
			//
			// The set of bound harnesses may well have changed, and that is
			// fine: Fire re-reads it per event, so the change applies from
			// the next event without touching the stream.
			continue
		}
	}
	m.mu.Unlock()

	// Cancel every session first, then wait: each one's DELETE can take a
	// few seconds against a slow server, and waiting on them one at a time
	// would hold the reload for the sum rather than the longest.
	for _, st := range toEnd {
		st.cancel()
	}
	for _, st := range toEnd {
		if st.done != nil {
			<-st.done
		}
	}

	// Anything the new config wants that is not live now: start it, or
	// record why it is not running.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.reconcileWebhooksLocked(cfg)
	for _, src := range cfg.OrderedChannels() {
		ref := core.SourceKindChannel + "." + src.Name
		existing := m.sources[ref]
		if existing != nil && existing.cancel != nil {
			continue // live and unchanged
		}
		switch {
		case !src.Enabled:
			if existing == nil || existing.status.State != trigger.StateDisabled {
				m.setStatusLocked(ref, core.SourceKindChannel, trigger.StateDisabled, "")
			}
		case len(cfg.BoundHarnesses(ref)) == 0:
			if existing == nil || existing.status.State != trigger.StateUnbound {
				m.setStatusLocked(ref, core.SourceKindChannel, trigger.StateUnbound, "")
			}
		default:
			// A replacement inherits the replaced session's own history,
			// read after it ended: whether it ever connected, and when it
			// went down. A session closed with its stream open went down
			// "now", so a reload-caused reconnect measures no outage; one
			// closed mid-outage keeps the outage's real start, because the
			// reload does not make the doorbells it missed any less missed.
			seed := sessionSeed{downSince: m.now()}
			if old, ok := replaced[ref]; ok {
				seed = old.exit
			}
			m.startSessionLocked(ctx, ref, src, seed)
		}
	}
}

// Kind is the source kind this state describes.
func (s *sourceState) Kind() string { return s.status.Kind }
