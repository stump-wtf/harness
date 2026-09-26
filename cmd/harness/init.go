package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/persona"
)

// newInitCmd is the `harness init` skeleton (SPEC-0018 REQ-4): only
// --list-templates is wired so far. The converge-style config generator
// itself lands with the answers/plan work (SPEC-0018 REQ-1/2/3/5/6, #437).
func newInitCmd(g *globalOpts) *cobra.Command {
	var listTemplates bool
	cmd := &cobra.Command{
		Use:           "init",
		Short:         "converge harnesses, client config and skills for a stack (--list-templates lists personas)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listTemplates {
				return runListTemplates(os.Stdout)
			}
			fmt.Fprintln(os.Stderr, "harness init: the config generator is not implemented yet (SPEC-0018 REQ-1/2/3/5/6, issue #437); use --list-templates")
			exitFn(2)
			return nil
		},
	}
	cmd.Flags().BoolVar(&listTemplates, "list-templates", false, "list the persona templates (built-in and yours)")
	return cmd
}

// runListTemplates prints each template's name, its source (built-in or
// path), and its description, noting shadowed built-ins
// (SPEC-0018 REQ-4 "Listing"). A malformed template anywhere fails the
// listing: it would fail `init` too, so it must not scroll past quietly.
func runListTemplates(stdout *os.File) error {
	templates, err := persona.List()
	if err != nil {
		return err
	}
	t := NewTable(stdout, "TEMPLATE", "SOURCE", "DESCRIPTION")
	for _, tpl := range templates {
		if tpl.Shadowed {
			// The user's shadow wins; say so, so an operator editing the
			// built-in text knows why nothing changes.
			t.Row(tpl.Name+" *", tpl.Source, tpl.Description+" — shadowed; your templates dir wins")
			continue
		}
		t.Row(tpl.Name, tpl.Source, tpl.Description)
	}
	return t.Flush()
}
