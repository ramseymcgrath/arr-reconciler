package trash

import (
	"errors"
	"syscall"
)

// isCrossDevice reports whether err is the EXDEV ("cross-device link") error
// returned by rename(2) when source and destination live on different
// filesystems (e.g. trash is a separate ZFS pool from the media). In that case
// Bin.Move falls back to copy-then-remove instead of an atomic rename.
func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
