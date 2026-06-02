package config

import "testing"

func TestWithinAllowedRoot(t *testing.T) {
	s := Safety{}
	s.AllowedRoots = []string{"/tank/media/movies", "/tank/media/tv"}
	// applyDefaults cleans roots; mimic that here.
	for i, r := range s.AllowedRoots {
		s.AllowedRoots[i] = r
	}

	cases := []struct {
		path string
		want bool
	}{
		{"/tank/media/movies/Foo (2020)/foo.mkv", true},
		{"/tank/media/tv/Show/S01/ep.mkv", true},
		{"/tank/media/movies", true},                 // the root itself
		{"/tank/media/music/album/song.flac", false}, // sibling, not allowed
		{"/etc/passwd", false},
		{"/tank/media/movies/../../../etc/passwd", false}, // traversal escapes root
		{"/tank/media/moviesextra/x.mkv", false},          // prefix-string trap, not a child
	}
	for _, c := range cases {
		if got := s.WithinAllowedRoot(c.path); got != c.want {
			t.Errorf("WithinAllowedRoot(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsProtectedExt(t *testing.T) {
	s := Safety{ProtectedExtensions: []string{".nfo", ".srt"}}
	cases := map[string]bool{
		"/x/movie.nfo":  true,
		"/x/movie.SRT":  true, // case-insensitive
		"/x/movie.mkv":  false,
		"/x/movie":      false,
		"/x/movie.nfo2": false,
	}
	for path, want := range cases {
		if got := s.IsProtectedExt(path); got != want {
			t.Errorf("IsProtectedExt(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Claude.Model == "" || c.Claude.MaxTokens == 0 || c.Claude.BaseURL == "" {
		t.Fatal("claude defaults not applied")
	}
	if c.Safety.MaxDeletesPerRun == 0 || c.Safety.MaxBytesPerRun == 0 {
		t.Fatal("safety caps not defaulted")
	}
	if c.StateFile == "" {
		t.Fatal("state file not defaulted")
	}
}

func TestApplyDefaultsNormalizesExtensions(t *testing.T) {
	c := &Config{Safety: Safety{ProtectedExtensions: []string{"NFO", " .srt ", "jpg"}}}
	c.applyDefaults()
	want := []string{".nfo", ".srt", ".jpg"}
	for i, w := range want {
		if c.Safety.ProtectedExtensions[i] != w {
			t.Errorf("ext[%d] = %q, want %q", i, c.Safety.ProtectedExtensions[i], w)
		}
	}
}

func TestValidate(t *testing.T) {
	base := func() *Config {
		return &Config{
			Instances: []Instance{{Name: "s", Kind: "sonarr", BaseURL: "http://x", APIKey: "k"}},
			Claude:    Claude{APIKey: "k"},
		}
	}

	if err := base().validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	noInst := base()
	noInst.Instances = nil
	if err := noInst.validate(); err == nil {
		t.Error("expected error for no instances")
	}

	badKind := base()
	badKind.Instances[0].Kind = "lidarr"
	if err := badKind.validate(); err == nil {
		t.Error("expected error for unsupported kind")
	}

	noClaude := base()
	noClaude.Claude.APIKey = ""
	if err := noClaude.validate(); err == nil {
		t.Error("expected error for missing claude key")
	}

	// delete_enabled requires trash dataset + allowed roots.
	delNoTrash := base()
	delNoTrash.Safety.DeleteEnabled = true
	if err := delNoTrash.validate(); err == nil {
		t.Error("expected error: delete_enabled without trash_dataset")
	}
	delNoRoots := base()
	delNoRoots.Safety.DeleteEnabled = true
	delNoRoots.Safety.TrashDataset = "/tank/trash"
	if err := delNoRoots.validate(); err == nil {
		t.Error("expected error: delete_enabled without allowed_roots")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90m"`)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Std().Minutes() != 90 {
		t.Fatalf("got %v, want 90m", d.Std())
	}
	b, err := d.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"1h30m0s"` {
		t.Fatalf("marshal got %s", b)
	}
}
