package ledger

import (
	"testing"
	"time"
)

// REQ-10 "A slow subscriber does not stall supervision": 1,000 closes, one
// subscriber that never reads, and every close still returns promptly; that
// subscriber's miss count reads 1,000 and every other subscriber got them all.
func TestSlowSubscriberDoesNotStallCloses(t *testing.T) {
	l := openT(t, t.TempDir(), Options{})
	_, cancelStuck := l.Subscribe("stuck", 1)
	defer cancelStuck()
	// Fill the stuck subscriber's one slot first, so every close below is a miss.
	mustAppend(t, l, opened("warm", 1, time.Now()), true)

	got, cancel := l.Subscribe("reader", 4096)
	defer cancel()
	done := make(chan int)
	go func() {
		n := 0
		for c := range got {
			if c.Type == TypeClosed {
				n++
			}
		}
		done <- n
	}()

	start := time.Now()
	for i := 1; i <= 1000; i++ {
		mustAppend(t, l, closed("busy", i, time.Now(), "success"), true)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("1,000 synced closes took %s behind a subscriber that never reads", took)
	}
	if d := l.FeedDropped()["stuck"]; d != 1000 {
		t.Errorf("stuck subscriber's misses = %d, want 1000", d)
	}
	cancel()
	if n := <-done; n != 1000 {
		t.Errorf("the reading subscriber got %d closes, want 1000", n)
	}
	if d := l.FeedDropped()["reader"]; d != 0 {
		t.Errorf("reader missed %d", d)
	}
}

// REQ-10 "A lossless consumer resumes": after a restart, Since(500) yields
// 501 onward, in order, including lines written while the consumer was down.
func TestSinceResumesAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	for i := 1; i <= 800; i++ {
		mustAppend(t, l, opened("h", i, time.Now()), i%100 == 0)
	}
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	l2 := openT(t, dir, Options{})
	for i := 801; i <= 900; i++ { // written after the restart: served from the ring
		mustAppend(t, l2, opened("h", i, time.Now()), i == 900)
	}
	want := uint64(501)
	for ln, err := range l2.Since(500) {
		if err != nil {
			t.Fatal(err)
		}
		if ln.Seq != want {
			t.Fatalf("got seq %d, want %d: out of order or a gap", ln.Seq, want)
		}
		want++
	}
	if want != 901 {
		t.Errorf("Since(500) ended at %d, want through 900", want-1)
	}
	// And from inside the ring.
	n := 0
	for ln := range l2.Since(890) {
		if ln.Seq <= 890 {
			t.Fatalf("seq %d at or below the cursor", ln.Seq)
		}
		n++
	}
	if n != 10 {
		t.Errorf("Since(890) yielded %d lines, want 10", n)
	}
}
