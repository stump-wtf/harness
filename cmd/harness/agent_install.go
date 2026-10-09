package main

// Agent Install, Uninstall And Prune
//
// The commands that change what a harness runs (SPEC-0026 REQ-6, REQ-9,
// REQ-11). install resolves a pin in the stable's local clone — never
// fetching — materializes the package by reading git objects, runs the
// #812 scan and confirmation gate, places the pin immutably, and writes
// source onto one [harness.<name>] table through the #810 editor. uninstall
// removes a whole table from whichever file declares it. prune drops store
// entries the global file does not reference — by source, or by a prompt or
// MCP file path pointing inside the pin — and says so.
//
// No command here writes any table but [harness.*]: [mcp.*], [job.*],
// [server], [profile.*], [adapter.*] and [skill_repo.*] are never touched
// (REQ-11), and a bundled prompts/ directory is named, never registered.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-6, REQ-9,
// REQ-11, Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#813.
//
// @joestump-agent 10/04/2026 - prune keeps a pin a global file path points
// into: a package path kept through the #882 review is the old pin's
// absolute path, and pruning that pin broke the next config load.

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
	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/config/tomledit"
	"github.com/stump-wtf/harness/internal/core"
)

type installOpts struct {
	as          string
	replace     bool
	yes         bool
	forceUnsafe bool
	readmeFull  bool
}

func newAgentInstallCmd(g *globalOpts) *cobra.Command {
	install := &cobra.Command{
		Use:           "install STABLE/PACKAGE[@VERSION]",
		Short:         "pin a package and bind it to a harness table (scan + confirm; never fetches)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var o installOpts
			o.as, _ = cmd.Flags().GetString("as")
			o.replace, _ = cmd.Flags().GetBool("replace")
			o.yes, _ = cmd.Flags().GetBool("yes")
			o.forceUnsafe, _ = cmd.Flags().GetBool("force-unsafe")
			o.readmeFull, _ = cmd.Flags().GetBool("readme-full")
			return runAgentInstall(cmd, g.opts(), o, args[0])
		},
	}
	install.Flags().String("as", "", "harness table name (defaults to the package name)")
	install.Flags().Bool("replace", false, "allow replacing an existing source from a different package")
	install.Flags().Bool("yes", false, "skip the ordinary confirmation (never clears a high finding or a write request)")
	install.Flags().Bool("force-unsafe", false, "override a high-severity finding (requires an interactive retype)")
	install.Flags().Bool("readme-full", false, "print the package README whole instead of capping it")
	return install
}

func newAgentUninstallCmd(g *globalOpts) *cobra.Command {

	uninstall := &cobra.Command{
		Use:           "uninstall NAME",
		Short:         "remove a package-installed harness table (the pin stays until prune)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			return runAgentUninstall(cmd, g.opts(), args[0], yes)
		},
	}
	uninstall.Flags().Bool("yes", false, "skip the removal confirmation")
	return uninstall
}

func newAgentPruneCmd(g *globalOpts) *cobra.Command {

	prune := &cobra.Command{
		Use:           "prune",
		Short:         "remove store pins the global harness.toml no longer references (by source or file path)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentPrune(cmd, g.opts())
		},
	}
	return prune
}

// runAgentInstall is REQ-6 end to end: resolve, materialize, scan, confirm,
// place, write the table. The clone is consulted read-only (git archive),
// a pin already in the store is used without opening the clone, and an
// existing pin directory is never rewritten.
func runAgentInstall(cmd *cobra.Command, o verbOpts, io installOpts, ref string) error {
	stable, pkg, version, err := splitRef(ref)
	if err != nil {
		return err
	}
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	if _, exists := cfg.Stables[stable]; !exists {
		return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, stable)
	}

	src, err := agentpkg.ResolvePin(stable, pkg, version)
	if err != nil {
		return err
	}

	// The scan source: an existing pin answers from the store; a new pin
	// materializes to a temp directory first and is placed only after the
	// gate passes.
	srcDir := agentpkg.PinDir(src)
	var tmp string
	if _, err := os.Stat(srcDir); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("agent: stat %s: %w", srcDir, err)
		}
		tmp, err = agentpkg.Materialize(src)
		if err != nil {
			return err
		}
		srcDir = tmp
	}
	if tmp != "" {
		// A refused install never leaves a materialization behind.
		defer agentpkg.Discard(tmp)
	}

	man, err := agentpkg.LoadManifest(filepath.Join(srcDir, "package.toml"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: package %q has no package.toml in stable %q", agentpkg.ErrUnknownPackage, pkg, stable)
		}
		return err
	}
	files, err := agentpkg.BundledFilesIn(srcDir)
	if err != nil {
		return err
	}
	findings, err := scan.Scan(srcDir, man)
	if err != nil {
		return err
	}

	manifestRaw, err := os.ReadFile(filepath.Join(srcDir, "package.toml"))
	if err != nil {
		return err
	}
	// A temp materialization is discarded, so only a stored pin can be
	// named as where the full README lives (harness#929).
	readme, err := agentpkg.LoadReadme(srcDir, tmp == "")
	if err != nil {
		return err
	}
	agentpkg.RenderReport(cmd.OutOrStdout(), agentpkg.ReportInput{
		Ref:          src.Stable + "/" + src.Package,
		ManifestRaw:  manifestRaw,
		Man:          man,
		Findings:     findings,
		BundledFiles: files,
		Readme:       readme,
		ReadmeFull:   io.readmeFull,
	})
	if hasHigh(findings) {
		agentpkg.LogBlocked(src.Stable+"/"+src.Package, findings)
	}

	gate := agentpkg.DecisionInput{
		Findings:    findings,
		Requests:    man.Requests,
		Yes:         io.yes,
		ForceUnsafe: io.forceUnsafe,
		Interactive: cliui.IsTTY(os.Stdin),
		Ref:         src.Stable + "/" + src.Package,
	}
	if err := runGate(cmd, gate); err != nil {
		return err
	}

	// Place the pin: adopt the materialization, or reuse what is there.
	if tmp != "" {
		if _, err := agentpkg.Place(src, tmp); err != nil {
			return err
		}
	}

	// Write exactly one key on one table (REQ-11): source.
	name := io.as
	if name == "" {
		name = pkg
	}
	// The confirmed requested scope lands on the table with the source
	// (issue #882): the effective mcp_allow is visible and hand-editable,
	// one place, never split between table and manifest. A package that
	// declares no scopes writes nothing — the default grant is the default.
	var extra [][2]any
	if len(man.Requests.MCPAllow) > 0 {
		extra = append(extra, [2]any{"mcp_allow", man.Requests.MCPAllow})
	}
	if err := writeSourceTable(o.configPath, cfg, name, src, io.replace, extra); err != nil {
		return err
	}

	// Retain an overridden finding in the install record (REQ-5).
	if hasHigh(findings) && io.forceUnsafe {
		rec := agentpkg.InstallRecord{
			Source:                    src.String(),
			ScannerVersion:            scan.TableVersion,
			Findings:                  findings,
			OverriddenWithForceUnsafe: true,
		}
		if err := agentpkg.WriteInstallRecord(src, rec); err != nil {
			return err
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "agent: installed %s as harness %q (source = %q, pinned at %s)\n",
		src.Stable+"/"+src.Package, name, src.String(), agentpkg.PinDir(src))
	for _, f := range files {
		if strings.HasPrefix(f, "prompts/") {
			fmt.Fprintf(cmd.OutOrStdout(), "agent: bundled prompts/ directory is NOT registered automatically; add it to [mcp.prompts] by hand if you want it\n")
			break
		}
	}
	reloadDaemonIfAny(cmd, o)
	return nil
}

// writeSourceTable writes or updates [harness.<name>] carrying source plus
// any extra confirmed keys, preserving every other key. Overwriting a table
// whose source names a different stable or package fails naming it unless
// replace is given (REQ-6).
func writeSourceTable(path string, cfg *core.Config, name string, src agentpkg.Source, replace bool, extra [][2]any) error {
	if existing, ok := cfg.Harnesses[name]; ok && existing.PackageSource != "" {
		if es, err := agentpkg.ParseSource(existing.PackageSource); err == nil {
			if es.Stable != src.Stable || es.Package != src.Package {
				if !replace {
					return fmt.Errorf("agent: harness %q is already installed from %q; rerun with --replace to change it", name, existing.PackageSource)
				}
			}
		}
	}
	ed, err := loadGlobalEditor(path)
	if err != nil {
		return err
	}
	if _, ok := cfg.Harnesses[name]; ok {
		if err := ed.SetHarnessKey(name, "source", src.String()); err != nil {
			return fmt.Errorf("agent: set source on [harness.%s]: %w", name, err)
		}
		for _, kv := range extra {
			if err := ed.SetHarnessKey(name, kv[0].(string), kv[1]); err != nil {
				return fmt.Errorf("agent: set %s on [harness.%s]: %w", kv[0], name, err)
			}
		}
	} else {
		keys := append([][2]any{{"source", src.String()}}, extra...)
		if err := ed.AddHarness(name, keys); err != nil {
			return fmt.Errorf("agent: add [harness.%s]: %w", name, err)
		}
	}
	if err := ed.Save(path); err != nil {
		return fmt.Errorf("agent: write %s: %w", path, err)
	}
	return nil
}

// runAgentUninstall removes the whole [harness.<name>] table from the file
// that declares it — the global file, or the project file when the harness
// is named there (REQ-9). A harness without a source is refused: it was
// never installed from a package. The pin directory stays; prune removes
// it.
func runAgentUninstall(cmd *cobra.Command, o verbOpts, name string, yes bool) error {
	// The global file first.
	cfg, cfgErr := config.Load(o.configPath)
	if cfgErr == nil {
		if h, ok := cfg.Harnesses[name]; ok {
			if h.PackageSource == "" {
				return fmt.Errorf("agent: harness %q was not installed from a package (no source key); edit the table by hand", name)
			}
			if err := confirmRemoval(cmd, name, yes); err != nil {
				return err
			}
			return removeHarnessTable(cmd, o, o.configPath, name, true)
		}
	}

	// Then the project file the harness is named in, if one is discovered.
	proj, err := config.DiscoverProjectExcluding(o.configPath)
	if err == nil && proj != nil && proj.Config != nil {
		if h, ok := proj.Config.Harnesses[name]; ok {
			if h.PackageSource == "" {
				return fmt.Errorf("agent: harness %q was not installed from a package (no source key); edit the table by hand", name)
			}
			if err := confirmRemoval(cmd, name, yes); err != nil {
				return err
			}
			return removeHarnessTable(cmd, o, proj.ConfigPath, name, false)
		}
	}

	if cfgErr != nil && !errors.Is(cfgErr, fs.ErrNotExist) {
		return fmt.Errorf("agent: load config: %w", cfgErr)
	}
	return fmt.Errorf("agent: harness %q is not declared in the global config or a discovered project file", name)
}

func confirmRemoval(cmd *cobra.Command, name string, yes bool) error {
	if yes {
		return nil
	}
	if !cliui.IsTTY(os.Stdin) {
		return fmt.Errorf("agent: removing harness %q needs confirmation; rerun with --yes", name)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "agent: remove the [harness.%s] table? [y/N]: ", name)
	typed, err := readLine()
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(typed)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("agent: removal of %q cancelled", name)
	}
}

func removeHarnessTable(cmd *cobra.Command, o verbOpts, path, name string, global bool) error {
	ed, err := tomledit.Load(path)
	if err != nil {
		return fmt.Errorf("agent: load %s: %w", path, err)
	}
	if err := ed.RemoveHarness(name); err != nil {
		return fmt.Errorf("agent: remove [harness.%s]: %w", name, err)
	}
	if err := ed.Save(path); err != nil {
		return fmt.Errorf("agent: write %s: %w", path, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "agent: removed the [harness.%s] table from %s\n", name, path)
	fmt.Fprintf(cmd.OutOrStdout(), "agent: the pin directory stays; `harness agent prune` removes unreferenced pins\n")
	if global {
		reloadDaemonIfAny(cmd, o)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "agent: a running project harness keeps its registration until `harness rm %q`\n", name)
	}
	return nil
}

// runAgentPrune removes every store entry the GLOBAL harness.toml does not
// reference, and states that limit (REQ-9): project files are never read,
// so a pin only a project references is removed. A reference is a source,
// or a file path config load reads that points inside a pin; prune names
// each pin only such a path holds.
func runAgentPrune(cmd *cobra.Command, o verbOpts) error {
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	out := cmd.OutOrStdout()
	referenced := map[agentpkg.Source]bool{}
	for _, name := range cfg.HarnessOrder {
		srcStr := cfg.Harnesses[name].PackageSource
		if srcStr == "" {
			continue
		}
		if src, err := agentpkg.ParseSource(srcStr); err == nil {
			referenced[src] = true
		}
	}

	// A package path kept through the upgrade review is the OLD pin's
	// absolute path, and config load fails without the file it names.
	// Collected after every source, so only a pin no source names is
	// reported as held by a path.
	type pathRef struct {
		src       agentpkg.Source
		name, key string
	}
	var held []pathRef
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		for _, f := range []struct{ key, path string }{
			{"prompt_file", h.PromptFile},
			{"prompt_template_file", h.PromptTemplateFile},
			{"system_prompt_file", h.SystemPromptFile},
			{"mcp_config", h.MCPConfig},
		} {
			if src, ok := agentpkg.PinOf(f.path); ok && !referenced[src] {
				held = append(held, pathRef{src, name, f.key})
			}
		}
	}
	for _, r := range held {
		referenced[r.src] = true
		fmt.Fprintf(out, "agent: kept %s: no source names it, but [harness.%s] %s points inside it\n", r.src, r.name, r.key)
	}

	removed, err := agentpkg.Prune(referenced)
	if err != nil {
		return err
	}
	for _, r := range removed {
		fmt.Fprintf(out, "agent: pruned %s\n", r)
	}
	if len(removed) == 0 {
		fmt.Fprintln(out, "agent: nothing to prune")
	}
	fmt.Fprintln(out, "agent: prune considered only the global harness.toml — pins referenced solely by project files are not protected")
	return nil
}

// splitRef splits "<stable>/<package>[@<version>]".
func splitRef(ref string) (stable, pkg, version string, err error) {
	rest := ref
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		rest, version = ref[:at], ref[at+1:]
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", "", fmt.Errorf("%w: %q: want <stable>/<package>[@<version>]", agentpkg.ErrInvalidSource, ref)
	}
	stable, pkg = rest[:slash], rest[slash+1:]
	if !agentpkg.NamePattern.MatchString(stable) {
		return "", "", "", fmt.Errorf("%w: stable name %q must match %s", agentpkg.ErrInvalidSource, stable, agentpkg.NamePattern)
	}
	if !agentpkg.NamePattern.MatchString(pkg) {
		return "", "", "", fmt.Errorf("%w: package name %q must match %s", agentpkg.ErrInvalidSource, pkg, agentpkg.NamePattern)
	}
	return stable, pkg, version, nil
}

func hasHigh(findings []agentpkg.Finding) bool {
	for _, f := range findings {
		if f.Severity == agentpkg.SeverityHigh {
			return true
		}
	}
	return false
}

// reloadDaemonIfAny asks a running daemon to re-read its config, the same
// reload the TUI triggers after a write. No daemon running is not an error:
// the next daemon start loads the new file.
func reloadDaemonIfAny(cmd *cobra.Command, o verbOpts) {
	c, err := client.Dial(o.socket, buildinfo.Version, nil)
	if err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "agent: no daemon reachable; it loads the new config on its next start")
		return
	}
	defer c.Close()
	if _, err := c.Reload(); err != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "agent: daemon reload failed (it reloads on its next start): %v\n", err)
	}
}
