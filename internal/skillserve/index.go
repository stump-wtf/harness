// The embedded FTS5 retrieval index over the serving clones' active skills.
//
// Governing: ADR-0030 (FTS5 over embeddings); SPEC-0007 REQ "Embedded
// Retrieval Index", REQ "Database Operation Standards", REQ "Error Handling
// Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stump-wtf/harness/internal/supervisor"

	_ "modernc.org/sqlite"
)

// ErrIndexUnavailable marks an index that could not be opened or queried.
// Searching with it down is a degraded service, not a lost cache: the index is
// rebuildable from the serving clones.
var ErrIndexUnavailable = errors.New("skillserve: index unavailable")

// Result limits for search_skills (SPEC-0007 REQ "Search And Retrieval
// Tools"): three hits unless the caller asks for more, and five at most
// however loudly it asks.
const (
	DefaultSearchLimit = 3
	MaxSearchLimit     = 5
)

// Index is the embedded full-text retrieval index. It holds no external
// service, no network access and no model weights; deleting the database file
// and reindexing from the serving clones restores equivalent behavior.
type Index struct {
	db *sql.DB
}

// DefaultIndexPath returns the index database path under the state directory:
// $XDG_STATE_HOME/harness/skills/index.db.
func DefaultIndexPath() string {
	return filepath.Join(supervisor.StateHome(), "skills", "index.db")
}

// OpenIndex opens (and creates) the index database at path. A missing parent
// directory is created, so a caller that has no skill repos declared can avoid
// ever creating the file by not calling this (SPEC-0007 REQ "Skill Repos":
// with no skill repo declared, no index file is created).
func OpenIndex(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("%w: create %s: %v", ErrIndexUnavailable, filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %v", ErrIndexUnavailable, path, err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

// migrate creates the schema. The FTS table is standalone (not external
// content): a full repo reindex rewrites both tables inside one transaction.
func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS skill_meta (
			rowid INTEGER PRIMARY KEY,
			repo TEXT NOT NULL,
			slug TEXT NOT NULL CHECK (length(slug) > 0),
			file_path TEXT NOT NULL,
			applies_to TEXT NOT NULL DEFAULT '',
			merged_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_skill_meta_repo ON skill_meta(repo)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS skill_fts USING fts5(
			name, description, symptoms, tags, applies_to,
			tokenize = 'porter unicode61'
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			_ = db.Close()
			return fmt.Errorf("%w: migrate: %v", ErrIndexUnavailable, err)
		}
	}
	return nil
}

// Close releases the database connection.
func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

// ReindexRepo replaces one skill repo's rows in a single transaction: a
// failure partway leaves the previous index contents queryable and no partial
// state visible (SPEC-0007 REQ "Database Operation Standards").
func (i *Index) ReindexRepo(repo string, entries []Entry) error {
	if i == nil || i.db == nil {
		return fmt.Errorf("%w: not open", ErrIndexUnavailable)
	}
	tx, err := i.db.Begin()
	if err != nil {
		return fmt.Errorf("%w: begin: %v", ErrIndexUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM skill_fts WHERE rowid IN (SELECT rowid FROM skill_meta WHERE repo = ?)`, repo); err != nil {
		return fmt.Errorf("%w: clear fts for %s: %v", ErrIndexUnavailable, repo, err)
	}
	if _, err := tx.Exec(`DELETE FROM skill_meta WHERE repo = ?`, repo); err != nil {
		return fmt.Errorf("%w: clear meta for %s: %v", ErrIndexUnavailable, repo, err)
	}
	for _, e := range entries {
		res, err := tx.Exec(
			`INSERT INTO skill_meta (rowid, repo, slug, file_path, applies_to, merged_at)
			 VALUES (NULL, ?, ?, ?, ?, ?)`,
			e.Repo, e.Slug, e.RelPath, strings.Join(e.AppliesTo, " "), e.MergedAt)
		if err != nil {
			return fmt.Errorf("%w: insert meta %s/%s: %v", ErrIndexUnavailable, repo, e.Slug, err)
		}
		rowid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("%w: rowid %s/%s: %v", ErrIndexUnavailable, repo, e.Slug, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO skill_fts (rowid, name, description, symptoms, tags, applies_to)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			rowid, e.Name, e.Description,
			strings.Join(e.Symptoms, "\n"), strings.Join(e.Tags, " "), strings.Join(e.AppliesTo, " ")); err != nil {
			return fmt.Errorf("%w: insert fts %s/%s: %v", ErrIndexUnavailable, repo, e.Slug, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%w: commit reindex %s: %v", ErrIndexUnavailable, repo, err)
	}
	return nil
}

// SearchHit is one search result: identifiers qualified by skill repo, plus
// the description. No skill body is ever returned by search.
type SearchHit struct {
	Repo        string   `json:"repo"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	AppliesTo   []string `json:"-"`
	Score       float64  `json:"score"`
}

var queryToken = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// buildMatchQuery reduces a caller's free-text query to a safe FTS5 MATCH
// expression: every word becomes a quoted token so query-language characters
// in agent-supplied search terms cannot change the query's structure. The
// tokens are bound as a parameter, never interpolated into SQL (SPEC-0007 REQ
// "Database Operation Standards").
func buildMatchQuery(query string) string {
	toks := queryToken.FindAllString(query, -1)
	if len(toks) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(toks))
	for _, t := range toks {
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

// Search runs a full-text query and returns at most limit hits, restricted to
// the named repos (empty means every indexed repo: scoping by serve_to happens
// here, so no caller can read past its own grant). paths, when given, rank a
// skill whose applies_to matches any of them above an otherwise equal skill
// (SPEC-0007 REQ "Search And Retrieval Tools"). Scores follow bm25(); lower is
// better.
func (i *Index) Search(query string, paths []string, limit int, repos []string) ([]SearchHit, error) {
	if i == nil || i.db == nil {
		return nil, fmt.Errorf("%w: not open", ErrIndexUnavailable)
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	match := buildMatchQuery(query)
	if match == "" {
		return nil, nil
	}
	// Over-fetch so applies_to ranking can reorder within the requested limit.
	fetch := limit * 4
	args := []any{match}
	where := "WHERE skill_fts MATCH ?"
	if len(repos) > 0 {
		where += " AND m.repo IN (" + placeholders(len(repos)) + ")"
		for _, r := range repos {
			args = append(args, r)
		}
	}
	args = append(args, fetch)
	rows, err := i.db.Query(
		`SELECT m.repo, m.slug, f.description, m.applies_to, bm25(skill_fts) AS score
	 FROM skill_fts f JOIN skill_meta m ON m.rowid = f.rowid
	 `+where+`
	 ORDER BY score
	 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: search: %v", ErrIndexUnavailable, err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var appliesTo string
		if err := rows.Scan(&h.Repo, &h.Slug, &h.Description, &appliesTo, &h.Score); err != nil {
			return nil, fmt.Errorf("%w: scan: %v", ErrIndexUnavailable, err)
		}
		h.AppliesTo = splitFields(appliesTo)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: rows: %v", ErrIndexUnavailable, err)
	}

	if len(paths) > 0 {
		var matching, rest []SearchHit
		for _, h := range hits {
			if matchesAnyPath(h.AppliesTo, paths) {
				matching = append(matching, h)
			} else {
				rest = append(rest, h)
			}
		}
		hits = append(matching, rest...)
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// placeholders returns n bound-parameter markers, so a repo list reaches
// SQLite as parameters and never as interpolated SQL (SPEC-0007 REQ "Database
// Operation Standards").
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// SkillRef is one active skill's identity, as the MCP resource listing and
// get_skill address it.
type SkillRef struct {
	Repo        string `json:"repo"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

// ListSkills returns every active skill in the named repos (empty means every
// indexed repo), ordered by repo and slug. Only active skills are indexed, so
// this is the set the per-skill MCP resources cover (SPEC-0007 REQ "Search And
// Retrieval Tools").
func (i *Index) ListSkills(repos []string) ([]SkillRef, error) {
	if i == nil || i.db == nil {
		return nil, fmt.Errorf("%w: not open", ErrIndexUnavailable)
	}
	args := []any{}
	where := ""
	if len(repos) > 0 {
		where = "WHERE m.repo IN (" + placeholders(len(repos)) + ")"
		for _, r := range repos {
			args = append(args, r)
		}
	}
	rows, err := i.db.Query(
		`SELECT m.repo, m.slug, f.description
	 FROM skill_meta m JOIN skill_fts f ON f.rowid = m.rowid
	 `+where+`
	 ORDER BY m.repo, m.slug`, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: list skills: %v", ErrIndexUnavailable, err)
	}
	defer rows.Close()

	var out []SkillRef
	for rows.Next() {
		var s SkillRef
		if err := rows.Scan(&s.Repo, &s.Slug, &s.Description); err != nil {
			return nil, fmt.Errorf("%w: list scan: %v", ErrIndexUnavailable, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: list rows: %v", ErrIndexUnavailable, err)
	}
	return out, nil
}

func splitFields(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Fields(s)
}

// matchesAnyPath reports whether any applies_to glob matches one of the
// caller's paths. A glob is tried against the full path and its base name,
// so ".gitea/*" and "ci.yml" both match ".gitea/workflows/ci.yml".
func matchesAnyPath(globs, paths []string) bool {
	for _, g := range globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		for _, p := range paths {
			p = strings.TrimPrefix(strings.TrimSpace(p), "./")
			if p == "" {
				continue
			}
			if ok, err := filepath.Match(g, p); err == nil && ok {
				return true
			}
			if ok, err := filepath.Match(g, filepath.Base(p)); err == nil && ok {
				return true
			}
		}
	}
	return false
}
