package governance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// workflowEvents returns the names a workflow is triggered by. `on:` is either
// one event, a list of them, or a mapping whose keys carry each event options.
func workflowEvents(doc map[string]any) map[string]bool {
	out := map[string]bool{}
	switch on := doc["on"].(type) {
	case string:
		out[strings.ToLower(on)] = true
	case []any:
		for _, event := range on {
			out[strings.ToLower(fmt.Sprint(event))] = true
		}
	case map[string]any:
		for name := range on {
			out[strings.ToLower(name)] = true
		}
	}
	return out
}

// concurrencyGroup returns the run-level concurrency group expression, and
// whether the workflow declares run-level concurrency at all. `concurrency:` is
// either that expression on its own or a mapping holding it.
func concurrencyGroup(doc map[string]any) (string, bool) {
	switch c := doc["concurrency"].(type) {
	case nil:
		return "", false
	case string:
		return c, true
	case map[string]any:
		group, _ := c["group"].(string)
		return group, true
	default:
		return fmt.Sprint(c), true
	}
}

// TestWorkflowConcurrencySeparatesPRs pins the concurrency key of every Gitea
// workflow in this repository.
//
// Correction (2026-10-29, runs-API re-read in #1027): the ref-only key never
// actually collapsed pull requests here — cross-PR runs coexisted in flight —
// so the pull_request.number term is defensive hygiene, not a fix for observed
// cancellations. It is still worth pinning: pull_request rows display a null
// ref and head_branch in actions/runs, so what ref a pull_request run gets
// server-side is not visible from the outside and is not contractually
// stable, and the per-PR term keeps the group correct whichever way the runner
// fills it.
//
// The ref still belongs in the key: main, the train/* branches and tags each
// need their own group, or a push to main would cancel the run already shipping
// it.
func TestWorkflowConcurrencySeparatesPRs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot(t), ".gitea", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflows found under .gitea/workflows")
	}

	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: parse workflow: %v", filepath.Base(path), err)
			continue
		}
		group, declared := concurrencyGroup(doc)
		if !declared {
			continue
		}
		name := filepath.Base(path)
		triggers := workflowEvents(doc)

		if !strings.Contains(group, ".ref") && (triggers["push"] || triggers["pull_request"]) {
			t.Errorf("%s: concurrency group %q separates nothing: push runs of main and the train share one group", name, group)
		}
		if triggers["pull_request"] && !strings.Contains(group, "pull_request.number") {
			t.Errorf("%s: concurrency group %q has no per-PR term; append ${{ gitea.event.pull_request.number }} so each pull request keeps its own group regardless of how the runner fills ref (#1027)", name, group)
		}
	}
}
