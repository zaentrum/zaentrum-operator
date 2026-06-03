// Package updates implements Stage-2 release-channel discovery for the Stube
// operator. It fetches the published releases.json (a small static document
// served over HTTP) and resolves a Stube spec.channel to the image tag that
// channel currently points at.
//
// The document shape is:
//
//	{
//	  "channels": { "stable": "<tag>", "edge": "<tag>" },
//	  "versions": { "<tag>": { "released": "<date>", "notes": "<text>" } }
//	}
//
// Discovery is intentionally best-effort: a network or parse failure must never
// block reconciliation. Callers treat a returned error as "no new information"
// and leave status.availableUpdate untouched.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultReleasesURL is the canonical published channel document. It is also
// the default the controller falls back to when RELEASES_URL is unset.
const DefaultReleasesURL = "https://raw.githubusercontent.com/nalet/stube/main/releases.json"

// fetchTimeout bounds a single releases.json fetch so a slow or hung endpoint
// can never stall a reconcile.
const fetchTimeout = 10 * time.Second

// maxBody caps the response body we will read. The document is a few hundred
// bytes; this guards against a misconfigured URL streaming an unbounded body.
const maxBody = 1 << 20 // 1 MiB

// VersionInfo is the per-tag metadata recorded in the channel document.
type VersionInfo struct {
	Released string `json:"released,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// Releases is the parsed channel document.
type Releases struct {
	Channels map[string]string      `json:"channels"`
	Versions map[string]VersionInfo `json:"versions,omitempty"`
}

// Resolve returns the image tag the named channel points at. The channel name
// is matched case-insensitively against the document's "channels" map. It
// returns an error when the channel is absent or maps to an empty tag.
func (r Releases) Resolve(channel string) (string, error) {
	if r.Channels == nil {
		return "", fmt.Errorf("releases document has no channels")
	}
	want := strings.ToLower(strings.TrimSpace(channel))
	for name, tag := range r.Channels {
		if strings.ToLower(strings.TrimSpace(name)) == want {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				return "", fmt.Errorf("channel %q maps to an empty tag", channel)
			}
			return tag, nil
		}
	}
	return "", fmt.Errorf("channel %q not found in releases document", channel)
}

// Client fetches and parses the channel document. The zero value is usable and
// fetches with a default-configured http.Client; set HTTP to inject a custom
// transport (e.g. in tests).
type Client struct {
	// HTTP is the transport used for fetches. When nil a package-default
	// client with a bounded timeout is used.
	HTTP *http.Client
}

// Fetch retrieves and parses the channel document at url. It is safe to call
// from a reconcile loop: it honours ctx cancellation and bounds both the
// request time and the response size.
func (c Client) Fetch(ctx context.Context, url string) (Releases, error) {
	if strings.TrimSpace(url) == "" {
		url = DefaultReleasesURL
	}

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: fetchTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Releases{}, fmt.Errorf("build releases request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return Releases{}, fmt.Errorf("fetch releases %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Releases{}, fmt.Errorf("fetch releases %s: unexpected status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Releases{}, fmt.Errorf("read releases body: %w", err)
	}

	var rel Releases
	if err := json.Unmarshal(body, &rel); err != nil {
		return Releases{}, fmt.Errorf("parse releases json: %w", err)
	}
	if len(rel.Channels) == 0 {
		return Releases{}, fmt.Errorf("releases document defines no channels")
	}
	return rel, nil
}

// EffectiveTag chooses the image tag to render this pass given the pinned
// spec.version and the channel target tag.
//
//   - A pinned version (non-empty and not "latest") always wins: an operator
//     who pins a concrete tag has opted out of channel tracking.
//   - Otherwise the channel target is used. When the channel target is empty
//     (discovery failed) we fall back to the spec version (or "latest").
func EffectiveTag(specVersion, channelTarget string) string {
	v := strings.TrimSpace(specVersion)
	if v != "" && v != "latest" {
		return v
	}
	if t := strings.TrimSpace(channelTarget); t != "" {
		return t
	}
	if v != "" {
		return v
	}
	return "latest"
}

// IsPinned reports whether spec.version pins a concrete tag (opts out of
// channel tracking).
func IsPinned(specVersion string) bool {
	v := strings.TrimSpace(specVersion)
	return v != "" && v != "latest"
}
