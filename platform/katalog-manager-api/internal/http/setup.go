package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/nalet/stube/platform/katalog-manager-api/internal/store"
)

// setupStatus is the response of GET /api/manage/setup/status. The admin UI
// polls this on launch to decide between the first-run wizard and the normal
// console. The shape is part of the first-run contract shared with the UI —
// keep it identical on both sides.
type setupStatus struct {
	Configured bool        `json:"configured"`
	Version    string      `json:"version"`
	Checks     setupChecks `json:"checks"`
}

type setupChecks struct {
	Database bool `json:"database"`
	Kafka    bool `json:"kafka"`
	Library  bool `json:"library"`
}

// setupRequest is the body of POST /api/manage/setup. streamSigningKey is
// optional — when absent the server generates one and persists it.
type setupRequest struct {
	DisplayName      string `json:"displayName"`
	OIDCIssuer       string `json:"oidcIssuer"`
	OIDCClientID     string `json:"oidcClientId"`
	LibraryPath      string `json:"libraryPath"`
	StreamSigningKey string `json:"streamSigningKey,omitempty"`
}

// SetupStatus reports whether first-run setup has completed plus the live
// dependency health checks the wizard surfaces. It never requires auth at the
// router level for the status read — the wizard must be reachable before any
// identity provider is configured (see router wiring).
func (a *API) SetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	configured, err := a.Store.IsConfigured(ctx)
	if err != nil {
		// A missing DB is reported as "not configured" with database=false
		// rather than a 500 — the wizard needs to render to tell the
		// operator the DB isn't reachable yet.
		configured = false
	}

	checks := setupChecks{
		Database: a.Store.Ping(ctx) == nil,
		Kafka:    a.Producer.Ready(),
		Library:  a.libraryConfigured(ctx),
	}

	writeJSON(w, http.StatusOK, setupStatus{
		Configured: configured,
		Version:    a.Version,
		Checks:     checks,
	})
}

// Setup persists the first-run configuration. It generates a stream signing
// key when the operator didn't supply one, then marks the install configured.
// Idempotent: re-running setup updates the stored config (last write wins) and
// keeps the existing signing key unless a new one is supplied.
func (a *API) Setup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.OIDCIssuer = strings.TrimSpace(req.OIDCIssuer)
	req.OIDCClientID = strings.TrimSpace(req.OIDCClientID)
	req.LibraryPath = strings.TrimSpace(req.LibraryPath)

	if req.DisplayName == "" || req.OIDCIssuer == "" || req.OIDCClientID == "" || req.LibraryPath == "" {
		writeErr(w, http.StatusBadRequest,
			"displayName, oidcIssuer, oidcClientId and libraryPath are required")
		return
	}

	key := strings.TrimSpace(req.StreamSigningKey)
	if key == "" {
		generated, err := generateSigningKey()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "could not generate signing key")
			return
		}
		key = generated
	}

	err := a.Store.Save(r.Context(), store.SetupInput{
		DisplayName:      req.DisplayName,
		OIDCIssuer:       req.OIDCIssuer,
		OIDCClientID:     req.OIDCClientID,
		LibraryPath:      req.LibraryPath,
		StreamSigningKey: key,
	})
	if writeStoreErr(w, r, "setup save", err) {
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"configured": true})
}

// libraryConfigured reports whether a library path has been persisted. The
// management plane records the path the operator points at; it does not stat
// the filesystem here (the path lives on a worker-mounted volume that may not
// be visible to this pod).
func (a *API) libraryConfigured(ctx context.Context) bool {
	c, err := a.Store.LoadConfig(ctx)
	if err != nil {
		return false
	}
	return strings.TrimSpace(c.LibraryPath) != ""
}

// generateSigningKey returns a 32-byte random key hex-encoded. Used when the
// operator doesn't supply one during first-run setup.
func generateSigningKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
