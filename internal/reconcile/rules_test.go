package reconcile

import "testing"

func TestClassifyOrphanRule(t *testing.T) {
	cases := []struct {
		path string
		size int64
		want ruleVerdict
	}{
		// Obvious junk sidecars -> rule junk (still rail-gated downstream).
		{"/storage/tv/Show/ep.metathumb", 0, ruleJunk},
		{"/storage/tv/Show/ep.metathumb", 1024, ruleJunk},
		{"/storage/tv/Show/ep.xml", 0, ruleJunk},
		// Sample clip.
		{"/storage/movies/Movie/sample.mkv", 50 << 20, ruleJunk},
		{"/storage/movies/Movie/Movie-sample.mp4", 10 << 20, ruleJunk},
		// A large file named with 'sample' is NOT auto-junked (could be real).
		{"/storage/movies/Sample Size (2024)/movie.mkv", 8 << 30, ruleEscalate},
		// Real media -> escalate (let the model/Claude judge).
		{"/storage/tv/Show/episode.mkv", 2 << 30, ruleEscalate},
		// Unknown extension -> escalate.
		{"/storage/tv/Show/weird.dat", 1234, ruleEscalate},
	}
	for _, c := range cases {
		if got := classifyOrphanRule(c.path, c.size); got != c.want {
			t.Errorf("classifyOrphanRule(%q, %d) = %d, want %d", c.path, c.size, got, c.want)
		}
	}
}
