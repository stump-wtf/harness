// Control-op tests for skill serving: the status op answers even with no
// manager, and the sync-report op reindexes only declared repos.
//
// Governing: SPEC-0007 REQ "Default-Branch Gate"; SPEC-0002 REQ "Control
// Operations" (every verb is on the socket).
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/skillserve"
)

func TestSkillsStatusOpAnswersWithoutManager(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)
	data, err := c.SkillsStatus()
	if err != nil {
		t.Fatalf("skills_status: %v", err)
	}
	if len(data.Repos) != 0 {
		t.Fatalf("no skill repos declared: want an empty reply, got %+v", data)
	}
}

func TestSkillsStatusOpReportsManagerState(t *testing.T) {
	mgr := skillserve.NewManager([]core.SkillRepo{
		{Name: "go-stack", Remote: "unused", Path: "skills", ServeTo: []string{"*"}, Public: true},
	}, filepath.Join(t.TempDir(), "index.db"), filepath.Join(t.TempDir(), "counts.db"), 30, 60, 0)
	td := newTestDaemon(t, sleeperTOML, func(o *Options) { o.Skills = mgr })
	c := td.dial(t, nil)
	data, err := c.SkillsStatus()
	if err != nil {
		t.Fatalf("skills_status: %v", err)
	}
	if len(data.Repos) != 1 || data.Repos[0].Name != "go-stack" {
		t.Fatalf("want the declared repo, got %+v", data)
	}
}
