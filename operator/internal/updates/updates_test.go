package updates

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolve(t *testing.T) {
	rel := Releases{Channels: map[string]string{
		"stable": "v1.2.0",
		"edge":   "v1.3.0-rc1",
	}}

	tests := []struct {
		name    string
		channel string
		want    string
		wantErr bool
	}{
		{"stable", "stable", "v1.2.0", false},
		{"edge", "edge", "v1.3.0-rc1", false},
		{"case-insensitive", "STABLE", "v1.2.0", false},
		{"trims whitespace", "  edge ", "v1.3.0-rc1", false},
		{"unknown channel", "beta", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rel.Resolve(tt.channel)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q): expected error, got %q", tt.channel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): unexpected error: %v", tt.channel, err)
			}
			if got != tt.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tt.channel, got, tt.want)
			}
		})
	}
}

func TestResolveEmptyTagIsError(t *testing.T) {
	rel := Releases{Channels: map[string]string{"stable": "   "}}
	if _, err := rel.Resolve("stable"); err == nil {
		t.Fatal("expected error for empty channel tag")
	}
}

func TestResolveNoChannels(t *testing.T) {
	var rel Releases
	if _, err := rel.Resolve("stable"); err == nil {
		t.Fatal("expected error when channels map is nil")
	}
}

func TestEffectiveTag(t *testing.T) {
	tests := []struct {
		spec, channel, want string
	}{
		{"", "v1.2.0", "v1.2.0"},          // unpinned -> channel
		{"latest", "v1.2.0", "v1.2.0"},    // latest is not a pin -> channel
		{"v0.9.0", "v1.2.0", "v0.9.0"},    // pinned wins over channel
		{"", "", "latest"},                // no info -> latest
		{"latest", "", "latest"},          // discovery failed -> latest
		{"  v1.0.0 ", "v2.0.0", "v1.0.0"}, // trims pin
	}
	for _, tt := range tests {
		if got := EffectiveTag(tt.spec, tt.channel); got != tt.want {
			t.Errorf("EffectiveTag(%q,%q) = %q, want %q", tt.spec, tt.channel, got, tt.want)
		}
	}
}

func TestIsPinned(t *testing.T) {
	pinned := []string{"v1.0.0", "1.2.3", "sha-abc"}
	unpinned := []string{"", "  ", "latest", " latest "}
	for _, v := range pinned {
		if !IsPinned(v) {
			t.Errorf("IsPinned(%q) = false, want true", v)
		}
	}
	for _, v := range unpinned {
		if IsPinned(v) {
			t.Errorf("IsPinned(%q) = true, want false", v)
		}
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name          string
		spec          string
		auto          bool
		channel       string
		wantRender    string
		wantAvailable string
		wantApplied   bool
	}{
		{
			name:       "pinned ignores channel and never updates",
			spec:       "v1.0.0",
			auto:       true,
			channel:    "v2.0.0",
			wantRender: "v1.0.0",
		},
		{
			name:          "manual surfaces newer channel target",
			spec:          "latest",
			auto:          false,
			channel:       "v2.0.0",
			wantRender:    "latest",
			wantAvailable: "v2.0.0",
		},
		{
			name:        "auto rolls channel target this pass",
			spec:        "latest",
			auto:        true,
			channel:     "v2.0.0",
			wantRender:  "v2.0.0",
			wantApplied: true,
		},
		{
			name:       "manual with channel == latest is no-op",
			spec:       "latest",
			auto:       false,
			channel:    "latest",
			wantRender: "latest",
		},
		{
			name:       "auto with channel == latest renders latest, applied",
			spec:       "",
			auto:       true,
			channel:    "latest",
			wantRender: "latest",
			// channel target ("latest") == render tag, but auto always rolls
			// the target, so Applied is set; availableUpdate stays empty.
			wantApplied: true,
		},
		{
			name:       "discovery failed falls back to latest, no update",
			spec:       "",
			auto:       false,
			channel:    "",
			wantRender: "latest",
		},
		{
			name:       "discovery failed in auto falls back to latest",
			spec:       "latest",
			auto:       true,
			channel:    "",
			wantRender: "latest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Decide(tt.spec, tt.auto, tt.channel)
			if d.RenderTag != tt.wantRender {
				t.Errorf("RenderTag = %q, want %q", d.RenderTag, tt.wantRender)
			}
			if d.AvailableUpdate != tt.wantAvailable {
				t.Errorf("AvailableUpdate = %q, want %q", d.AvailableUpdate, tt.wantAvailable)
			}
			if d.Applied != tt.wantApplied {
				t.Errorf("Applied = %v, want %v", d.Applied, tt.wantApplied)
			}
		})
	}
}

func TestFetch(t *testing.T) {
	const body = `{
	  "channels": {"stable": "v1.2.0", "edge": "v1.3.0-rc1"},
	  "versions": {"v1.2.0": {"released": "2026-06-01", "notes": "ga"}}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client()}
	rel, err := c.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: unexpected error: %v", err)
	}
	tag, err := rel.Resolve("stable")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tag != "v1.2.0" {
		t.Fatalf("Resolve(stable) = %q, want v1.2.0", tag)
	}
	if rel.Versions["v1.2.0"].Notes != "ga" {
		t.Fatalf("version notes = %q, want ga", rel.Versions["v1.2.0"].Notes)
	}
}

func TestFetchNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client()}
	if _, err := c.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestFetchEmptyChannelsIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"channels": {}}`))
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client()}
	if _, err := c.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error when document defines no channels")
	}
}

func TestFetchBadJSONIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	}))
	defer srv.Close()

	c := Client{HTTP: srv.Client()}
	if _, err := c.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on malformed json")
	}
}
