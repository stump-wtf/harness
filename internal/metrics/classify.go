package metrics

// Model Error Classifier
//
// An agent's error mark carries the provider's refusal as text. This maps that
// text to one of five classes where the error is observed, so an alert rule
// never has to regex provider wording (SPEC-0013 REQ-3). The classes are
// operational situations, not error types:
//
//   - quota: the account may not make calls right now — a rate limit, an
//     exhausted weekly or monthly allowance, a spent credit balance.
//     Restarting does not fix it, it has a reset time outside our control,
//     and it tends to hit every harness sharing the provider at once.
//   - auth: the credential is missing, wrong, expired, or not allowed. A
//     human has to fix configuration.
//   - timeout: the request was accepted and took too long.
//   - transport: the provider could not be reached, or could not serve the
//     request — refused and reset connections, DNS and TLS failures, 5xx
//     gateway errors, provider overload. Transient; nothing about our account
//     or configuration is wrong.
//   - other: everything else, including errors we recognise that fit none of
//     the above (a context-window rejection is the common one).
//
// Overload (Anthropic's 529 overloaded_error, a bare 503) is transport, not
// quota, on purpose. It is the provider's capacity, not our allowance: it has
// no reset time, it clears by itself, and filing it under quota would send an
// operator to check a budget that is fine. The alert that matters — quota
// climbing on every harness at once — must not fire for a busy afternoon.
//
// The tables are adapter-aware (design.md "Classifying a model error"): each
// adapter's own phrasings come first, then the provider shapes any adapter can
// relay, because crush, Claude Code and codex can all front the same Anthropic
// or OpenAI endpoint, directly or through litellm. Rule order is significant: a
// context-window rejection usually arrives as a 400 Bad Request, and a rate
// limit as "429 ... retry after a timeout", so the specific rule has to win.
//
// An error that matches nothing is class other AND unclassified. The
// unclassified counter is the classifier's control: when a provider rewords its
// errors, that counter rises instead of the classifier silently decaying into
// "everything is other" — which reads exactly like a healthy zero on the quota
// series.
//
// Governing: ADR-0020, SPEC-0013 REQ-3; design.md "Classifying a model error".
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"regexp"

	"github.com/stump-wtf/harness/internal/sessionguard"
)

// Class is a model-error class: the value of the `class` label.
type Class string

// The five classes SPEC-0013 REQ-3 allows.
const (
	ClassQuota     Class = "quota"
	ClassAuth      Class = "auth"
	ClassTimeout   Class = "timeout"
	ClassTransport Class = "transport"
	ClassOther     Class = "other"
)

// Classes is every class in exposition order. Every observable harness emits
// each one, zeros included, so an increase() over a class has a series to
// start from before its first error.
var Classes = []Class{ClassQuota, ClassAuth, ClassTimeout, ClassTransport, ClassOther}

// rule maps text matching re to class.
type rule struct {
	class Class
	re    *regexp.Regexp
}

func rx(class Class, pattern string) rule {
	// (?i): provider casing varies ("Rate limit", "RATE_LIMIT_EXCEEDED").
	return rule{class: class, re: regexp.MustCompile(`(?i)` + pattern)}
}

// commonRules are provider shapes any adapter can relay. Status codes match as
// whole numbers (\b), so "4290 tokens" is never a 429.
var commonRules = []rule{
	// quota — checked before timeout and transport: "429 ... please retry
	// after the timeout" is a rate limit, not a timeout.
	rx(ClassQuota, `\b429\b|too many requests|rate[ _-]?limit`),
	rx(ClassQuota, `insufficient[ _]quota|exceeded your current quota|quota[ _]exceeded|resource[ _]exhausted`),
	rx(ClassQuota, `usage limit|weekly limit|monthly limit|daily limit|spend(ing)? limit|budget exceeded`),
	rx(ClassQuota, `\b402\b|payment required|credit balance|insufficient (credits|balance|funds)|billing`),

	// auth — before transport, so a gateway's "403 Forbidden" is auth.
	rx(ClassAuth, `\b401\b|\b403\b|unauthori[sz]ed|forbidden`),
	rx(ClassAuth, `invalid[ _-]?(x-)?api[ _-]?key|incorrect api key|api key not valid|(no|missing) api key`),
	rx(ClassAuth, `authentication[ _]?(error|failed)|permission[ _]?(error|denied)|access denied|token (has )?expired|invalid[ _]token`),

	// timeout — accepted, then too slow.
	rx(ClassTimeout, `\b408\b|\b504\b|gateway time-?out|request time-?out|timed? ?out|deadline exceeded`),

	// transport — unreachable, or the provider could not serve. Overload is
	// here and not in quota; see the header.
	rx(ClassTransport, `connection (refused|reset|closed|aborted|error)|broken pipe|\beof\b|no such host|network is unreachable|dial tcp|server disconnected`),
	rx(ClassTransport, `\btls\b|x509|certificate|handshake`),
	rx(ClassTransport, `\b500\b|\b502\b|\b503\b|\b529\b|bad gateway|service unavailable|internal server error|overloaded`),
}

// adapterRules are each adapter's own phrasings, checked before commonRules.
var adapterRules = map[string][]rule{
	// Crush records a failed turn as a finish part: the message is the HTTP
	// status text, the details the provider body — often litellm's, whose
	// exception class names are unambiguous whatever the status line says.
	"crush": {
		rx(ClassQuota, `litellm\.(ratelimiterror|budgetexceedederror)`),
		rx(ClassAuth, `litellm\.(authenticationerror|permissiondeniederror)`),
		rx(ClassTimeout, `litellm\.timeout`),
		rx(ClassTransport, `litellm\.(apiconnectionerror|serviceunavailableerror|internalservererror)`),
	},
	// Claude Code prints its own summaries over the API error.
	"claude-code": {
		rx(ClassQuota, `usage limit reached|credit balance is too low`),
		rx(ClassAuth, `please run /login|oauth token (has )?expired`),
		rx(ClassTimeout, `request timed out`),
	},
	// Codex reports stream and retry failures in its own words.
	"codex": {
		rx(ClassQuota, `you've hit your usage limit|exceeded retry limit, last status: 429`),
		rx(ClassAuth, `unexpected status (401|403)`),
		rx(ClassTransport, `stream disconnected before completion|error sending request`),
	},
}

// Classify maps an agent error note to its class. known is false when nothing
// matched: the class is then ClassOther and the caller also counts the error
// as unclassified. A context-window rejection is recognised but is none of the
// four named situations, so it is ClassOther with known = true — it must never
// feed the unclassified control, or every wedged session would read as a
// provider changing its wording.
func Classify(adapter, note string) (class Class, known bool) {
	// Context-window first: it rides a 400 Bad Request the tables would
	// otherwise leave unmatched, and sharing the session guard's list means
	// the two can never disagree about what one looks like.
	if sessionguard.IsContextError(note) {
		return ClassOther, true
	}
	for _, rules := range [][]rule{adapterRules[adapter], commonRules} {
		for _, rl := range rules {
			if rl.re.MatchString(note) {
				return rl.class, true
			}
		}
	}
	return ClassOther, false
}
