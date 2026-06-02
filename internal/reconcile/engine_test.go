package reconcile

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAgeBucketStableUnderDrift(t *testing.T) {
	// Two ages a few minutes apart in the same band must bucket identically, so
	// an otherwise-unchanged payload serializes the same across runs.
	a := ageBucket(50*time.Hour + 3*time.Minute)
	b := ageBucket(50*time.Hour + 47*time.Minute)
	if a != b {
		t.Errorf("same-band ages bucketed differently: %d vs %d", a, b)
	}
	// 50h falls in the daily band -> multiple of 24.
	if a%24 != 0 {
		t.Errorf("daily-band bucket %d is not a multiple of 24", a)
	}
}

func TestAgeBucketBands(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want int64
	}{
		{-5 * time.Hour, 0},               // negative clamps to 0
		{30 * time.Minute, 0},             // <1h -> 0
		{5 * time.Hour, 5},                // hourly within first day
		{24 * time.Hour, 24},              // boundary stays hourly
		{50 * time.Hour, 48},              // daily band: floor to 2 days
		{8 * 24 * time.Hour, 7 * 24},      // weekly band: floor to 1 week
		{20 * 24 * time.Hour, 2 * 7 * 24}, // weekly band: floor to 2 weeks
	}
	for _, c := range cases {
		if got := ageBucket(c.d); got != c.want {
			t.Errorf("ageBucket(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}

func TestDedupe(t *testing.T) {
	in := []string{"a", "", "b", "a", "b", "c", ""}
	got := dedupe(in)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if dedupe(nil) != nil {
		t.Error("dedupe(nil) should be nil")
	}
}

// The whole point of bucketing: identical inputs that differ only by sub-band
// time drift must produce byte-identical JSON (so the gateway cache can hit).
func TestOrphanPayloadDeterministicAcrossDrift(t *testing.T) {
	mk := func(driftMin int) []byte {
		now := time.Now()
		modTime := now.Add(-50*time.Hour - time.Duration(driftMin)*time.Minute)
		cand := orphanCandidate{
			Ref:      "o-0",
			Path:     "/storage/tv/Show/junk.mkv",
			Ext:      ".mkv",
			SizeMB:   1234,
			AgeHours: ageBucket(now.Sub(modTime)),
		}
		raw, err := json.Marshal([]orphanCandidate{cand})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if string(mk(3)) != string(mk(41)) {
		t.Errorf("payload drifted within the same age band:\n %s\n %s", mk(3), mk(41))
	}
}

func TestQueueCandidateOmitsEmpties(t *testing.T) {
	raw, err := json.Marshal(queueCandidate{Ref: "q-1", Title: "X", AgeHours: 5})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// Empty optional fields must be omitted to keep the payload small.
	for _, k := range []string{"status", "error_message", "messages", "protocol", "indexer"} {
		if strings.Contains(s, "\""+k+"\"") {
			t.Errorf("empty field %q should have been omitted: %s", k, s)
		}
	}
	// Required fields always present.
	for _, k := range []string{"ref", "title", "age_hours", "percent_remaining"} {
		if !strings.Contains(s, "\""+k+"\"") {
			t.Errorf("required field %q missing: %s", k, s)
		}
	}
}
