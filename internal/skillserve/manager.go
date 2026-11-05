// The daemon-side skill serving manager: clone inspection, the FTS5 index,
// the retrieval counts, and the reindex triggers (daemon start, file change
// in a clone, and a fast-forward reported by `harness skills sync`).
//
// Governing: ADR-0030 (serving clones, embedded index); SPEC-0007 REQ
// "Default-Branch Gate", REQ "Embedded Retrieval Index", REQ "Retrieval-Count
// Retirement", REQ "Skill Repos", REQ "Error Handling Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/stump-wtf/harness/internal/core"
)

// Sentinel errors (SPEC-0007 REQ "Error Handling Standards"). The
// malformed-frontmatter and skill-not-found sentinels live in skillstore and
// are reused; these two are the serving-layer additions.
var (
	// ErrSkillRepoNotFound is returned when no skill repo with the requested
	// name is declared.
	ErrSkillRepoNotFound = errors.New("skillserve: skill repo not found")

	// ErrNotServed is returned when the skill repo exists but its serve_to
	// selectors do not admit the calling harness (SPEC-0007 REQ "Search And
	// Retrieval Tools").
	ErrNotServed = errors.New("skillserve: skill repo is not served to this harness")

	// ErrBadSkillID is returned when an identifier does not parse as
	// <repo>/<slug>.
	ErrBadSkillID = errors.New("skillserve: malformed skill id")
)

// pollInterval is the reconciliation cadence: it catches a clone created or
// repaired while the watcher was not watching it yet, and re-checks the gate
// so a clone fixed by the operator returns to service without a daemon
// restart.
const pollInterval = time.Minute

// RepoStatus is one skill repo's serving state, as `harness doctor` and the
// control socket report it.
type RepoStatus struct {
	Name     string   `json:"name"`
	State    string   `json:"state"`
	Detail   string   `json:"detail,omitempty"`
	Skills   int      `json:"skills"`
	Warnings []string `json:"warnings,omitempty"`
}

// Manager owns the serving clones' index and counts for the daemon. It holds
// no write path into any clone: its only git access is the read-only
// inspection in clone.go.
type Manager struct {
	mu    sync.Mutex
	repos []core.SkillRepo

	indexPath   string
	countsPath  string
	index       *Index
	countsStore *CountsStore

	states      map[string]CloneInfo
	warnings    map[string][]string
	skillCounts map[string]int
	indexed     map[string]bool

	graceDays  int
	windowDays int
	summaryMax int

	// dirFor resolves a repo name to its serving clone. It defaults to
	// CloneDir; tests point it at a temp clone.
	dirFor func(string) string

	watcher *fsnotify.Watcher
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewManager builds a manager for the declared skill repos. Paths default to
// the state-directory layout; tests pass temp paths. graceDays, windowDays and
// summaryMaxChars come from the [skills] table; a summaryMaxChars below 1
// takes core.DefaultSummaryMaxChars.
func NewManager(repos []core.SkillRepo, indexPath, countsPath string, graceDays, windowDays, summaryMaxChars int) *Manager {
	if repos == nil {
		repos = []core.SkillRepo{}
	}
	if summaryMaxChars < 1 {
		summaryMaxChars = core.DefaultSummaryMaxChars
	}
	return &Manager{
		repos:       repos,
		indexPath:   indexPath,
		countsPath:  countsPath,
		states:      map[string]CloneInfo{},
		warnings:    map[string][]string{},
		skillCounts: map[string]int{},
		indexed:     map[string]bool{},
		graceDays:   graceDays,
		windowDays:  windowDays,
		summaryMax:  summaryMaxChars,
		dirFor:      CloneDir,
		stop:        make(chan struct{}),
	}
}

// Start opens the stores (only when a skill repo is declared: with none, no
// index file is created, SPEC-0007 REQ "Skill Repos"), runs the initial
// reindex, and serves reindex triggers until Stop.
func (m *Manager) Start() error {
	m.mu.Lock()
	repos := m.repos
	m.mu.Unlock()

	if len(repos) == 0 {
		return nil
	}
	if err := m.openStores(); err != nil {
		return err
	}
	m.reindexAll()
	m.watch()
	return nil
}

func (m *Manager) openStores() error {
	if m.index != nil {
		return nil
	}
	idx, err := OpenIndex(m.indexPath)
	if err != nil {
		return err
	}
	m.index = idx
	cs, err := OpenCounts(m.countsPath)
	if err != nil {
		_ = idx.Close()
		m.index = nil
		return err
	}
	m.countsStore = cs
	return nil
}

// Stop releases the watcher, the index and the counts store.
func (m *Manager) Stop() {
	close(m.stop)
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watcher != nil {
		_ = m.watcher.Close()
		m.watcher = nil
	}
	_ = m.index.Close()
	_ = m.countsStore.Close()
	m.index = nil
	m.countsStore = nil
}

// UpdateRepos applies a config reload: adds new repos, drops removed ones,
// and refreshes changed settings. Opening the stores is deferred to the next
// Start-like pass so a config that gains its first skill repo still creates
// the index.
func (m *Manager) UpdateRepos(repos []core.SkillRepo) {
	m.mu.Lock()
	m.repos = repos
	m.mu.Unlock()
	m.reindexAll()
}

// reindexAll reindexes every declared repo. Called on start, on reload, and
// from the reconcile loop.
func (m *Manager) reindexAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.repos))
	for _, r := range m.repos {
		names = append(names, r.Name)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.reindexRepo(name)
	}
}

// reindexRepo inspects one serving clone and, when the default-branch gate
// passes, replaces that repo's index rows. A detached or dirty clone keeps
// the previous index; its condition is recorded for `harness doctor`.
// Governing: SPEC-0007 REQ "Default-Branch Gate", REQ "Database Operation
// Standards".
func (m *Manager) reindexRepo(name string) {
	m.mu.Lock()
	var repo *core.SkillRepo
	for i := range m.repos {
		if m.repos[i].Name == name {
			repo = &m.repos[i]
			break
		}
	}
	if repo == nil {
		m.mu.Unlock()
		return
	}
	rc := *repo
	dir := m.dirFor(rc.Name)
	m.mu.Unlock()

	info, err := Inspect(dir)
	if err != nil {
		m.record(name, CloneInfo{State: CloneMissing, Detail: err.Error()}, nil, 0, false)
		return
	}

	if info.State != CloneOK {
		// Keep serving the previous index; only report the condition.
		m.record(name, info, nil, m.repoSkillCount(name), m.wasIndexed(name))
		return
	}

	entries, warnings, err := collectEntries(rc.Name, dir, rc.Path)
	if err != nil {
		// A missing or unreadable skill path keeps the previous index too.
		m.record(name, CloneInfo{State: info.State, DefaultBranch: info.DefaultBranch, Head: info.Head,
			Detail: err.Error()}, nil, m.repoSkillCount(name), m.wasIndexed(name))
		return
	}

	if err := m.index.ReindexRepo(rc.Name, entries); err != nil {
		// The transaction already rolled back: the previous index stays
		// queryable. Record the failure so doctor can surface it — as an
		// index error, not CloneDirty: the working tree is clean, and a
		// dirty diagnosis would send the operator to fix the wrong thing.
		m.record(name, CloneInfo{State: CloneIndexError, DefaultBranch: info.DefaultBranch, Head: info.Head,
			Detail: err.Error()}, warnings, m.repoSkillCount(name), m.wasIndexed(name))
		return
	}
	m.record(name, info, warnings, len(entries), true)
}

func (m *Manager) record(name string, info CloneInfo, warnings []string, skillCount int, indexed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[name] = info
	if warnings == nil {
		warnings = []string{}
	}
	m.warnings[name] = warnings
	m.skillCounts[name] = skillCount
	m.indexed[name] = indexed
}

func (m *Manager) repoSkillCount(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.skillCounts[name]
}

func (m *Manager) wasIndexed(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.indexed[name]
}

// Statuses returns one RepoStatus per declared repo, in declaration order.
func (m *Manager) Statuses() []RepoStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RepoStatus, 0, len(m.repos))
	for _, r := range m.repos {
		st := RepoStatus{Name: r.Name, State: string(m.states[r.Name].State), Detail: m.states[r.Name].Detail,
			Skills: m.skillCounts[r.Name]}
		if w := m.warnings[r.Name]; len(w) > 0 {
			st.Warnings = append([]string{}, w...)
		}
		out = append(out, st)
	}
	return out
}

// Search runs a query against the index on behalf of one caller: the repos it
// searches are exactly those whose serve_to admits that caller, so a caller
// cannot read a repo it is not served. An empty hit list with no error means
// the caller is scoped out of every declared repo.
//
// It is the seam the facade's search_skills tool (#79) calls through.
func (m *Manager) Search(caller Caller, query string, paths []string, limit int) ([]SearchHit, error) {
	m.mu.Lock()
	idx := m.index
	served := m.servedNames(caller)
	m.mu.Unlock()
	if idx == nil {
		return nil, fmt.Errorf("%w: no skill repos declared", ErrIndexUnavailable)
	}
	if len(served) == 0 {
		return nil, nil
	}
	return idx.Search(query, paths, limit, served)
}

// Skills returns the active skills the caller may read, which is exactly the
// set of skills the harness://skills/<repo>/<slug> resources cover (SPEC-0007
// REQ "Search And Retrieval Tools").
func (m *Manager) Skills(caller Caller) ([]SkillRef, error) {
	m.mu.Lock()
	idx := m.index
	served := m.servedNames(caller)
	m.mu.Unlock()
	if idx == nil {
		return nil, fmt.Errorf("%w: no skill repos declared", ErrIndexUnavailable)
	}
	if len(served) == 0 {
		return nil, nil
	}
	return idx.ListSkills(served)
}

// RecordRetrieval counts one search or get_skill retrieval per (skill repo,
// skill, calling harness, run) in daemon state (SPEC-0007 REQ "Search And
// Retrieval Tools").
func (m *Manager) RecordRetrieval(repo, slug, harness, run string, at time.Time) error {
	m.mu.Lock()
	cs := m.countsStore
	m.mu.Unlock()
	if cs == nil {
		return fmt.Errorf("skillserve: counts store not open")
	}
	return cs.Record(repo, slug, harness, run, at)
}

// EligibleForRetirement reports whether a skill has become eligible for a
// retire pull request: past the grace period since its merge, zero retrievals
// inside the window, and a count store that was not just recreated. The
// retire pull request itself belongs to #80.
func (m *Manager) EligibleForRetirement(repo, slug string, mergedAt time.Time, now time.Time) (bool, error) {
	m.mu.Lock()
	cs := m.countsStore
	grace, window := m.graceDays, m.windowDays
	m.mu.Unlock()
	if cs == nil {
		return false, nil
	}
	return cs.EligibleForRetirement(repo, slug, mergedAt, now, grace, window)
}

// watch serves the file-change reindex trigger until Stop: an fsnotify watch
// on each existing serving clone debounces into a reindex, and a reconcile
// ticker re-checks the gate so a clone created or repaired later is picked up
// without a daemon restart.
//
// Governing: SPEC-0007 REQ "Default-Branch Gate" (reindex on clone file
// change and on daemon start).
func (m *Manager) watch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		// No watcher only means reindex triggers come from the reconcile
		// ticker and the control socket; the index itself is unaffected.
		m.watcher = nil
	} else {
		for _, r := range m.repos {
			_ = w.Add(m.dirFor(r.Name))
		}
		m.watcher = w
	}
	m.wg.Add(1)
	go m.loop()
}

func (m *Manager) loop() {
	defer m.wg.Done()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	// Debounce window for fsnotify bursts (a sync checks out many files).
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	pending := map[string]bool{}

	for {
		select {
		case <-m.stop:
			return
		case evt, ok := <-m.watcherEvents():
			if !ok {
				m.mu.Lock()
				m.watcher = nil
				m.mu.Unlock()
				continue
			}
			name := m.repoForPath(evt.Name)
			if name == "" {
				continue
			}
			pending[name] = true
			debounce.Reset(2 * time.Second)
		case <-debounce.C:
			for name := range pending {
				m.reindexRepo(name)
			}
			pending = map[string]bool{}
		case <-ticker.C:
			m.reconcile()
		}
	}
}

// repoForPath maps a watched path back to its skill repo name.
func (m *Manager) repoForPath(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos {
		dir := m.dirFor(r.Name) + string(filepath.Separator)
		if strings.HasPrefix(path, dir) {
			return r.Name
		}
	}
	return ""
}

// reconcile re-checks every repo's gate. A clone that became OK (created by a
// sync that reached no daemon, or repaired by the operator) gets indexed; a
// clone that left OK keeps its previous index and is only re-reported.
func (m *Manager) reconcile() {
	m.mu.Lock()
	names := make([]string, 0, len(m.repos))
	for _, r := range m.repos {
		names = append(names, r.Name)
	}
	m.mu.Unlock()
	for _, name := range names {
		info, err := Inspect(m.dirFor(name))
		if err != nil {
			continue
		}
		m.mu.Lock()
		prev := m.states[name]
		m.mu.Unlock()
		if info.State == CloneOK && (!m.wasIndexed(name) || prev.State != CloneOK) {
			m.reindexRepo(name)
		} else if info.State != prev.State {
			m.reindexRepo(name)
		}
	}
}

// watcherEvents returns the watcher's event channel, or nil when no watcher
// is running (a nil channel blocks forever, so the loop just ticks).
func (m *Manager) watcherEvents() chan fsnotify.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watcher == nil {
		return nil
	}
	return m.watcher.Events
}

// ReindexRepos reindexes the named repos, normally after `harness skills
// sync` reported them as created or fast-forwarded (SPEC-0007 REQ
// "Default-Branch Gate"). Unknown names are ignored.
func (m *Manager) ReindexRepos(names []string) {
	m.mu.Lock()
	known := map[string]bool{}
	for _, r := range m.repos {
		known[r.Name] = true
	}
	m.mu.Unlock()
	for _, n := range names {
		if known[n] {
			m.reindexRepo(n)
		}
	}
}
