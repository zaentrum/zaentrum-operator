package http

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nalet/stube/platform/katalog-manager-api/internal/store"
)

// GetConfig returns the current non-secret configuration. The stream signing
// key is never included — it is a secret and lives only in the DB and the
// stream plane's environment.
func (a *API) GetConfig(w http.ResponseWriter, r *http.Request) {
	c, err := a.Store.LoadConfig(r.Context())
	if writeStoreErr(w, r, "load config", err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// configUpdate is the body of PUT /api/manage/config. Only the editable
// non-secret fields are accepted; `configured` is server-owned (set by
// first-run setup) and is ignored if sent.
type configUpdate struct {
	DisplayName  string `json:"displayName"`
	OIDCIssuer   string `json:"oidcIssuer"`
	OIDCClientID string `json:"oidcClientId"`
	LibraryPath  string `json:"libraryPath"`
}

// UpdateConfig applies a non-secret config update. It will not run before
// first-run setup has created the config row — returns 409 in that case so
// the UI can route the operator back to the wizard.
func (a *API) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	var req configUpdate
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

	err := a.Store.UpdateConfig(r.Context(), store.Config{
		Configured:   true,
		DisplayName:  req.DisplayName,
		OIDCIssuer:   req.OIDCIssuer,
		OIDCClientID: req.OIDCClientID,
		LibraryPath:  req.LibraryPath,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusConflict, "not yet configured; run first-run setup")
		return
	}
	if writeStoreErr(w, r, "update config", err) {
		return
	}

	// Echo back the persisted config so the UI doesn't need a follow-up GET.
	c, err := a.Store.LoadConfig(r.Context())
	if writeStoreErr(w, r, "reload config", err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}
