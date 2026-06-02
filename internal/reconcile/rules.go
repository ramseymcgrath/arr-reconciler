package reconcile

import (
	"path/filepath"
	"strings"
)

// Tier-0 deterministic rules. These classify only NARROW, unambiguous cases
// without consulting any model. A rule may shortcut to:
//   - ruleKeep:     leave it alone, do not even send it onward.
//   - ruleJunk:     obvious removable leftover. NOTE: this still passes through
//     every hard safety rail (allowlist, protected ext, min-age,
//     per-run caps) before anything is moved — the rule only
//     decides "no model needed", never "bypass the rails".
//   - ruleEscalate: not obvious; hand to the next tier (local model / Claude).
//
// Rules must stay conservative: when in any doubt, escalate. They exist to take
// the dead-obvious bulk (e.g. zero-byte .metathumb/.xml sidecars, sample files)
// off the model's plate, not to make subtle calls.
type ruleVerdict int

const (
	ruleEscalate ruleVerdict = iota
	ruleKeep
	ruleJunk
)

// junkExtensions are sidecar/metadata types that, when orphaned (no tracked
// media references them), are safe leftovers. Lowercase, with dot. These are
// only ever acted on after the hard rails pass.
var junkExtensions = map[string]bool{
	".metathumb": true,
	".xml":       true, // arr/Plex metadata sidecars (NFO is protected separately)
}

// classifyOrphanRule applies tier-0 rules to an orphan candidate. size is the
// file size in bytes; name is the base filename.
func classifyOrphanRule(path string, size int64) ruleVerdict {
	name := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))

	// Zero-byte files of any non-media kind are stray artifacts.
	if size == 0 && junkExtensions[ext] {
		return ruleJunk
	}
	// Orphaned metadata sidecars (any size) of the known junk types.
	if junkExtensions[ext] {
		return ruleJunk
	}
	// Obvious sample files: tiny clips left behind by some release groups.
	if strings.Contains(name, "sample") && isVideoExt(ext) && size < 300<<20 {
		return ruleJunk
	}
	// Everything else needs judgement.
	return ruleEscalate
}

func isVideoExt(ext string) bool {
	switch ext {
	case ".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv", ".ts", ".m2ts":
		return true
	}
	return false
}
