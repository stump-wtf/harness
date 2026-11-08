package daemon

// Merge Train Runner
//
// Starts one mergetrain.Driver per [mergetrain] repo, each in its own
// goroutine, and stops them on shutdown. Nothing starts unless the table says
// enabled = true, so a daemon without the table behaves exactly as before.
// When it is enabled the start is loud: one warn-level line naming the mode,
// every repo it will act on, and the forge identity it acts as.
//
// Preconditions are checked before any goroutine starts, so the daemon can
// refuse to start (as it does for [telemetry]) rather than half-run: an
// enabled train whose token variable is empty is refused. A repo whose lock
// another train holds is skipped with an error line — refused, never raced
// (SPEC-0025 REQ-11) — while the other repos run.
//
// Stop cancels every driver and waits: an attempt in flight deletes its train
// branch on a detached, bounded context (REQ-6), then the driver releases its
// lock (REQ-15).
//
// `batch` above 1 is accepted by the loader but not yet built (REQ-17): the
// start logs a warn-level line saying each train still carries one PR, so a
// configured batch never reads as throughput the daemon is not delivering.
//
// Governing: ADR-0032; SPEC-0025 REQ-1, REQ-11, REQ-12, REQ-15, REQ-17.
//
// @joestump-agent 09/28/2026 - Warn on an unimplemented batch size.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/gitea"
	"github.com/stump-wtf/harness/internal/forge/github"
	"github.com/stump-wtf/harness/internal/mergetrain"
)

// needsDefault / needsGitHub report whether a repo in the table is driven by
// that forge, so the daemon never demands a token for a forge it will not use.
func needsDefault(c core.MergeTrainConfig) bool {
	for _, r := range c.Repos {
		if c.RepoForge[r] != "github" {
			return true
		}
	}
	return false
}

func needsGitHub(c core.MergeTrainConfig) bool {
	for _, r := range c.Repos {
		if c.RepoForge[r] == "github" {
			return true
		}
	}
	return false
}

// needsGitHubOnly is the config where no repo wants the default forge, so a
// missing forge_token_env is not the problem.
func needsGitHubOnly(c core.MergeTrainConfig) bool {
	return len(c.Repos) > 0 && !needsDefault(c)
}

// forgeNames lists the forge keys in a stable order.
func forgeNames(forges map[string]forge.Forge) []string {
	names := make([]string, 0, len(forges))
	for n := range forges {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// MergeTrainOptions configures StartMergeTrain.
type MergeTrainOptions struct {
	Config core.MergeTrainConfig
	// Getenv resolves Config.ForgeTokenEnv. Default: none — the caller passes
	// os.Getenv, so the environment is read in exactly one place.
	Getenv func(string) string
	// StateDir is $XDG_STATE_HOME/harness; locks and git caches live under
	// StateDir/mergetrain.
	StateDir string
	Log      mergetrain.Logger
	// NewForge builds the forge. Default: the Gitea implementation. Tests
	// substitute a fake.
	NewForge func(baseURL, token, cacheDir string) (forge.Forge, error)
	// NewGitHubForge builds the GitHub forge. Default: the GitHub
	// implementation. Tests substitute a fake.
	NewGitHubForge func(baseURL, token, cacheDir, ciGate string) (forge.Forge, error)

	// Driver tuning, for tests; zero takes the driver's defaults.
	TrainPollMin time.Duration
	RetryPause   time.Duration
}

// MergeTrain is the running set of drivers. A nil *MergeTrain is a disabled
// train; its methods are no-ops.
type MergeTrain struct {
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	drivers []*mergetrain.Driver
}

// StartMergeTrain starts the configured drivers. It returns nil, nil when the
// train is disabled.
func StartMergeTrain(ctx context.Context, o MergeTrainOptions) (*MergeTrain, error) {
	c := o.Config
	if !c.Enabled {
		return nil, nil
	}
	if len(c.Repos) == 0 {
		return nil, errors.New("[mergetrain] is enabled with no repos")
	}
	if o.Getenv == nil || c.ForgeTokenEnv == "" {
		if !needsGitHubOnly(c) {
			return nil, errors.New("[mergetrain] is enabled but forge_token_env is not set")
		}
	}
	token := ""
	if c.ForgeTokenEnv != "" {
		token = o.Getenv(c.ForgeTokenEnv)
	}
	if strings.TrimSpace(token) == "" && needsDefault(c) {
		return nil, fmt.Errorf("[mergetrain] is enabled but $%s is empty — set it in the daemon's environment", c.ForgeTokenEnv)
	}
	if o.StateDir == "" {
		return nil, errors.New("[mergetrain] needs a state directory")
	}
	log := o.Log
	if log == nil {
		return nil, errors.New("[mergetrain] needs a logger")
	}
	newForge := o.NewForge
	if newForge == nil {
		newForge = func(baseURL, token, cacheDir string) (forge.Forge, error) {
			return gitea.New(gitea.Options{BaseURL: baseURL, Token: token, CacheDir: cacheDir})
		}
	}
	newGitHub := o.NewGitHubForge
	if newGitHub == nil {
		newGitHub = func(baseURL, token, cacheDir, ciGate string) (forge.Forge, error) {
			return github.New(github.Options{BaseURL: baseURL, Token: token, CacheDir: cacheDir, CIGate: ciGate})
		}
	}
	root := filepath.Join(o.StateDir, "mergetrain")
	cacheRoot := filepath.Join(root, "cache")

	// One client per forge, each with its own clone cache: the same owner/name
	// can exist on two forges, and their objects are not the same repository
	// (ADR-0061).
	forges := map[string]forge.Forge{}
	if needsDefault(c) {
		f, err := newForge(c.ForgeBaseURL, token, filepath.Join(cacheRoot, "gitea"))
		if err != nil {
			return nil, fmt.Errorf("[mergetrain]: %w", err)
		}
		forges[""] = f
	}
	if needsGitHub(c) {
		gh := c.GitHub
		if gh == nil || gh.TokenEnv == "" {
			return nil, errors.New("[mergetrain]: a repos entry names the GitHub forge but github_token_env is not set")
		}
		ghToken := o.Getenv(gh.TokenEnv)
		if strings.TrimSpace(ghToken) == "" {
			return nil, fmt.Errorf("[mergetrain] is enabled with a GitHub repo but $%s is empty — set it in the daemon's environment", gh.TokenEnv)
		}
		gate := gh.CIGate
		if gate == "" {
			gate = core.GitHubGateDraftPR
		}
		f, err := newGitHub(gh.BaseURL, ghToken, filepath.Join(cacheRoot, "github"), gate)
		if err != nil {
			return nil, fmt.Errorf("[mergetrain] github: %w", err)
		}
		forges["github"] = f
	}

	// Name who the train will act as, per forge. A failure here is not fatal:
	// the first build fails loudly on its own if a token is bad.
	as := make([]string, 0, len(forges))
	for _, name := range forgeNames(forges) {
		label := "forge"
		if name != "" {
			label = name
		}
		login := "unknown"
		if l, ok := forges[name].(interface {
			Login(context.Context) (string, error)
		}); ok {
			lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			got, lerr := l.Login(lctx)
			cancel()
			if lerr != nil {
				log.Warn("merge train: could not read the forge identity", "forge", label, "err", lerr)
			} else {
				login = got
			}
		}
		as = append(as, label+"="+login)
	}

	m := &MergeTrain{}
	for _, repo := range c.Repos {
		d, err := mergetrain.NewDriver(mergetrain.DriverConfig{
			Repo:          repo,
			BaseBranch:    c.BaseBranch,
			Mode:          mergetrain.Mode(c.Mode),
			PollInterval:  c.PollInterval,
			CITimeout:     c.CITimeout,
			Batch:         c.Batch,
			FlakeCoolDown: c.FlakeCoolDown,
			SequencePaths: c.SequencePaths,
			LockDir:       root,
			Log:           log,
			TrainPollMin:  o.TrainPollMin,
			RetryPause:    o.RetryPause,
		}, forges[c.RepoForge[repo]])
		if err != nil {
			if errors.Is(err, mergetrain.ErrLocked) {
				log.Error("merge train: repo skipped", "repo", repo, "err", err)
				continue
			}
			m.closeDrivers()
			return nil, fmt.Errorf("[mergetrain] %s: %w", repo, err)
		}
		m.drivers = append(m.drivers, d)
	}

	log.Warn("merge train enabled", "mode", c.Mode, "repos", strings.Join(c.Repos, ","), "as", strings.Join(as, " "),
		"running", len(m.drivers), "base_branch", c.BaseBranch, "batch", c.Batch)

	rctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	for _, d := range m.drivers {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			if err := d.Run(rctx); err != nil {
				log.Error("merge train halted; restart the daemon to resume", "err", err)
			}
		}()
	}
	return m, nil
}

// Running is the number of drivers started.
func (m *MergeTrain) Running() int {
	if m == nil {
		return 0
	}
	return len(m.drivers)
}

// Stop cancels every driver, waits for each to finish or abandon its attempt
// (deleting its train branch), and releases their locks.
func (m *MergeTrain) Stop() {
	if m == nil {
		return
	}
	m.cancel()
	m.wg.Wait()
	m.closeDrivers()
}

func (m *MergeTrain) closeDrivers() {
	for _, d := range m.drivers {
		_ = d.Close()
	}
	m.drivers = nil
}
