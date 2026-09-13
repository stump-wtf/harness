package sessionguard

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// newStore builds a crush-shaped store (messages with role/parts/created_at,
// created_at in unix seconds) at t.TempDir()/.crush/crush.db and returns its
// path.
func newStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".crush"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, ".crush", "crush.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, parts TEXT NOT NULL DEFAULT '[]', model TEXT, created_at INTEGER, updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return dbPath
}

func addMsg(t *testing.T, dbPath, role, parts string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO messages (role, parts, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		role, parts, at.Unix(), at.Unix()); err != nil {
		t.Fatal(err)
	}
}

const ctxErr = `["{\"type\":\"finish\",\"data\":{\"reason\":\"error\",\"message\":\"Bad Request\",\"details\":\"Prompt exceeds max length\"}}"]`
const okTurn = `["{\"type\":\"text\",\"text\":\"done\"}"]`

func TestStorePath(t *testing.T) {
	if got := StorePath(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Fatalf("missing store: got %q, want empty", got)
	}
	db := newStore(t)
	if got := StorePath(filepath.Dir(filepath.Dir(db))); got != db {
		t.Fatalf("got %q, want %q", got, db)
	}
}

func TestStalledAllTurnsAreContextErrors(t *testing.T) {
	db := newStore(t)
	now := time.Now()
	addMsg(t, db, "assistant", ctxErr, now.Add(-2*time.Minute))
	addMsg(t, db, "assistant", ctxErr, now.Add(-1*time.Minute))

	got, err := Stalled(db, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stalled || got.Turns != 2 || got.Errors != 2 {
		t.Fatalf("got %+v, want stalled with 2/2 errors", got)
	}
}

// The healthy shape: the same window holding ordinary successful turns must
// NOT read as stalled. A guard written under the failure's blind spot would
// flag any error ever; the shape matters, not the presence.
func TestStalledHealthyTurns(t *testing.T) {
	db := newStore(t)
	now := time.Now()
	addMsg(t, db, "assistant", okTurn, now.Add(-2*time.Minute))
	addMsg(t, db, "assistant", okTurn, now.Add(-1*time.Minute))

	got, err := Stalled(db, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stalled || got.Turns != 2 || got.Errors != 0 {
		t.Fatalf("got %+v, want healthy with 0 errors", got)
	}
}

// A single context error followed by a successful turn means the session
// recovered (transient provider error, or a compact happened) — not stalled.
func TestStalledMixedTurns(t *testing.T) {
	db := newStore(t)
	now := time.Now()
	addMsg(t, db, "assistant", ctxErr, now.Add(-2*time.Minute))
	addMsg(t, db, "assistant", okTurn, now.Add(-1*time.Minute))

	got, err := Stalled(db, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stalled {
		t.Fatalf("got %+v, want not stalled (session recovered)", got)
	}
}

// An idle harness must be healthy: silence is not failure, and flagging an
// idle worker would rotate working sessions for nothing.
func TestStalledIdleStore(t *testing.T) {
	db := newStore(t)
	now := time.Now()
	addMsg(t, db, "assistant", ctxErr, now.Add(-2*time.Hour))

	got, err := Stalled(db, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stalled || got.Turns != 0 {
		t.Fatalf("got %+v, want no recent turns", got)
	}
}

// User-role messages never count: only assistant turns answer, so a burst of
// inbound events with no replies yet must not stall the guard.
func TestStalledIgnoresUserRole(t *testing.T) {
	db := newStore(t)
	now := time.Now()
	addMsg(t, db, "user", `["{\"type\":\"text\",\"text\":\"hello\"}"]`, now.Add(-1*time.Minute))

	got, err := Stalled(db, 15*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stalled || got.Turns != 0 {
		t.Fatalf("got %+v, want user turns ignored", got)
	}
}

func TestStalledMissingStore(t *testing.T) {
	_, err := Stalled(filepath.Join(t.TempDir(), "absent.db"), time.Minute, time.Now())
	if err == nil {
		t.Fatal("want error for missing store")
	}
}

func TestArchive(t *testing.T) {
	db := newStore(t)
	now := time.Now()
	// A hot store carries WAL/SHM siblings; all three must move together.
	for _, sib := range []string{db + "-wal", db + "-shm"} {
		if err := os.WriteFile(sib, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	archived, err := Archive(db, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("store still present after archive (err=%v)", err)
	}
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("archive missing: %v", archived)
	}
	for _, sib := range []string{archived + "-wal", archived + "-shm"} {
		if _, err := os.Stat(sib); err != nil {
			t.Fatalf("sibling not archived: %v", sib)
		}
	}
}
