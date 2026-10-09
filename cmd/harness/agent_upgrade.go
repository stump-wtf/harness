package main

// Agent Upgrade
//
// `harness agent upgrade` re-pins a package-sourced harness to a newer
// commit of the stable's local clone (SPEC-0026 REQ-8): resolve exactly as
// install does (never fetching), diff the candidate against the installed
// pin, rescan with the new-finding marks, review the effective-value diff,
// run install's gate, then update the @<sha> on each matching source line.
// A non-empty review never auto-applies: --yes and unattended runs refuse
// naming every change, and an interactive run chooses per row (issue #882).
// Only the rows the operator chose touch any other key on the table, and
// the prior pin's directory is never deleted — rollback is `harness agent
// install <stable>/<package>@<old-sha> --as <name> --replace`, answered
// from the retained pin with no network.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-8, REQ-5 (the
// rescan and its new-finding marks), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#814.
//
// @joestump-agent 10/04/2026 - The #882 review: a taken row removes the
// local override and writes nothing else, so the new pin supplies the value
// (a manifest path only resolves under its own pin directory).
//
// @joestump-agent 10/04/2026 - A kept package path says it holds the old
// pin: prune now counts it as a reference.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/agentpkg/scan"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config/tomledit"
	"github.com/stump-wtf/harness/internal/core"
)

type upgradeOpts struct {
	all         bool
	yes         bool
	forceUnsafe bool
	readmeFull  bool
}

func newAgentUpgradeCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "upgrade STABLE/PACKAGE[@VERSION]",
		Short:         "re-pin a package-sourced harness to a newer local-clone commit (diff, rescan, confirm; never fetches)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var o upgradeOpts
			o.all, _ = cmd.Flags().GetBool("all")
			o.yes, _ = cmd.Flags().GetBool("yes")
			o.forceUnsafe, _ = cmd.Flags().GetBool("force-unsafe")
			o.readmeFull, _ = cmd.Flags().GetBool("readme-full")
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			return runAgentUpgrade(cmd, g.opts(), o, ref)
		},
	}
	cmd.Flags().Bool("all", false, "upgrade every package-sourced harness in the global config")
	cmd.Flags().Bool("yes", false, "skip the ordinary confirmation (never clears a high finding or a write request)")
	cmd.Flags().Bool("force-unsafe", false, "override a high-severity finding (requires an interactive retype)")
	cmd.Flags().Bool("readme-full", false, "print the package README whole instead of capping it")
	return cmd
}

// upgradeTarget is one package with the harnesses sourcing it.
type upgradeTarget struct {
	src   agentpkg.Source
	names []string
}

// runAgentUpgrade collects the package-sourced harnesses the request names
// (or all of them under --all), then upgrades each package: resolve, diff,
// rescan, gate, re-pin.
func runAgentUpgrade(cmd *cobra.Command, o verbOpts, uo upgradeOpts, ref string) error {
	if uo.all && ref != "" {
		return errors.New("agent: --all and a package reference are mutually exclusive")
	}
	if !uo.all && ref == "" {
		return errors.New("agent: upgrade needs a <stable>/<package>[@<version>] or --all")
	}
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}

	var version, wantStable, wantPkg string
	if !uo.all {
		var err error
		wantStable, wantPkg, version, err = splitRef(ref)
		if err != nil {
			return err
		}
	}
	targets := map[string]*upgradeTarget{}
	var order []string
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.PackageSource == "" {
			continue
		}
		src, err := agentpkg.ParseSource(h.PackageSource)
		if err != nil {
			continue
		}
		if !uo.all && (src.Stable != wantStable || src.Package != wantPkg) {
			continue
		}
		key := src.Stable + "/" + src.Package
		if targets[key] == nil {
			targets[key] = &upgradeTarget{src: agentpkg.Source{Stable: src.Stable, Package: src.Package}}
			order = append(order, key)
		}
		targets[key].names = append(targets[key].names, name)
	}
	if len(order) == 0 {
		if uo.all {
			fmt.Fprintln(cmd.OutOrStdout(), "agent: no package-sourced harnesses in the global config; nothing to upgrade")
			return nil
		}
		return fmt.Errorf("agent: no harness in the global config is installed from %q", ref)
	}

	var firstErr error
	for _, key := range order {
		target := targets[key]
		if err := upgradeOne(cmd, o, uo, cfg, target, version); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "agent: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	reloadDaemonIfAny(cmd, o)
	return firstErr
}

// upgradeOne runs the REQ-8 sequence for one package: resolve, diff,
// rescan with new-finding marks, gate, re-pin every harness table.
func upgradeOne(cmd *cobra.Command, o verbOpts, uo upgradeOpts, cfg *core.Config, target *upgradeTarget, version string) error {
	stable, pkg := target.src.Stable, target.src.Package
	if _, exists := cfg.Stables[stable]; !exists {
		return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, stable)
	}

	newSrc, err := agentpkg.ResolvePin(stable, pkg, version)
	if err != nil {
		return fmt.Errorf("agent: version %q is not present in stable %q's local clone — run `harness agent stable update %s` first (no fetch is performed on your behalf): %w",
			version, stable, stable, err)
	}

	// Group the harnesses by their current pin: an upgrade shows one diff
	// per distinct installed commit.
	groups := map[string][]string{}
	for _, name := range target.names {
		src, _ := agentpkg.ParseSource(cfg.Harnesses[name].PackageSource)
		groups[src.SHA] = append(groups[src.SHA], name)
	}

	for oldSHA, names := range groups {
		oldSrc := agentpkg.Source{Stable: stable, Package: pkg, SHA: oldSHA}
		if oldSHA == newSrc.SHA {
			for _, name := range names {
				fmt.Fprintf(cmd.OutOrStdout(), "agent: %s: already up to date (@%s)\n", name, oldSHA)
			}
			continue
		}

		newDir := agentpkg.PinDir(newSrc)
		var tmp string
		if _, err := os.Stat(newDir); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("agent: stat %s: %w", newDir, err)
			}
			tmp, err = agentpkg.Materialize(newSrc)
			if err != nil {
				return err
			}
			newDir = tmp
			defer agentpkg.Discard(tmp)
		}

		oldDir := agentpkg.PinDir(oldSrc)
		oldMan, err := agentpkg.LoadManifest(filepath.Join(oldDir, "package.toml"))
		if err != nil {
			return fmt.Errorf("agent: installed pin %s: %w", oldSrc, err)
		}
		newMan, err := agentpkg.LoadManifest(filepath.Join(newDir, "package.toml"))
		if err != nil {
			return fmt.Errorf("agent: candidate pin %s: %w", newSrc, err)
		}

		oldFindings, err := scan.Scan(oldDir, oldMan)
		if err != nil {
			return err
		}
		newFindingsAll, err := scan.Scan(newDir, newMan)
		if err != nil {
			return err
		}
		newFindings := agentpkg.NewSince(oldFindings, newFindingsAll)

		diffLines, err := agentpkg.PinDiff(oldDir, newDir, oldMan, newMan)
		if err != nil {
			return err
		}
		if len(diffLines) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "diff since @%s:\n", oldSHA)
			for _, l := range diffLines {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", l)
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "diff since @%s: no content changes\n", oldSHA)
		}

		manifestRaw, err := os.ReadFile(filepath.Join(newDir, "package.toml"))
		if err != nil {
			return err
		}
		files, err := agentpkg.BundledFilesIn(newDir)
		if err != nil {
			return err
		}
		// The candidate's README. A change since the installed pin already
		// showed as a README.md hunk in the diff above (harness#929).
		readme, err := agentpkg.LoadReadme(newDir, tmp == "")
		if err != nil {
			return err
		}
		agentpkg.RenderReport(cmd.OutOrStdout(), agentpkg.ReportInput{
			Ref:          stable + "/" + pkg,
			ManifestRaw:  manifestRaw,
			Man:          newMan,
			Findings:     newFindingsAll,
			NewFindings:  newFindings,
			BundledFiles: files,
			Readme:       readme,
			ReadmeFull:   uo.readmeFull,
		})
		if hasHigh(newFindingsAll) {
			agentpkg.LogBlocked(stable+"/"+pkg, newFindingsAll)
		}

		// The effective-value review (issue #882): a diff the operator may
		// want to see never auto-applies. --yes and unattended runs refuse
		// loudly; an interactive run chooses per row.
		choices := map[string]reviewChoice{}
		for _, name := range names {
			h := cfg.Harnesses[name]
			chgs := agentpkg.EffectiveChanges(&h, oldMan, newMan)
			if len(chgs) == 0 {
				continue
			}
			interactive := cliui.IsTTY(os.Stdin)
			if uo.yes || !interactive {
				return reviewRefusal(name, chgs)
			}
			take, err := chooseReview(cmd, name, chgs)
			if err != nil {
				return err
			}
			choices[name] = reviewChoice{chgs: chgs, take: take}
		}

		gate := agentpkg.DecisionInput{
			Findings:    newFindingsAll,
			Requests:    newMan.Requests,
			Yes:         uo.yes,
			ForceUnsafe: uo.forceUnsafe,
			Interactive: cliui.IsTTY(os.Stdin),
			Ref:         stable + "/" + pkg,
		}
		if err := runGate(cmd, gate); err != nil {
			return err
		}

		// Place the new pin; the prior pin's directory is never deleted.
		if tmp != "" {
			if _, err := agentpkg.Place(newSrc, tmp); err != nil {
				return err
			}
		}

		// Update the @<sha> on each source line (REQ-8), plus the review
		// choices: kept package values pin onto the table, and a taken row
		// over a local override removes it so the new pin's value applies.
		ed, err := loadGlobalEditor(o.configPath)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := ed.SetHarnessKey(name, "source", newSrc.String()); err != nil {
				return fmt.Errorf("agent: update source on [harness.%s]: %w", name, err)
			}
			if rc, ok := choices[name]; ok {
				if err := applyReviewChoices(cmd, ed, name, rc.chgs, rc.take); err != nil {
					return err
				}
			}
		}
		if err := ed.Save(o.configPath); err != nil {
			return fmt.Errorf("agent: write %s: %w", o.configPath, err)
		}

		if hasHigh(newFindingsAll) && uo.forceUnsafe {
			rec := agentpkg.InstallRecord{
				Source:                    newSrc.String(),
				ScannerVersion:            scan.TableVersion,
				Findings:                  newFindingsAll,
				OverriddenWithForceUnsafe: true,
			}
			if err := agentpkg.WriteInstallRecord(newSrc, rec); err != nil {
				return err
			}
		}

		for _, name := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "agent: upgraded %s: @%s -> @%s (prior pin retained at %s)\n",
				name, oldSHA, newSrc.SHA, oldDir)
		}
	}
	return nil
}

// reviewRefusal is the loud bomb-out for a reviewable diff under --yes or
// without a terminal (issue #882): auto-accepting a conflict is exactly the
// danger the review exists for.
func reviewRefusal(name string, chgs []agentpkg.EffectiveChange) error {
	var rows []string
	for _, c := range chgs {
		rows = append(rows, c.Key+": "+c.Render(c.Old)+" -> "+c.Render(c.New))
	}
	return fmt.Errorf("agent: harness %q: the upgrade changes values you may want to review (%s) — rerun without --yes on a terminal to choose per change", name, strings.Join(rows, "; "))
}

// chooseReview shows the numbered rows and reads one answer: Enter keeps
// every current value, "all" takes every new one, and a comma list takes
// those rows. Rows marked added (the new pin introduces the key) always
// arrive with the upgrade.
func chooseReview(cmd *cobra.Command, name string, chgs []agentpkg.EffectiveChange) (map[int]bool, error) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "agent: harness %s: choose what the upgrade changes (Enter keeps current values, \"all\" takes every new value, or a comma list to take, e.g. \"1,2\"):\n", name)
	for i, c := range chgs {
		note := ""
		switch {
		case c.Added:
			note = "  (new with this version)"
		case c.OldLocal:
			note = "  (your local override)"
		}
		fmt.Fprintf(out, "  %d) %s: %s -> %s%s\n", i+1, c.Key, c.Render(c.Old), c.Render(c.New), note)
	}
	fmt.Fprintf(out, "choice: ")
	line, err := readLine()
	if err != nil {
		return nil, fmt.Errorf("agent: read review choice: %w", err)
	}
	return parseReviewChoice(strings.TrimSpace(line), len(chgs))
}

// parseReviewChoice maps the answer to a take-set: true = take the row's
// new value.
func parseReviewChoice(answer string, n int) (map[int]bool, error) {
	take := make(map[int]bool)
	switch answer {
	case "":
		return take, nil
	case "all":
		for i := 0; i < n; i++ {
			take[i] = true
		}
		return take, nil
	}
	for _, part := range strings.Split(answer, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var i int
		if _, err := fmt.Sscanf(part, "%d", &i); err != nil || i < 1 || i > n {
			return nil, fmt.Errorf("agent: review choice %q is not a row number, \"all\", or empty", part)
		}
		take[i-1] = true
	}
	return take, nil
}

// reviewChoice is one harness's reviewed rows and the operator's answer.
type reviewChoice struct {
	chgs []agentpkg.EffectiveChange
	take map[int]bool
}

// applyReviewChoices writes one harness's choices to the editor. Keeping a
// package-supplied value pins it onto the table as an explicit override.
// Taking a new value removes any local key standing in its way and writes
// nothing else: the new pin supplies the value, and a manifest path only
// resolves under its own pin directory. Added rows always land.
func applyReviewChoices(cmd *cobra.Command, ed *tomledit.Editor, name string, chgs []agentpkg.EffectiveChange, take map[int]bool) error {
	out := cmd.OutOrStdout()
	for i, c := range chgs {
		taken := take[i] || c.Added
		switch c.Kind {
		case "request":
			if taken {
				if err := ed.SetHarnessKey(name, "mcp_allow", c.New); err != nil {
					return fmt.Errorf("agent: grant mcp_allow on [harness.%s]: %w", name, err)
				}
				fmt.Fprintf(out, "agent: %s: granted mcp_allow %s\n", name, c.Render(c.New))
			} else {
				fmt.Fprintf(out, "agent: %s: declined mcp_allow %s; the request stays unmet\n", name, c.Render(c.New))
			}
		default:
			switch {
			case taken:
				err := ed.RemoveHarnessKey(name, c.Key)
				switch {
				case err == nil:
					fmt.Fprintf(out, "agent: %s: took the package's %s (local override removed)\n", name, c.Key)
				case !errors.Is(err, tomledit.ErrKeyNotFound):
					return fmt.Errorf("agent: drop local %q on [harness.%s]: %w", c.Key, name, err)
				case c.New == nil:
					fmt.Fprintf(out, "agent: %s: %s gone with the old pin\n", name, c.Key)
				default:
					fmt.Fprintf(out, "agent: %s: took the package's %s\n", name, c.Key)
				}
			case c.OldLocal:
				// Kept a local override: it already wins over the pin.
				fmt.Fprintf(out, "agent: %s: kept your %s = %s\n", name, c.Key, c.Render(c.Old))
			case c.Old != nil:
				if err := ed.SetHarnessKey(name, c.Key, c.Old); err != nil {
					return fmt.Errorf("agent: pin %q on [harness.%s]: %w", c.Key, name, err)
				}
				// A kept package path is the old pin's absolute path: it
				// now holds that pin, and prune keeps it while it does.
				held := ""
				if p, ok := c.Old.(string); ok {
					if src, ok := agentpkg.PinOf(p); ok {
						held = fmt.Sprintf("; prune keeps @%s while this path points into it", src.SHA)
					}
				}
				fmt.Fprintf(out, "agent: %s: kept %s = %s (pinned onto the table%s)\n", name, c.Key, c.Render(c.Old), held)
			default:
				// Kept a row with no old value: only the Added shape reaches
				// here, and Added rows are always taken.
			}
		}
	}
	return nil
}
