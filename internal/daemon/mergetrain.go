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
// Governing: ADR-0032; SPEC-0025 REQ-1, REQ-11, REQ-12, REQ-15.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/gitea"
	"github.com/stump-wtf/harness/internal/mergetrain"
)

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
		return nil, errors.New("[mergetrain] is enabled but forge_token_env is not set")
	}
	token := o.Getenv(c.ForgeTokenEnv)
	if strings.TrimSpace(token) == "" {
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
	root := filepath.Join(o.StateDir, "mergetrain")
	f, err := newForge(c.ForgeBaseURL, token, filepath.Join(root, "cache"))
	if err != nil {
		return nil, fmt.Errorf("[mergetrain]: %w", err)
	}

	// Name who the train will act as. A failure here is not fatal: the
	// first build fails loudly on its own if the token is bad.
	as := "unknown"
	if l, ok := f.(interface {
		Login(context.Context) (string, error)
	}); ok {
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		login, lerr := l.Login(lctx)
		cancel()
		if lerr != nil {
			log.Warn("merge train: could not read the forge identity", "err", lerr)
		} else {
			as = login
		}
	}

	m := &MergeTrain{}
	for _, repo := range c.Repos {
		d, err := mergetrain.NewDriver(mergetrain.DriverConfig{
			Repo:         repo,
			BaseBranch:   c.BaseBranch,
			Mode:         mergetrain.Mode(c.Mode),
			PollInterval: c.PollInterval,
			CITimeout:    c.CITimeout,
			LockDir:      root,
			Log:          log,
			TrainPollMin: o.TrainPollMin,
			RetryPause:   o.RetryPause,
		}, f)
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

	log.Warn("merge train enabled", "mode", c.Mode, "repos", strings.Join(c.Repos, ","), "as", as,
		"running", len(m.drivers), "base_branch", c.BaseBranch)

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
