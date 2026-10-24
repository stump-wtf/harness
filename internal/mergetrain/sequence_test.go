package mergetrain

// Sequence Collision Tests
//
// Governing tests: SPEC-0025 REQ-18, ADR-0052: a claim is the first run of
// three or more digits in a file's name, scoped to its directory; a pair the
// base branch already holds is not a verdict; a collision the attempt
// introduces ends the attempt before CI, and names which PR brings each file.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/fake"
)

func TestSequenceClaimParses(t *testing.T) {
	cases := []struct {
		path   string
		dir    string
		number string
		digits string
		ok     bool
	}{
		{"docs/adrs/adr-0051-official-approvals.md", "docs/adrs", "51", "0051", true},
		{"docs/openspec/specs/SPEC-0025-merge-train.md", "docs/openspec/specs", "25", "0025", true},
		{"migrations/0032_add_index.sql", "migrations", "32", "0032", true},
		{"migrations/0000_noop.sql", "migrations", "0", "0000", true},
		{"docs/adrs/adr-7-single-digit.md", "", "", "", false},
		{"docs/adrs/index.md", "", "", "", false},
		{"README.md", "", "", "", false},
	}
	for _, c := range cases {
		dir, number, digits, ok := sequenceClaim(c.path)
		if dir != c.dir || number != c.number || digits != c.digits || ok != c.ok {
			t.Errorf("sequenceClaim(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
				c.path, dir, number, digits, ok, c.dir, c.number, c.digits, c.ok)
		}
	}
}

func TestFindSequenceCollisions(t *testing.T) {
	watched := []string{"docs/adrs/", "migrations/"}
	cases := []struct {
		name  string
		train []string
		base  []string
		want  int
	}{
		{
			name:  "collision between two new files",
			train: []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md"},
			base:  []string{"docs/adrs/adr-0050-a.md"},
			want:  1,
		},
		{
			name:  "near miss",
			train: []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0052-b.md"},
			base:  nil,
			want:  0,
		},
		{
			name:  "same number in different directories",
			train: []string{"docs/adrs/adr-0051-a.md", "migrations/0051_add.sql"},
			base:  nil,
			want:  0,
		},
		{
			name:  "zero padding is the same claim",
			train: []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-51-b.md"},
			base:  nil,
			want:  1,
		},
		{
			name:  "a pair already on base is not a verdict",
			train: []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md", "docs/adrs/adr-0052-c.md"},
			base:  []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md"},
			want:  0,
		},
		{
			name:  "a new file against one on base is",
			train: []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md"},
			base:  []string{"docs/adrs/adr-0051-a.md"},
			want:  1,
		},
		{
			name:  "unwatched paths are ignored",
			train: []string{"internal/adr-0051-a.go", "internal/adr-0051-b.go"},
			base:  nil,
			want:  0,
		},
		{
			name:  "names without a number are ignored",
			train: []string{"docs/adrs/index.md", "docs/adrs/template.md"},
			base:  nil,
			want:  0,
		},
		{
			name:  "two collisions report apart",
			train: []string{"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md", "migrations/0007_a.sql", "migrations/0007_b.sql"},
			base:  nil,
			want:  2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findSequenceCollisions(c.train, c.base, watched)
			if len(got) != c.want {
				t.Fatalf("got %d collisions %v, want %d", len(got), got, c.want)
			}
			for _, col := range got {
				if len(col.Introduced()) == 0 {
					t.Errorf("collision on %s %s introduces nothing: %v", col.Dir, col.Number, col.Claims)
				}
			}
		})
	}
}

func TestFindSequenceCollisionsOrdersByDirThenNumber(t *testing.T) {
	got := findSequenceCollisions([]string{
		"migrations/0009_a.sql", "migrations/0009_b.sql",
		"docs/adrs/adr-0052-a.md", "docs/adrs/adr-0052-b.md",
		"docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md",
	}, nil, []string{"docs/adrs", "migrations"})
	if len(got) != 3 {
		t.Fatalf("got %d collisions, want 3", len(got))
	}
	want := []sequenceClaimKey{{"docs/adrs", "51"}, {"docs/adrs", "52"}, {"migrations", "9"}}
	for i, k := range want {
		if got[i].Dir != k.dir || got[i].Number != k.number {
			t.Errorf("collision %d = %s %s, want %s %s", i, got[i].Dir, got[i].Number, k.dir, k.number)
		}
	}
}

// sequenceWorld is a fake whose base branch holds docs/adrs/adr-0050-old.md
// and whose train commit for pr holds whatever paths the caller lists.
func sequenceWorld(t *testing.T, trainSHA string, paths ...string) (*fake.Fake, PullRequest) {
	t.Helper()
	f := fake.New()
	pr := ready()
	f.AddPR(repo, pr)
	f.SetBranch(repo, "main", "base")
	f.SetFile(repo, "base", "docs/adrs/adr-0050-old.md", []byte("old"))
	files := append([]string{"docs/adrs/adr-0050-old.md"}, paths...)
	f.OnTrain(func(forge.TrainSpec) (forge.TrainBranch, error) {
		for _, p := range files {
			f.SetFile(repo, trainSHA, p, []byte("x"))
		}
		return forge.TrainBranch{SHA: trainSHA, Tree: "T", Changed: paths}, nil
	})
	f.SetStatus(repo, trainSHA, CISuccess)
	return f, pr
}

func TestRunTrainRefusesSequenceCollisionBeforeCI(t *testing.T) {
	f, pr := sequenceWorld(t, trainSHA, "docs/adrs/adr-0051-new.md", "docs/adrs/adr-0051-other.md")
	c := cfg(nil)
	c.SequencePaths = []string{"docs/adrs/"}
	res := RunTrain(context.Background(), f, c, pr, "base")
	if res.Outcome != OutcomeSequenceConflict {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeSequenceConflict)
	}
	if len(res.Collisions) != 1 || res.Collisions[0].Digits != "0051" {
		t.Fatalf("collisions = %v", res.Collisions)
	}
	for _, call := range f.Calls() {
		if call.Method == "CombinedStatus" {
			t.Error("a doomed attempt must not spend a CI run")
		}
	}
	left := 0
	for name := range f.Branches(repo) {
		if strings.HasPrefix(name, "train/") {
			left++
		}
	}
	if left != 0 {
		t.Errorf("train branch left behind: %v", f.Branches(repo))
	}
}

func TestRunTrainPassesWhenNumbersDiffer(t *testing.T) {
	f, pr := sequenceWorld(t, trainSHA, "docs/adrs/adr-0051-new.md")
	c := cfg(nil)
	c.SequencePaths = []string{"docs/adrs/"}
	res := RunTrain(context.Background(), f, c, pr, "base")
	if res.Outcome != OutcomeGreen {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeGreen)
	}
	if len(res.Collisions) != 0 {
		t.Errorf("collisions = %v, want none", res.Collisions)
	}
}

func TestRunTrainWithoutWatchedPathsIsUnchanged(t *testing.T) {
	f, pr := sequenceWorld(t, trainSHA, "docs/adrs/adr-0051-a.md", "docs/adrs/adr-0051-b.md")
	res := RunTrain(context.Background(), f, cfg(nil), pr, "base")
	if res.Outcome != OutcomeGreen {
		t.Fatalf("outcome = %q, want %q: the check is off with no watched paths", res.Outcome, OutcomeGreen)
	}
}

func TestCheckSequencesAttributesBatchHeads(t *testing.T) {
	f := fake.New()
	const base = "base"
	f.SetBranch(repo, "main", base)
	first, second := ready(), ready()
	first.Number, first.HeadSHA = 11, "head11"
	second.Number, second.HeadSHA = 12, "head12"
	f.SetFile(repo, base, "docs/adrs/adr-0050-old.md", []byte("old"))
	f.SetFile(repo, "head11", "docs/adrs/adr-0050-old.md", []byte("old"))
	f.SetFile(repo, "head11", "docs/adrs/adr-0051-first.md", []byte("1"))
	f.SetFile(repo, "head12", "docs/adrs/adr-0050-old.md", []byte("old"))
	f.SetFile(repo, "head12", "docs/adrs/adr-0051-second.md", []byte("2"))
	f.SetFile(repo, "train1112", "docs/adrs/adr-0050-old.md", []byte("old"))
	f.SetFile(repo, "train1112", "docs/adrs/adr-0051-first.md", []byte("1"))
	f.SetFile(repo, "train1112", "docs/adrs/adr-0051-second.md", []byte("2"))
	collisions, err := checkSequences(context.Background(), f, repo, []string{"docs/adrs/"},
		[]PullRequest{first, second}, "train1112", base)
	if err != nil {
		t.Fatalf("checkSequences: %v", err)
	}
	if len(collisions) != 1 {
		t.Fatalf("collisions = %v, want one", collisions)
	}
	got := map[string]int{}
	for _, cl := range collisions[0].Claims {
		if strings.HasSuffix(cl.Path, "adr-0050-old.md") && !cl.OnBase {
			t.Errorf("%s should be marked as already on the base branch", cl.Path)
		}
		got[cl.Path] = cl.PR
	}
	if got["docs/adrs/adr-0051-first.md"] != 11 || got["docs/adrs/adr-0051-second.md"] != 12 {
		t.Errorf("attribution = %v, want first to 11 and second to 12", got)
	}
}

func TestCollisionDetailNamesFilesAndPRs(t *testing.T) {
	detail := CollisionDetail([]SequenceCollision{{
		Dir: "docs/adrs", Number: "51", Digits: "0051",
		Claims: []SequenceClaim{
			{Path: "docs/adrs/adr-0051-a.md", PR: 11},
			{Path: "docs/adrs/adr-0051-b.md", PR: 12},
			{Path: "docs/adrs/adr-0051-c.md", OnBase: true},
		},
	}}, "main")
	for _, want := range []string{"adr-0051-a.md", "PR #11", "PR #12", "already on `main`", "0051"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail omits %q:\n%s", want, detail)
		}
	}
}

func TestDriverCommentsSequenceConflictOncePerHead(t *testing.T) {
	w := newWorld(t, prAt(1, time.Minute))
	w.SetFile(repo, "base0", "docs/adrs/adr-0051-old.md", []byte("old"))
	train := trainFor(1, "base0")
	w.SetFile(repo, train, "docs/adrs/adr-0051-old.md", []byte("old"))
	w.SetFile(repo, train, "docs/adrs/adr-0051-new.md", []byte("new"))
	cfg := driverCfg(t, ModeMerge, &logRec{})
	cfg.SequencePaths = []string{"docs/adrs/"}
	d := newDriver(t, w, cfg)
	ticks(t, d, 2)
	comments := w.Comments(repo, 1)
	if len(comments) != 1 {
		t.Fatalf("PR 1 comments = %d, want 1: %v", len(comments), comments)
	}
	if !strings.Contains(comments[0], CauseSequenceConflict) || !strings.Contains(comments[0], "adr-0051-new.md") {
		t.Errorf("comment is not a sequence-conflict report:\n%s", comments[0])
	}
	if !strings.Contains(comments[0], "already on `main`") {
		t.Errorf("the claim held by the base branch should say so:\n%s", comments[0])
	}
	if w.Merged(repo, 1) {
		t.Error("the colliding PR merged")
	}
	for _, c := range w.Calls() {
		if c.Method == "CombinedStatus" {
			t.Fatal("the train waited for CI on a commit it should have refused")
		}
	}
}
