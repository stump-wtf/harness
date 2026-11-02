// Skill serving on the daemon: the serving-clone index manager, its control
// ops, and the reload path.
//
// The daemon indexes serving clones; it never creates, fetches, fast-forwards
// or writes one. `harness skills sync` (the CLI) owns those actions and
// reports fast-forwards here over the control socket.
//
// Governing: ADR-0030 (grounded skill distillation); SPEC-0007 REQ
// "Default-Branch Gate", REQ "Embedded Retrieval Index", REQ "Retrieval-Count
// Retirement", REQ "Skill Repos".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package daemon

import (
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/skillserve"
)

// newSkillsManager builds the serving manager from a parsed config.
func newSkillsManager(cfg *core.Config) *skillserve.Manager {
	repos := cfg.OrderedSkillRepos()
	return skillserve.NewManager(
		repos,
		skillserve.DefaultIndexPath(),
		skillserve.DefaultCountsPath(),
		cfg.Skills.RetireGraceDaysOrDefault(),
		cfg.Skills.RetireWindowDaysOrDefault(),
		cfg.Skills.SummaryMaxCharsOrDefault(),
	)
}

// loadDaemonConfig re-parses the config of record for the skills refresh.
func loadDaemonConfig(path string) (*core.Config, error) {
	return config.Load(path)
}

// opSkillsSynced reindexes the serving clones `harness skills sync` reported
// as created or fast-forwarded (SPEC-0007 REQ "Default-Branch Gate": the
// daemon reindexes when sync reports a fast-forward over the control socket).
// Each repo is re-inspected first, so a reported name whose gate now fails
// keeps its previous index instead of being indexed off-branch.
func (c *conn) opSkillsSynced(req protocol.ControlReq) {
	mgr := c.srv.skills
	if mgr == nil {
		_ = c.pc.WriteError(req.ID, protocol.ErrInternal, "no skill repos declared")
		return
	}
	mgr.ReindexRepos(req.Names)
	c.respond(req, skillsStatusData(mgr))
}

// opSkillsStatus returns each skill repo's serving state for `harness doctor`.
func (c *conn) opSkillsStatus() protocol.SkillsStatusData {
	return skillsStatusData(c.srv.skills)
}

func skillsStatusData(mgr *skillserve.Manager) protocol.SkillsStatusData {
	out := protocol.SkillsStatusData{Repos: []protocol.SkillRepoStatus{}}
	if mgr == nil {
		return out
	}
	for _, st := range mgr.Statuses() {
		out.Repos = append(out.Repos, protocol.SkillRepoStatus{
			Name:     st.Name,
			State:    st.State,
			Detail:   st.Detail,
			Skills:   st.Skills,
			Warnings: st.Warnings,
		})
	}
	return out
}

// SyncSkillsManager creates or refreshes the skill serving manager from the
// current config. It runs on daemon start and after every successful reload,
// so declared skill repos always match the config of record. With no skill
// repo declared, no index file is created (SPEC-0007 REQ "Skill Repos").
func (s *Server) SyncSkillsManager() error {
	cfg, err := loadDaemonConfig(s.configPath)
	if err != nil {
		// The reload path has already reported the config error; a skills
		// refresh simply keeps the previous state.
		return nil
	}
	repos := cfg.OrderedSkillRepos()
	s.skillsMu.Lock()
	mgr := s.skills
	s.skillsMu.Unlock()
	if mgr == nil {
		if len(repos) == 0 {
			return nil
		}
		mgr = newSkillsManager(cfg)
		s.skillsMu.Lock()
		s.skills = mgr
		s.skillsMu.Unlock()
		// The initial reindex runs alongside startup rather than blocking it;
		// until it finishes the previous (empty) index serves no results.
		return mgr.Start()
	}
	mgr.UpdateRepos(repos)
	return nil
}
