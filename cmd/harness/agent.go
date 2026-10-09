package main

// Agent Package Commands
//
// The `harness agent` subtree is the CLI half of SPEC-0026: it manages the
// [stable.*] trust ledger, clones and fast-forwards stable repositories, and
// discovers packages in them. Every command here runs entirely in the CLI
// process, reading and writing harness.toml and the on-disk stores directly
// (ADR-0044) — no daemon required, and no daemon involvement: `stable
// update` is the only command in the tree that performs a network fetch.
//
// Remotes are shown redacted everywhere: a remote may carry userinfo, and no
// output or error line may echo it.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-1 (stable
// registration and global-only trust), REQ-2 (stable layout and local
// discovery), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#811.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/agentpkg/scan"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/config/tomledit"
	"github.com/stump-wtf/harness/internal/core"
)

func newAgentCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "agent",
		Short:         "install and manage agent packages from trusted stables",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(
		newAgentStableCmd(g),
		newAgentListCmd(g),
		newAgentInstallCmd(g),
		newAgentUninstallCmd(g),
		newAgentPruneCmd(g),
		newAgentUpgradeCmd(g),
		newAgentSearchCmd(g),
		newAgentInfoCmd(g),
	)
	return cmd
}

// loadGlobalConfig loads the global config, tolerating a missing file: the
// agent commands may be the first thing ever written into it (a fresh
// machine has no harness.toml until `agent stable add` creates one).
func loadGlobalConfig(path string) (*core.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &core.Config{}, nil
		}
		return nil, err
	}
	return cfg, nil
}

// loadGlobalEditor loads the config file for editing, creating an empty
// editor when the file does not exist yet. The parent directory is created
// so a fresh $XDG_CONFIG_HOME/harness can receive its first table.
func loadGlobalEditor(path string) (*tomledit.Editor, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("agent: create config directory: %w", err)
	}
	ed, err := tomledit.Load(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return tomledit.New(nil), nil
		}
		return nil, fmt.Errorf("agent: load %s: %w", path, err)
	}
	return ed, nil
}

// ---- harness agent stable -------------------------------------------------

func newAgentStableCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "stable",
		Short:         "manage the [stable.*] trust ledger and its clones",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	add := &cobra.Command{
		Use:           "add NAME REMOTE",
		Short:         "trust a stable: clone REMOTE, then write [stable.NAME]",
		Args:          cobra.ExactArgs(2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentStableAdd(cmd, g.opts(), args[0], args[1])
		},
	}
	var public, private bool
	add.Flags().BoolVar(&public, "public", false, "any harness may install from this stable (default)")
	add.Flags().BoolVar(&private, "private", false, "only this operator's own harness installs")
	add.PreRunE = func(cmd *cobra.Command, args []string) error {
		if public && private {
			return fmt.Errorf("agent: --public and --private are mutually exclusive")
		}
		return nil
	}

	remove := &cobra.Command{
		Use:           "remove NAME",
		Short:         "remove the [stable.NAME] table (installed harnesses are untouched)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentStableRemove(cmd, g.opts(), args[0])
		},
	}

	update := &cobra.Command{
		Use:           "update [NAME]",
		Short:         "fetch and fast-forward stable clones (the only command that fetches)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			return runAgentStableUpdate(cmd, g.opts(), name)
		},
	}

	list := &cobra.Command{
		Use:           "list",
		Short:         "list trusted stables",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentStableList(cmd, g.opts())
		},
	}

	cmd.AddCommand(add, remove, update, list, newAgentStableLintCmd(), newAgentStableCheckCmd())
	return cmd
}

// runAgentStableAdd clones before trusting (SPEC-0026 REQ-1): the remote is
// cloned into a temp directory and renamed into place, and only then is the
// [stable.NAME] table written. A failed clone writes nothing; a failed table
// write removes the clone again, so a stable is never half-registered.
func runAgentStableAdd(cmd *cobra.Command, o verbOpts, name, remote string) error {
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	if _, exists := cfg.Stables[name]; exists {
		return fmt.Errorf("agent: stable %q is already registered; run `harness agent stable update %s` to fetch it", name, name)
	}

	head, err := agentpkg.CloneStable(name, remote)
	if err != nil {
		return err
	}

	ed, err := loadGlobalEditor(o.configPath)
	if err != nil {
		agentRemoveClone(name)
		return err
	}
	keys := [][2]any{{"remote", remote}}
	if cmd.Flags().Changed("private") {
		keys = append(keys, [2]any{"public", false})
	}
	if err := ed.AddStable(name, keys); err != nil {
		agentRemoveClone(name)
		return fmt.Errorf("agent: register stable %q: %w", name, err)
	}
	if err := ed.Save(o.configPath); err != nil {
		agentRemoveClone(name)
		return fmt.Errorf("agent: write %s: %w", o.configPath, err)
	}

	vis := "public"
	if cmd.Flags().Changed("private") {
		vis = "private"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "agent: stable %s added from %s (head %s, %s)\n",
		name, redact.Redact(remote), shortSHA(head), vis)
	return nil
}

func agentRemoveClone(name string) {
	os.RemoveAll(agentpkg.StableDir(name))
}

// runAgentStableRemove deletes the table and reports every installed harness
// whose source names that stable, without uninstalling anything (SPEC-0026
// REQ-1). The clone is left on disk — installed pins never depended on it —
// and re-adding the name replaces it.
func runAgentStableRemove(cmd *cobra.Command, o verbOpts, name string) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	if _, exists := cfg.Stables[name]; !exists {
		return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, name)
	}

	var installed []string
	for _, hname := range cfg.HarnessOrder {
		h := cfg.Harnesses[hname]
		if h.PackageSource == "" {
			continue
		}
		if src, err := agentpkg.ParseSource(h.PackageSource); err == nil && src.Stable == name {
			installed = append(installed, fmt.Sprintf("%s (source %s)", hname, h.PackageSource))
		}
	}

	ed, err := loadGlobalEditor(o.configPath)
	if err != nil {
		return err
	}
	if err := ed.RemoveStable(name); err != nil {
		return fmt.Errorf("agent: remove [stable.%s]: %w", name, err)
	}
	if err := ed.Save(o.configPath); err != nil {
		return fmt.Errorf("agent: write %s: %w", o.configPath, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "agent: stable %s removed\n", name)
	for _, h := range installed {
		fmt.Fprintf(cmd.OutOrStdout(), "agent: still installed: harness %s — left exactly as it was; it resolves from the local pin store, not the stable\n", h)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "agent: the clone at %s was left in place; re-adding %q replaces it\n",
		agentpkg.StableDir(name), name)
	return nil
}

// runAgentStableUpdate is the one operation that fetches (SPEC-0026 REQ-1).
// With a name it updates that stable; without one, every registered stable.
// A diverged clone fails with the diverged-clone sentinel and is left
// untouched.
func runAgentStableUpdate(cmd *cobra.Command, o verbOpts, name string) error {
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}

	var names []string
	if name != "" {
		if _, exists := cfg.Stables[name]; !exists {
			return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, name)
		}
		names = []string{name}
	} else {
		for _, s := range cfg.OrderedStables() {
			names = append(names, s.Name)
		}
		if len(names) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "agent: no stables registered; nothing to update")
			return nil
		}
	}

	var failed error
	for _, n := range names {
		res, err := agentpkg.UpdateStable(n)
		switch {
		case err != nil:
			fmt.Fprintf(cmd.ErrOrStderr(), "agent: %v\n", err)
			failed = err
		case res.FastForwarded:
			fmt.Fprintf(cmd.OutOrStdout(), "agent: stable %s: fast-forwarded to %s\n", n, shortSHA(res.Head))
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "agent: stable %s: up to date (%s)\n", n, shortSHA(res.Head))
		}
	}
	return failed
}

func runAgentStableList(cmd *cobra.Command, o verbOpts) error {
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	stables := cfg.OrderedStables()
	if len(stables) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "agent: no stables registered (add one with `harness agent stable add NAME REMOTE`)")
		return nil
	}
	for _, s := range stables {
		vis := "public"
		if !s.Public {
			vis = "private"
		}
		head, err := agentpkg.CloneHead(s.Name)
		state := "no clone (run `harness agent stable add` again)"
		if err == nil {
			state = "head " + shortSHA(head)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "agent: %s  %s  %s  %s\n", s.Name, redact.Redact(s.Remote), vis, state)
	}
	return nil
}

// ---- harness agent search -------------------------------------------------

func newAgentSearchCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "search [QUERY] [--stable NAME]",
		Short:         "search packages in local stable clones (no fetch)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) > 0 {
				query = args[0]
			}
			stable, _ := cmd.Flags().GetString("stable")
			return runAgentSearch(cmd, g.opts(), query, stable)
		},
	}
	cmd.Flags().String("stable", "", "search only this stable")
	return cmd
}

// runAgentSearch lists packages whose name or description matches the query
// case-insensitively, reading only local clones (SPEC-0026 REQ-2): the clone
// answers as the last `stable update` left it, and nothing here fetches. An
// unreadable manifest never fails the search; the package is listed as
// invalid with its load error.
func runAgentSearch(cmd *cobra.Command, o verbOpts, query, stable string) error {
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}

	stables := cfg.OrderedStables()
	if stable != "" {
		s, exists := cfg.Stables[stable]
		if !exists {
			return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, stable)
		}
		stables = []core.Stable{s}
	}
	if len(stables) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "agent: no stables registered; nothing to search")
		return nil
	}

	q := strings.ToLower(query)
	found := 0
	for _, s := range stables {
		pkgs, err := agentpkg.ListPackages(s.Name)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "agent: %v\n", err)
			continue
		}
		for _, p := range pkgs {
			if p.Err != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "agent: %s/%s: invalid package: %v\n", s.Name, p.Name, p.Err)
				continue
			}
			name := strings.ToLower(p.Name)
			desc := strings.ToLower(p.Manifest.Package.Description)
			if q != "" && !strings.Contains(name, q) && !strings.Contains(desc, q) {
				continue
			}
			found++
			line := fmt.Sprintf("agent: %s/%s", s.Name, p.Name)
			if p.Manifest.Package.Version != "" {
				line += " " + p.Manifest.Package.Version
			}
			if p.Manifest.Package.Description != "" {
				line += " — " + p.Manifest.Package.Description
			}
			fmt.Fprintln(cmd.OutOrStdout(), line)
		}
	}
	if found == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "agent: no matching packages")
	}
	return nil
}

// ---- harness agent info ---------------------------------------------------

func newAgentInfoCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "info STABLE/PACKAGE",
		Short:         "show a package's manifest, requests and bundled files (no fetch, no install)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentInfo(cmd, g.opts(), args[0])
		},
	}
	return cmd
}

// runAgentInfo prints a package's manifest, [requests] table, scan findings
// and bundled file list from the stable's local clone, without installing
// anything (SPEC-0026 REQ-2: no entry appears under the content-addressed
// store; REQ-5: the findings and the no-guarantee statement print too).
func runAgentInfo(cmd *cobra.Command, o verbOpts, ref string) error {
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return fmt.Errorf("%w: %q: want <stable>/<package>", agentpkg.ErrInvalidSource, ref)
	}
	stable, pkg := ref[:slash], ref[slash+1:]

	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	if _, exists := cfg.Stables[stable]; !exists {
		return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, stable)
	}

	man, err := agentpkg.LoadPackage(stable, pkg)
	if err != nil {
		return err
	}
	files, err := agentpkg.BundledFiles(stable, pkg)
	if err != nil {
		return err
	}
	// The scan runs read-only over the local clone (SPEC-0026 REQ-5): info
	// never fetches and never writes anything under the pin store.
	findings, err := scan.Scan(agentpkg.PackageDir(stable, pkg), man)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	p := man.Package
	fmt.Fprintf(out, "agent: %s\n", ref)
	fmt.Fprintf(out, "package:\n")
	fmt.Fprintf(out, "  name %s\n", p.Name)
	if p.Version != "" {
		fmt.Fprintf(out, "  version %s\n", p.Version)
	}
	if p.Description != "" {
		fmt.Fprintf(out, "  description %s\n", p.Description)
	}
	if p.Author != "" {
		fmt.Fprintf(out, "  author %s\n", p.Author)
	}
	if p.Homepage != "" {
		fmt.Fprintf(out, "  homepage %s\n", p.Homepage)
	}

	hv := man.Harness
	fmt.Fprintf(out, "harness:\n")
	fmt.Fprintf(out, "  harness %s\n", hv.Harness)
	if hv.Model != "" {
		fmt.Fprintf(out, "  model %s\n", hv.Model)
	}
	for _, a := range hv.Args {
		fmt.Fprintf(out, "  arg %s\n", a)
	}
	for _, a := range hv.Argv {
		fmt.Fprintf(out, "  argv %s\n", a)
	}
	if hv.AutoAccept != nil {
		fmt.Fprintf(out, "  auto_accept %t\n", *hv.AutoAccept)
	}
	if hv.MaxTurns != nil {
		fmt.Fprintf(out, "  max_turns %d\n", *hv.MaxTurns)
	}
	if hv.Quiet != nil {
		fmt.Fprintf(out, "  quiet %t\n", *hv.Quiet)
	}
	for _, p := range []struct{ key, val string }{
		{"prompt", hv.Prompt},
		{"prompt_file", hv.PromptFile},
		{"prompt_template", hv.PromptTemplate},
		{"prompt_template_file", hv.PromptTemplateFile},
	} {
		if p.val != "" {
			fmt.Fprintf(out, "  %s %s\n", p.key, p.val)
		}
	}
	if hv.SystemPromptFile != "" {
		fmt.Fprintf(out, "  system_prompt_file %s\n", hv.SystemPromptFile)
	}
	if hv.MCPConfig != "" {
		fmt.Fprintf(out, "  mcp_config %s\n", hv.MCPConfig)
	}
	for _, t := range hv.AllowedTools {
		fmt.Fprintf(out, "  allowed_tool %s\n", t)
	}

	req := man.Requests
	fmt.Fprintf(out, "requests:\n")
	if req.SkillPaths != nil {
		fmt.Fprintf(out, "  skill_paths %t\n", *req.SkillPaths)
	}
	for _, m := range req.MCPAllow {
		fmt.Fprintf(out, "  mcp_allow %s\n", m)
	}
	if req.Network != nil {
		fmt.Fprintf(out, "  network %t\n", *req.Network)
	}

	fmt.Fprintf(out, "scan findings:\n")
	if len(findings) == 0 {
		fmt.Fprintf(out, "  none\n")
	}
	for _, f := range findings {
		fmt.Fprintf(out, "  %s:%d  %s  %s\n", f.File, f.Line, f.PatternID, f.Severity)
	}
	fmt.Fprintf(out, "bundled files:\n")
	for _, f := range files {
		fmt.Fprintf(out, "  %s\n", f)
	}
	fmt.Fprintf(out, "%s\n", agentpkg.NoGuarantee)
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
