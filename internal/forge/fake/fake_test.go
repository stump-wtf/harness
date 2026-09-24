package fake

// Fake Forge Tests
//
// Governing tests: #599 — each method round-trips what was set, the Err hook
// fails exactly the named method, calls are recorded in order, and 8
// goroutines can share one fake under -race.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
)

const repo = "stump.wtf/harness"

var ctx = context.Background()

func pr(n int, head string) forge.PullRequest {
	return forge.PullRequest{
		Number: n, Author: "joestump", HeadSHA: head, CIState: "success", Mergeable: true,
		Reviews: []forge.Review{{Author: "joestump-agent", State: "APPROVED", CommitID: head, SubmittedAt: time.Unix(0, 0)}},
	}
}

func TestRoundTrips(t *testing.T) {
	f := New()
	f.AddPR(repo, pr(2, "h2"))
	f.AddPR(repo, pr(1, "h1"))
	f.SetBranch(repo, "main", "base")
	f.SetStatus(repo, "h1", "failure")
	f.SetFile(repo, "base", "README.md", []byte("hello"))
	f.SetTree(repo, "base", "tree-base")

	prs, err := f.ListOpenPRs(ctx, repo)
	if err != nil || len(prs) != 2 || prs[0].Number != 1 || prs[1].Number != 2 {
		t.Fatalf("ListOpenPRs = %+v, %v; want #1 then #2", prs, err)
	}
	prs[0].Reviews[0].Author = "mutated"
	again, _ := f.ListOpenPRs(ctx, repo)
	if again[0].Reviews[0].Author != "joestump-agent" {
		t.Fatal("ListOpenPRs returned the fake's own review slice")
	}

	if sha, err := f.BranchHead(ctx, repo, "main"); err != nil || sha != "base" {
		t.Fatalf("BranchHead = %q, %v", sha, err)
	}
	if _, err := f.BranchHead(ctx, repo, "nope"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("BranchHead(missing) err = %v, want ErrNotFound", err)
	}

	if st, _ := f.CombinedStatus(ctx, repo, "h1"); st != "failure" {
		t.Fatalf("CombinedStatus(h1) = %q", st)
	}
	if st, _ := f.CombinedStatus(ctx, repo, "unknown"); st != "pending" {
		t.Fatalf("CombinedStatus(unset) = %q, want pending", st)
	}

	if b, err := f.FileContentAtRef(ctx, repo, "README.md", "base"); err != nil || string(b) != "hello" {
		t.Fatalf("FileContentAtRef(sha) = %q, %v", b, err)
	}
	if b, err := f.FileContentAtRef(ctx, repo, "README.md", "main"); err != nil || string(b) != "hello" {
		t.Fatalf("FileContentAtRef(branch) = %q, %v", b, err)
	}
	if _, err := f.FileContentAtRef(ctx, repo, "missing", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("FileContentAtRef(missing) err = %v, want ErrNotFound", err)
	}
	if tree, err := f.TreeOf(ctx, repo, "base"); err != nil || tree != "tree-base" {
		t.Fatalf("TreeOf = %q, %v", tree, err)
	}

	if err := f.Comment(ctx, repo, 1, "first"); err != nil {
		t.Fatal(err)
	}
	_ = f.Comment(ctx, repo, 1, "second")
	if got, _ := f.ListComments(ctx, repo, 1); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("ListComments = %q", got)
	}
}

func TestTrainBranchAndMerge(t *testing.T) {
	f := New()
	f.AddPR(repo, pr(1, "h1"))
	f.SetBranch(repo, "main", "base")
	f.SetFile(repo, "base", "keep.txt", []byte("kept"))

	spec := forge.TrainSpec{Branch: "train/1", BaseSHA: "base", PR: 1, HeadSHA: "h1"}
	tb, err := f.CreateTrainBranch(ctx, repo, spec)
	if err != nil || tb.SHA == "" || tb.Tree == "" {
		t.Fatalf("CreateTrainBranch = %+v, %v", tb, err)
	}
	if got := f.Branches(repo)["train/1"]; got != tb.SHA {
		t.Fatalf("train/1 = %q, want %q", got, tb.SHA)
	}
	if _, err := f.CreateTrainBranch(ctx, repo, spec); err == nil {
		t.Fatal("CreateTrainBranch over an existing branch succeeded")
	}
	f.SetFile(repo, tb.SHA, "new.txt", []byte("added"))

	if _, err := f.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/x", PR: 1, HeadSHA: "old"}); !errors.Is(err, forge.ErrStale) {
		t.Fatalf("stale head err = %v, want ErrStale", err)
	}

	if err := f.DeleteBranch(ctx, repo, "train/1"); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteBranch(ctx, repo, "train/1"); err != nil {
		t.Fatalf("deleting a missing branch = %v, want nil", err)
	}

	if _, err := f.SquashMerge(ctx, repo, 99, "h", "t", "m"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("SquashMerge(unknown) err = %v", err)
	}
	var se *forge.StatusError
	if _, err := f.SquashMerge(ctx, repo, 1, "moved", "t", "m"); !errors.As(err, &se) || se.Status != 409 {
		t.Fatalf("SquashMerge(wrong head) err = %v, want a 409", err)
	}
	merged, err := f.SquashMerge(ctx, repo, 1, "h1", "t", "m")
	if err != nil || merged == "" {
		t.Fatalf("SquashMerge = %q, %v", merged, err)
	}
	if !f.Merged(repo, 1) {
		t.Fatal("PR not marked merged")
	}
	if head, _ := f.BranchHead(ctx, repo, "main"); head != merged {
		t.Fatalf("main = %q, want the merge commit %q", head, merged)
	}
	if tree, _ := f.TreeOf(ctx, repo, merged); tree != tb.Tree {
		t.Fatalf("merged tree = %q, want the train's %q", tree, tb.Tree)
	}
	for path, want := range map[string]string{"keep.txt": "kept", "new.txt": "added"} {
		if b, err := f.FileContentAtRef(ctx, repo, path, "main"); err != nil || string(b) != want {
			t.Fatalf("%s on main = %q, %v; want %q", path, b, err, want)
		}
	}
	if prs, _ := f.ListOpenPRs(ctx, repo); len(prs) != 0 {
		t.Fatalf("merged PR still listed open: %+v", prs)
	}
	if _, err := f.SquashMerge(ctx, repo, 1, "h1", "t", "m"); !errors.As(err, &se) || se.Status != 405 {
		t.Fatalf("SquashMerge twice err = %v, want a 405", err)
	}
}

func TestHooks(t *testing.T) {
	f := New()
	f.AddPR(repo, pr(1, "h1"))
	f.SetBranch(repo, "main", "base")

	f.OnStatus(func(sha string, n int) string {
		if n < 3 {
			return "pending"
		}
		return "success"
	})
	for i, want := range []string{"pending", "pending", "success"} {
		if st, _ := f.CombinedStatus(ctx, repo, "x"); st != want {
			t.Fatalf("poll %d = %q, want %q", i+1, st, want)
		}
	}

	f.OnTrain(func(forge.TrainSpec) (forge.TrainBranch, error) { return forge.TrainBranch{}, forge.ErrConflict })
	if _, err := f.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/1", PR: 1, HeadSHA: "h1"}); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if _, ok := f.Branches(repo)["train/1"]; ok {
		t.Fatal("a conflicting build created a branch")
	}

	f.OnTrain(func(s forge.TrainSpec) (forge.TrainBranch, error) {
		return forge.TrainBranch{SHA: "train-sha", Tree: "T", Changed: []string{"a"}}, nil
	})
	if tb, _ := f.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/1", PR: 1, HeadSHA: "h1"}); tb.SHA != "train-sha" {
		t.Fatalf("OnTrain ignored: %+v", tb)
	}
	f.OnMerge(func(int, forge.TrainBranch) string { return "other-tree" })
	merged, _ := f.SquashMerge(ctx, repo, 1, "h1", "t", "m")
	if tree, _ := f.TreeOf(ctx, repo, merged); tree != "other-tree" {
		t.Fatalf("OnMerge ignored: tree = %q", tree)
	}
}

func TestErrHookIsolatesOneMethod(t *testing.T) {
	f := New()
	f.SetBranch(repo, "main", "base")
	boom := errors.New("boom")
	f.Err("CombinedStatus", boom)

	if _, err := f.CombinedStatus(ctx, repo, "base"); !errors.Is(err, boom) {
		t.Fatalf("CombinedStatus err = %v, want boom", err)
	}
	if _, err := f.BranchHead(ctx, repo, "main"); err != nil {
		t.Fatalf("BranchHead failed too: %v", err)
	}
	if err := f.Comment(ctx, repo, 1, "x"); err != nil {
		t.Fatalf("Comment failed too: %v", err)
	}
	if _, err := f.ListOpenPRs(ctx, repo); err != nil {
		t.Fatalf("ListOpenPRs failed too: %v", err)
	}

	f.Err("CombinedStatus", nil)
	if _, err := f.CombinedStatus(ctx, repo, "base"); err != nil {
		t.Fatalf("cleared hook still failing: %v", err)
	}
	if got := f.Methods(); got[0] != "CombinedStatus" {
		t.Fatalf("a failed call was not recorded: %v", got)
	}
}

func TestCallsRecordedInOrder(t *testing.T) {
	f := New()
	f.AddPR(repo, pr(1, "h1"))
	f.SetBranch(repo, "main", "base")

	_, _ = f.ListOpenPRs(ctx, repo)
	_, _ = f.BranchHead(ctx, repo, "main")
	tb, _ := f.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/1", BaseSHA: "base", PR: 1, HeadSHA: "h1"})
	_, _ = f.CombinedStatus(ctx, repo, tb.SHA)
	_ = f.DeleteBranch(ctx, repo, "train/1")
	_, _ = f.SquashMerge(ctx, repo, 1, "h1", "title", "msg")
	_ = f.Comment(ctx, repo, 1, "c")

	want := []string{"ListOpenPRs", "BranchHead", "CreateTrainBranch", "CombinedStatus", "DeleteBranch", "SquashMerge", "Comment"}
	if got := f.Methods(); !slices.Equal(got, want) {
		t.Fatalf("methods = %v\nwant      %v", got, want)
	}
	calls := f.Calls()
	if last := calls[len(calls)-1]; !slices.Equal(last.Args, []any{repo, 1, "c"}) {
		t.Fatalf("Comment args = %v", last.Args)
	}
	if merge := calls[5]; !slices.Equal(merge.Args, []any{repo, 1, "h1", "title", "msg"}) {
		t.Fatalf("SquashMerge args = %v", merge.Args)
	}
}

func TestConcurrentUse(t *testing.T) {
	f := New()
	f.SetBranch(repo, "main", "base")
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				n := g*1000 + i
				head := fmt.Sprintf("h%d", n)
				f.AddPR(repo, pr(n, head))
				f.SetStatus(repo, head, "success")
				_, _ = f.ListOpenPRs(ctx, repo)
				tb, err := f.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: fmt.Sprintf("train/%d", n), BaseSHA: "base", PR: n, HeadSHA: head})
				if err != nil {
					t.Errorf("CreateTrainBranch(%d): %v", n, err)
					return
				}
				_, _ = f.CombinedStatus(ctx, repo, tb.SHA)
				_ = f.Comment(ctx, repo, n, "c")
				_ = f.DeleteBranch(ctx, repo, fmt.Sprintf("train/%d", n))
				_, _ = f.SquashMerge(ctx, repo, n, head, "t", "m")
			}
		})
	}
	wg.Wait()
	// Six recorded calls per iteration; AddPR and SetStatus are setters.
	if n := len(f.Calls()); n != 8*50*6 {
		t.Fatalf("recorded %d calls, want %d", n, 8*50*6)
	}
	for name := range f.Branches(repo) {
		if name != "main" {
			t.Fatalf("branch %s left behind", name)
		}
	}
}
