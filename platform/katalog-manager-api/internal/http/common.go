package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/config"
	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/events"
	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/k8s"
	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/keycloak"
	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/store"
)

// API bundles the dependencies every handler needs. One value is shared
// across all handler methods so the router stays a thin wiring layer.
type API struct {
	Cfg      config.Config
	Store    *store.Store
	Producer *events.Producer
	// K8s propagates first-run config + the generated signing key out to the
	// runtime objects sibling services read at startup (the stube-env
	// ConfigMap, the stube-stream-signing Secret) and rolls the affected
	// Deployments. It is never nil: outside a cluster it is a no-op client.
	K8s *k8s.Client
	// Keycloak is the Admin REST client backing the /api/manage/users surface
	// and the first-run bundled-admin bootstrap. It is never nil: when the
	// integration is unconfigured it is a disabled client whose methods return
	// keycloak.ErrDisabled (mapped to 503).
	Keycloak *keycloak.Client
	// Version is the build/version string reported by setup status.
	Version string
}

// writeJSON marshals v as JSON with the given status. Single place for the
// encode-failure log so handlers stay terse.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("response encode failed", "err", err)
	}
}

// writeErr writes a JSON error envelope so the admin UI can show a message
// instead of parsing plain text.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON reads the request body into v, returning false (and writing a
// 400) when the body is missing or malformed.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeErr(w, http.StatusBadRequest, "request body required")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// writeStoreErr collapses the {ErrNotFound, ErrNoPool, generic} fan-out into
// one helper. Returns true when it handled the error (caller must return).
func writeStoreErr(w http.ResponseWriter, r *http.Request, op string, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNoPool):
		writeErr(w, http.StatusServiceUnavailable, "database not configured")
	default:
		slog.Error("store call failed", "op", op, "path", r.URL.Path, "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
	return true
}
