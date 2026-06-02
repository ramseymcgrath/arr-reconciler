package reconcile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ramseymcgrath/arr-reconciler/internal/arr"
)

// queueGraceKey is a stable identity for a queue item across runs. The download
// client ID survives the queue-record ID churn of season packs; fall back to the
// numeric record ID when no downloadId is present.
func queueGraceKey(instance string, r arr.QueueRecord) string {
	if r.DownloadID != "" {
		return instance + "/" + r.DownloadID
	}
	return fmt.Sprintf("%s/#%d", instance, r.ID)
}

// graceStore tracks, per queue item, how many CONSECUTIVE runs it has looked
// stuck. An item is only eligible for removal once its streak reaches the
// configured GraceRuns, so a transiently stalled download that recovers on its
// own is never removed. The store persists across runs as a sidecar JSON next
// to the state file.
//
// Keys are stable per item: "<instance>/<downloadId>" when a downloadId is
// known (survives the queue-record ID churn of season packs), else the numeric
// record id. Items not seen in a run are dropped, resetting their streak.
type graceStore struct {
	path   string
	Counts map[string]int `json:"counts"` // persisted streaks from prior runs
	next   map[string]int // streaks observed during the current run (not persisted as-is)
}

func loadGraceStore(stateFile string) *graceStore {
	g := &graceStore{Counts: map[string]int{}}
	if stateFile == "" {
		return g
	}
	g.path = filepath.Join(filepath.Dir(stateFile), "queue-grace.json")
	data, err := os.ReadFile(g.path)
	if err != nil {
		return g // missing/unreadable -> start fresh
	}
	var loaded graceStore
	if json.Unmarshal(data, &loaded) == nil && loaded.Counts != nil {
		g.Counts = loaded.Counts
	}
	return g
}

// begin starts a new run's accounting. Streaks for items not seen between begin
// and commit are dropped (reset to zero).
func (g *graceStore) begin() { g.next = map[string]int{} }

// seen increments and returns an item's consecutive-stuck streak this run,
// building on its persisted streak from prior runs.
func (g *graceStore) seen(key string) int {
	g.next[key] = g.Counts[key] + 1
	return g.next[key]
}

// commit replaces the persisted counts with only the items seen this run (so
// recovered/disappeared items reset) and writes the sidecar file.
func (g *graceStore) commit() error {
	g.Counts = g.next
	if g.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, g.path)
}
