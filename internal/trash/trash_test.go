package trash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMovePreservesLayoutAndRemovesOriginal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	trashRoot := filepath.Join(dir, "trash")
	src := filepath.Join(root, "movies", "Foo (2020)", "foo.mkv")

	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := New(trashRoot, time.Hour)
	dst, err := b.Move(src, root)
	if err != nil {
		t.Fatalf("move: %v", err)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("original still present after move")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("trashed file missing: %v", err)
	}
	// Layout under root ("movies/Foo (2020)/foo.mkv") should be preserved.
	if !strings.HasSuffix(dst, filepath.Join("movies", "Foo (2020)", "foo.mkv")) {
		t.Errorf("layout not preserved: %s", dst)
	}
}

func TestMoveDoesNotClobber(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	trashRoot := filepath.Join(dir, "trash")
	b := New(trashRoot, time.Hour)

	mk := func() string {
		p := filepath.Join(root, "x.mkv")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("v"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	d1, err := b.Move(mk(), root)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := b.Move(mk(), root)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatalf("second move clobbered first: both %s", d1)
	}
}

func TestMoveRefusesNonRegular(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "media", "adir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := New(filepath.Join(dir, "trash"), time.Hour)
	if _, err := b.Move(subdir, filepath.Join(dir, "media")); err == nil {
		t.Fatal("expected refusal to move a directory")
	}
}

func TestPurgeRemovesExpiredOnly(t *testing.T) {
	dir := t.TempDir()
	trashRoot := filepath.Join(dir, "trash")

	old := filepath.Join(trashRoot, "2000-01-01")
	recent := time.Now().UTC().Format("2006-01-02")
	keep := filepath.Join(trashRoot, recent)
	notDate := filepath.Join(trashRoot, "keep-me")

	for _, p := range []string{old, keep, notDate} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	b := New(trashRoot, 24*time.Hour)
	n, err := b.Purge()
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired dir not purged")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("today's dir wrongly purged")
	}
	if _, err := os.Stat(notDate); err != nil {
		t.Error("non-date dir wrongly purged")
	}
}

func TestPurgeZeroTTLIsNoop(t *testing.T) {
	dir := t.TempDir()
	trashRoot := filepath.Join(dir, "trash")
	if err := os.MkdirAll(filepath.Join(trashRoot, "2000-01-01"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := New(trashRoot, 0)
	n, err := b.Purge()
	if err != nil || n != 0 {
		t.Fatalf("expected noop, got n=%d err=%v", n, err)
	}
}
