package http

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		path      string
		wantType  string
		wantTitle string
	}{
		{"/library/movies/Blade Runner (1982)/Blade Runner (1982).mkv", "movie", "Blade Runner"},
		{"/library/movies/Arrival.2016.1080p.mkv", "movie", "Arrival 1080p"},
		{"/library/shows/Foundation/Season 1/Foundation.S01E03.mkv", "episode", "Foundation"},
		{"/library/shows/Show/show.s2e10.web.mp4", "episode", "show web"},
	}
	for _, c := range cases {
		gotType, gotTitle := classify(c.path)
		if gotType != c.wantType {
			t.Errorf("classify(%q) type = %q, want %q", c.path, gotType, c.wantType)
		}
		if gotTitle != c.wantTitle {
			t.Errorf("classify(%q) title = %q, want %q", c.path, gotTitle, c.wantTitle)
		}
	}
}

func TestWithinRoot(t *testing.T) {
	root := "/library"
	cases := []struct {
		candidate string
		wantOK    bool
	}{
		{"/library/movies", true},
		{"/library", true},
		{"movies/action", true},     // relative -> joined under root
		{"/library/../etc", false},  // escapes root
		{"/etc/passwd", false},      // outside root
		{"/library2/sneaky", false}, // prefix-but-not-child
	}
	for _, c := range cases {
		_, ok := withinRoot(root, c.candidate)
		if ok != c.wantOK {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", root, c.candidate, ok, c.wantOK)
		}
	}
}

func TestGenerateSigningKey(t *testing.T) {
	k1, err := generateSigningKey()
	if err != nil {
		t.Fatalf("generateSigningKey: %v", err)
	}
	if len(k1) != 64 { // 32 bytes hex-encoded
		t.Errorf("key length = %d, want 64", len(k1))
	}
	k2, _ := generateSigningKey()
	if k1 == k2 {
		t.Error("two generated keys should differ")
	}
}
