package trash

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Bin relocates files into a trash directory (intended to be its own ZFS
// dataset) instead of unlinking them, preserving recoverability and letting
// ZFS snapshots/quotas apply. Files are namespaced by relocation date so a
// TTL purge can remove old entries.
type Bin struct {
	root string
	ttl  time.Duration
}

// New returns a Bin rooted at the given path. The path should be the mountpoint
// of a dedicated ZFS dataset (e.g. tank/trash mounted at /tank/trash).
func New(root string, ttl time.Duration) *Bin {
	return &Bin{root: filepath.Clean(root), ttl: ttl}
}

// Move relocates src into the trash. It returns the destination path. The
// original directory tree under the matched root is preserved beneath a
// date-stamped folder to avoid collisions and enable TTL purging.
//
// originRoot is the library root the file lived under; the portion of src
// relative to originRoot is preserved in the trash layout.
func (b *Bin) Move(src, originRoot string) (string, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to move non-regular file %s", src)
	}

	rel, err := filepath.Rel(originRoot, src)
	if err != nil || strings.HasPrefix(rel, "..") {
		// Fall back to base name if src is not under originRoot.
		rel = filepath.Base(src)
	}

	stamp := time.Now().UTC().Format("2006-01-02")
	dst := filepath.Join(b.root, stamp, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("mkdir trash dir for %s: %w", dst, err)
	}

	// Avoid clobbering an existing trashed file from an earlier run.
	dst = uniquePath(dst)

	if err := os.Rename(src, dst); err != nil {
		// Cross-device (different dataset/pool) rename fails with EXDEV; fall
		// back to copy+remove so relocation still works across mountpoints.
		if isCrossDevice(err) {
			if cerr := copyThenRemove(src, dst); cerr != nil {
				return "", fmt.Errorf("relocate %s -> %s: %w", src, dst, cerr)
			}
			return dst, nil
		}
		return "", fmt.Errorf("rename %s -> %s: %w", src, dst, err)
	}
	return dst, nil
}

// Purge removes date-stamped trash folders older than the configured TTL.
// It returns the number of top-level entries removed.
func (b *Bin) Purge() (int, error) {
	if b.ttl <= 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(b.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read trash root %s: %w", b.root, err)
	}
	cutoff := time.Now().UTC().Add(-b.ttl)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		day, err := time.Parse("2006-01-02", e.Name())
		if err != nil {
			continue // not a date-stamped dir; leave it alone
		}
		// Treat the folder as expiring TTL after the END of its day.
		if day.Add(24 * time.Hour).Before(cutoff) {
			p := filepath.Join(b.root, e.Name())
			if err := os.RemoveAll(p); err != nil {
				return removed, fmt.Errorf("purge %s: %w", p, err)
			}
			removed++
		}
	}
	return removed, nil
}

func uniquePath(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Lstat(cand); errors.Is(err, os.ErrNotExist) {
			return cand
		}
	}
}

func copyThenRemove(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("sync dst: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("close dst: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove src after copy: %w", err)
	}
	return nil
}
