package reconcile

import (
	"path/filepath"
	"testing"

	"github.com/ramseymcgrath/arr-reconciler/internal/arr"
)

func TestGraceStoreStreaksAcrossRuns(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")

	// Run 1: item seen once.
	g := loadGraceStore(state)
	g.begin()
	if got := g.seen("sonarr/abc"); got != 1 {
		t.Fatalf("run1 streak = %d, want 1", got)
	}
	if err := g.commit(); err != nil {
		t.Fatal(err)
	}

	// Run 2: same item -> streak 2 (persisted across the reload).
	g = loadGraceStore(state)
	g.begin()
	if got := g.seen("sonarr/abc"); got != 2 {
		t.Fatalf("run2 streak = %d, want 2", got)
	}
	if err := g.commit(); err != nil {
		t.Fatal(err)
	}

	// Run 3: item NOT seen (recovered) -> its streak resets. A different item
	// starts fresh at 1.
	g = loadGraceStore(state)
	g.begin()
	if got := g.seen("sonarr/other"); got != 1 {
		t.Fatalf("new item streak = %d, want 1", got)
	}
	if err := g.commit(); err != nil {
		t.Fatal(err)
	}

	// Run 4: the original item reappears -> back to 1 (streak was reset in run 3).
	g = loadGraceStore(state)
	g.begin()
	if got := g.seen("sonarr/abc"); got != 1 {
		t.Fatalf("reset item streak = %d, want 1", got)
	}
}

func TestQueueGraceKey(t *testing.T) {
	withDL := arr.QueueRecord{ID: 5, DownloadID: "HASH123"}
	if got := queueGraceKey("sonarr", withDL); got != "sonarr/HASH123" {
		t.Errorf("downloadId key = %q", got)
	}
	noDL := arr.QueueRecord{ID: 5}
	if got := queueGraceKey("sonarr", noDL); got != "sonarr/#5" {
		t.Errorf("fallback key = %q", got)
	}
}

func TestGraceStoreNoStateFileIsInMemory(t *testing.T) {
	g := loadGraceStore("") // no path
	g.begin()
	g.seen("x")
	if err := g.commit(); err != nil {
		t.Errorf("commit with no state file should be a no-op, got %v", err)
	}
}

func TestGraceStoreCommitCreatesDir(t *testing.T) {
	// state file in a not-yet-existing subdir
	state := filepath.Join(t.TempDir(), "sub", "dir", "state.json")
	g := loadGraceStore(state)
	g.begin()
	g.seen("sonarr/x")
	if err := g.commit(); err != nil {
		t.Fatalf("commit should create the dir, got %v", err)
	}
	// reload proves it persisted
	g2 := loadGraceStore(state)
	g2.begin()
	if got := g2.seen("sonarr/x"); got != 2 {
		t.Fatalf("streak not persisted across dir creation: %d", got)
	}
}
