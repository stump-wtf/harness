package main

// Install-time Defaults
//
// The engine that turns a package's [defaults] suggestions into the
// operator's own keys (SPEC-0026 REQ-6, REQ-8; ADR-0044 amendment of
// 2026-10-09). A suggestion is inert everywhere else: config load never
// applies one, and nothing here writes a key the operator did not choose.
//
// Every key is decided in DefaultsKeyOrder order, so two runs of the same
// manifest ask the same questions in the same order. A key the target table
// already sets is never offered and never moved — reading the raw TOML is
// the only honest way to know, since the loaded config cannot tell an
// explicit zero from an omitted key. A suggested triggers value is accepted
// only when every source it names is declared, because install never
// creates a trigger source. And before the write, the table the write would
// produce is run through the daemon's own loader: a suggestion that would
// break the file (a schedule beside the operator's enabled = true, a timeout
// with nothing to time out) is skipped naming the loader's error.
//
// Governing: ADR-0044, SPEC-0026 REQ-6, REQ-8, Error Handling Standards.
//
// @joestump-agent 10/10/2026 - Added for harness#931.

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/config/tomledit"
	"github.com/stump-wtf/harness/internal/core"
)

// The outcome of one suggested key, as the install and upgrade output
// states it (REQ-6: the output lists every default written and every one
// skipped, with the reason).
const (
	// defaultWritten: the operator chose it, and it lands on the table as
	// their own key in the same write as source.
	defaultWritten = "written"
	// defaultSkipped: not written, for the named reason.
	defaultSkipped = "skipped"
	// defaultKeptYours: the table already sets the key, so it was never
	// offered and its value is untouched (REQ-8).
	defaultKeptYours = "kept"
)

// defaultOutcome is one suggested key's decision.
type defaultOutcome struct {
	Key    string
	Value  any
	Status string
	Reason string
}

// render spells the outcome for the output line.
func (d defaultOutcome) render() string {
	line := fmt.Sprintf("%s = %s: %s", d.Key, agentpkg.RenderDefault(d.Key, d.Value), d.Status)
	if d.Reason != "" {
		line += " (" + d.Reason + ")"
	}
	return line
}

// defaultsAsk is everything the engine needs to decide the suggestions for
// one harness table.
type defaultsAsk struct {
	// ConfigPath is the file the write lands in: the table the offers are
	// made against and the one the loader check copies.
	ConfigPath string
	// Table is the [harness.<name>] table the write targets.
	Table string
	// Man is the manifest whose [defaults] are being decided.
	Man *agentpkg.Manifest
	// Cfg is the loaded config, consulted only for whether a suggested
	// trigger source exists.
	Cfg *core.Config
	// Only limits the offered keys to the named ones; empty offers every
	// suggested key. Upgrade passes the added and changed suggestions,
	// because one the new pin leaves unchanged is not asked about twice
	// (REQ-8).
	Only []string
	// Candidate applies the changes the write is itself about to make
	// (its source line, its mcp_allow grant, any review choices) to an
	// editor over a copy of the file, creating the table when it is absent.
	// The loader check runs against that table plus each accepted default.
	Candidate func(ed *tomledit.Editor) error
	// Yes consents to the trust review and to nothing else: it accepts no
	// suggestion and says so (ADR-0044 amendment of 2026-10-09).
	Yes bool
	// AcceptDefaults accepts every suggestion without prompting, which
	// counts as the operator's choice even unattended.
	AcceptDefaults bool
	// Interactive reports whether a terminal is present to prompt in.
	Interactive bool
}

// decideDefaults returns one outcome per offered key, in REQ-3 key order.
// It writes nothing: the caller writes the written ones, in the same edit
// as the source line.
func decideDefaults(cmd *cobra.Command, ask defaultsAsk) ([]defaultOutcome, error) {
	keys := ask.Man.Defaults.Ordered()
	if len(keys) == 0 {
		return nil, nil
	}
	set, err := tableKeyValues(ask.ConfigPath, ask.Table)
	if err != nil {
		return nil, err
	}
	// One reader for the whole run: readLine buffers per call, so a second
	// prompt would otherwise discard whatever the first one's reader slurped
	// past its own line.
	in := bufio.NewReader(os.Stdin)
	var outcomes []defaultOutcome
	var chosen [][2]any
	for _, key := range keys {
		if len(ask.Only) > 0 && !slices.Contains(ask.Only, key) {
			continue
		}
		value, _ := ask.Man.Defaults.Get(key)
		// A kept line states the operator's own value, not the suggestion:
		// the suggestion lost, and echoing it would read as if it landed.
		if yours, ok := set[key]; ok {
			outcomes = append(outcomes, defaultOutcome{key, yours, defaultKeptYours, "your table already sets it"})
			continue
		}
		if key == "triggers" {
			if missing := missingTriggerSources(ask.Cfg, value); len(missing) > 0 {
				outcomes = append(outcomes, defaultOutcome{key, value, defaultSkipped,
					"no source named " + strings.Join(missing, ", ") + " is declared, and install never creates one"})
				continue
			}
		}
		pick := value
		switch {
		case ask.AcceptDefaults:
		case ask.Yes || !ask.Interactive:
			outcomes = append(outcomes, defaultOutcome{key, value, defaultSkipped,
				"--yes accepts no suggestions; --accept-defaults would have accepted it"})
			continue
		default:
			accepted, override, err := promptDefault(cmd, in, key, value)
			if err != nil {
				return nil, err
			}
			if !accepted {
				outcomes = append(outcomes, defaultOutcome{key, value, defaultSkipped, "you declined it"})
				continue
			}
			pick = override
		}
		next := append(slices.Clone(chosen), [2]any{key, pick})
		if err := checkCandidate(ask, next); err != nil {
			outcomes = append(outcomes, defaultOutcome{key, pick, defaultSkipped,
				fmt.Sprintf("the config loader refused it: %v", err)})
			continue
		}
		chosen = next
		outcomes = append(outcomes, defaultOutcome{key, pick, defaultWritten, ""})
	}
	return outcomes, nil
}

// printDefaults states the decisions in the output (REQ-6, REQ-8), after
// the write that carried them.
func printDefaults(cmd *cobra.Command, prefix string, outcomes []defaultOutcome) {
	if len(outcomes) == 0 {
		return
	}
	for _, d := range outcomes {
		fmt.Fprintf(cmd.OutOrStdout(), "agent: %sdefault %s\n", prefix, d.render())
	}
}

// promptDefault offers one suggestion with REQ-6's three choices: accept,
// overwrite, or skip. An overridden value is validated by the same parser
// REQ-3 applies and, when invalid, is reported and asked for again — never
// written. An unreadable prompt is a cancel, not a silent skip. The reader
// is the run's, shared across every prompt.
func promptDefault(cmd *cobra.Command, in *bufio.Reader, key string, value any) (bool, any, error) {
	out := cmd.OutOrStdout()
	for {
		fmt.Fprintf(out, "agent: [defaults] suggests %s = %s\n", key, agentpkg.RenderDefault(key, value))
		fmt.Fprint(out, "agent:   [a]ccept, [o]verwrite or [s]kip? [a]: ")
		typed, err := readLineFrom(in)
		if err != nil {
			return false, nil, fmt.Errorf("agent: default %s prompt: %w", key, err)
		}
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "", "a", "accept", "y", "yes":
			return true, value, nil
		case "s", "skip", "n", "no":
			return false, nil, nil
		case "o", "overwrite", "override":
			fmt.Fprint(out, "agent:   new value (Enter keeps the suggestion): ")
			raw, err := readLineFrom(in)
			if err != nil {
				return false, nil, fmt.Errorf("agent: default %s value prompt: %w", key, err)
			}
			if strings.TrimSpace(raw) == "" {
				return true, value, nil
			}
			parsed, err := agentpkg.NormalizeDefault(key, raw)
			if err != nil {
				fmt.Fprintf(out, "agent:   %v — try again\n", err)
				continue
			}
			return true, parsed, nil
		default:
			fmt.Fprintln(out, "agent:   answer a, o or s")
		}
	}
}

// missingTriggerSources names the sources a suggested triggers value
// references that the config does not declare. Install never creates one,
// so a suggestion naming a missing source is skipped rather than accepted
// (REQ-6).
func missingTriggerSources(cfg *core.Config, value any) []string {
	var missing []string
	for _, ref := range agentpkg.TriggerRefs(value) {
		r, err := core.ParseTriggerRef(ref)
		if err != nil {
			missing = append(missing, ref)
			continue
		}
		switch r.Kind {
		case core.SourceKindChannel:
			if _, ok := cfg.Channels[r.Name]; !ok {
				missing = append(missing, ref)
			}
		case core.SourceKindWebhook:
			if _, ok := cfg.Webhooks[r.Name]; !ok {
				missing = append(missing, ref)
			}
		default:
			missing = append(missing, ref)
		}
	}
	return missing
}

// tableKeyValues lists the keys and values [harness.<name>] already sets in
// the file at path. A file with no such table sets nothing; the raw TOML is
// consulted because the decoded config cannot tell an explicit zero from an
// omitted key, and offering a set key would invite the operator to move a
// value REQ-8 says is theirs.
func tableKeyValues(path, name string) (map[string]any, error) {
	out := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("agent: read %s: %w", path, err)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		return nil, fmt.Errorf("agent: parse %s: %w", path, err)
	}
	tables, ok := doc["harness"].(map[string]any)
	if !ok {
		return out, nil
	}
	table, ok := tables[name].(map[string]any)
	if !ok {
		return out, nil
	}
	for key, value := range table {
		out[key] = value
	}
	return out, nil
}

// checkCandidate loads the table this write would produce, resolving every
// source from the content-addressed store, because the defaults are decided
// after the pin is placed. Its error is exactly the error REQ-6 says to name when
// a suggestion is skipped, so the operator reads why rather than a
// second-guessed rule.
func checkCandidate(ask defaultsAsk, keys [][2]any) error {
	return loadCandidate(ask.ConfigPath, func(ed *tomledit.Editor) error {
		if ask.Candidate != nil {
			if err := ask.Candidate(ed); err != nil {
				return err
			}
		}
		for _, kv := range keys {
			if err := ed.SetHarnessKey(ask.Table, kv[0].(string), kv[1]); err != nil {
				return fmt.Errorf("agent: stage %s on [harness.%s]: %w", kv[0], ask.Table, err)
			}
		}
		return nil
	}, nil)
}

// loadCandidate parses the config one write would produce: a copy of the
// file at path carrying apply's changes, run through the daemon's own
// loader, so the merged result is judged by the rules a hand-written table
// meets (SPEC-0026 REQ-3, REQ-7). resolve says where a `source` resolves
// from instead of the pin store; nil is the store. The error is the
// loader's own: the file, line, harness and offending key.
func loadCandidate(path string, apply func(ed *tomledit.Editor) error, resolve func(agentpkg.Source) string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agent: read %s: %w", path, err)
	}
	ed := tomledit.New(raw)
	if apply != nil {
		if err := apply(ed); err != nil {
			return err
		}
	}
	if _, err := config.ParseWith(ed.Bytes(), path, config.ParseOptions{PinDir: resolve}); err != nil {
		return err
	}
	return nil
}
