package mergetrain

// VerifyLanded Tests
//
// Governing tests: SPEC-0025 REQ-8; #601 — all match, one of three differs, a
// path missing from the forge, several mismatches sorted, empty want.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/fake"
)

const repo = "stump.wtf/harness"

func landed(t *testing.T, files map[string]string) *fake.Fake {
	t.Helper()
	f := fake.New()
	f.SetBranch(repo, "main", "m1")
	for p, b := range files {
		f.SetFile(repo, "m1", p, []byte(b))
	}
	return f
}

func TestVerifyLandedAllMatch(t *testing.T) {
	f := landed(t, map[string]string{"a": "1", "b/c.go": "2", "d": "3"})
	ok, mis, err := VerifyLanded(context.Background(), f, repo, "main", map[string][]byte{"a": []byte("1"), "b/c.go": []byte("2"), "d": []byte("3")})
	if !ok || len(mis) != 0 || err != nil {
		t.Fatalf("VerifyLanded = %v, %q, %v; want true, [], nil", ok, mis, err)
	}
}

func TestVerifyLandedOneDiffers(t *testing.T) {
	f := landed(t, map[string]string{"a": "1", "b": "old", "c": "3"})
	ok, mis, err := VerifyLanded(context.Background(), f, repo, "main", map[string][]byte{"a": []byte("1"), "b": []byte("new"), "c": []byte("3")})
	if ok || !slices.Equal(mis, []string{"b"}) || err != nil {
		t.Fatalf("VerifyLanded = %v, %q, %v; want false, [b], nil", ok, mis, err)
	}
}

func TestVerifyLandedMissingPath(t *testing.T) {
	f := landed(t, map[string]string{"a": "1"})
	ok, mis, err := VerifyLanded(context.Background(), f, repo, "main", map[string][]byte{"a": []byte("1"), "gone": []byte("x")})
	if ok || !slices.Equal(mis, []string{"gone"}) {
		t.Fatalf("VerifyLanded = %v, %q; want false, [gone]", ok, mis)
	}
	if !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want the read error (ErrNotFound) returned alongside", err)
	}
}

func TestVerifyLandedSortsMismatches(t *testing.T) {
	f := landed(t, map[string]string{"z": "1", "m": "1", "a": "1", "ok": "1"})
	want := map[string][]byte{"z": []byte("2"), "m": []byte("2"), "a": []byte("2"), "ok": []byte("1")}
	for range 5 { // map iteration order varies; the output must not
		_, mis, _ := VerifyLanded(context.Background(), f, repo, "main", want)
		if !slices.Equal(mis, []string{"a", "m", "z"}) {
			t.Fatalf("mismatched = %q, want [a m z]", mis)
		}
	}
}

func TestVerifyLandedEmptyWant(t *testing.T) {
	f := fake.New()
	for _, want := range []map[string][]byte{nil, {}} {
		ok, mis, err := VerifyLanded(context.Background(), f, repo, "main", want)
		if !ok || mis != nil || err != nil {
			t.Fatalf("VerifyLanded(empty) = %v, %v, %v; want true, nil, nil", ok, mis, err)
		}
	}
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("empty want made %d forge calls", n)
	}
}

func TestVerifyLandedEmptyFileIsNotMissing(t *testing.T) {
	f := landed(t, map[string]string{"empty": ""})
	ok, mis, err := VerifyLanded(context.Background(), f, repo, "main", map[string][]byte{"empty": {}})
	if !ok || len(mis) != 0 || err != nil {
		t.Fatalf("an empty file on main = %v, %q, %v; want a match", ok, mis, err)
	}
}
