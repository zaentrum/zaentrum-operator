package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
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

// setupResponse is the body of a successful POST /api/manage/setup. configured
// is always true on success. propagation reports the best-effort push of the
// persisted config + generated key out to the cluster objects sibling services
// read at startup. Setup succeeds even when propagation is partial or skipped —
// the operator can re-run setup or roll the deployments manually.
type setupResponse struct {
	Configured  bool            `json:"configured"`
	Propagation propagationInfo `json:"propagation"`
}

// propagationInfo summarises what the config push did. Applied lists the
// objects actually patched/restarted; Skipped explains a no-op (e.g. running
// outside a cluster, or nothing changed). Errors carries best-effort failures
// so the UI can warn without failing the whole setup.
type propagationInfo struct {
	// Enabled is false when running outside a cluster (no ServiceAccount
	// token); in that case nothing is patched and that is not an error.
	Enabled  bool     `json:"enabled"`
	Applied  []string `json:"applied"`
	Restarts []string `json:"restarts"`
	Skipped  []string `json:"skipped,omitempty"`
	Errors   []string `json:"errors,omitempty"`
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

	ctx := r.Context()

	// Snapshot the current persisted state BEFORE saving so we can compute
	// exactly what changed and only patch/restart for real changes. A missing
	// row (fresh install) yields a zero Config / empty key, so everything is
	// treated as new — which is correct for first run.
	prevCfg, _ := a.Store.LoadConfig(ctx)
	prevKey, _ := a.Store.StreamSigningKey(ctx)

	// Resolve the signing key. If the operator supplied one, use it. Otherwise
	// reuse the existing persisted key when present (re-running setup must not
	// rotate the key — that would break every issued playback token), and only
	// generate a fresh one on a truly fresh install.
	key := strings.TrimSpace(req.StreamSigningKey)
	if key == "" {
		if prevKey != "" {
			key = prevKey
		} else {
			generated, err := generateSigningKey()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "could not generate signing key")
				return
			}
			key = generated
		}
	}

	err := a.Store.Save(ctx, store.SetupInput{
		DisplayName:      req.DisplayName,
		OIDCIssuer:       req.OIDCIssuer,
		OIDCClientID:     req.OIDCClientID,
		LibraryPath:      req.LibraryPath,
		StreamSigningKey: key,
	})
	if writeStoreErr(w, r, "setup save", err) {
		return
	}

	// Propagate the persisted config + key out to the cluster objects the
	// sibling services read at STARTUP, then roll those deployments so the
	// values take effect. Best-effort: setup already succeeded (the DB write is
	// the source of truth); propagation status is surfaced in the response so
	// the UI can warn on partial failures without failing the whole wizard.
	info := a.propagateSetup(ctx, prevCfg, prevKey, store.Config{
		Configured:   true,
		DisplayName:  req.DisplayName,
		OIDCIssuer:   req.OIDCIssuer,
		OIDCClientID: req.OIDCClientID,
		LibraryPath:  req.LibraryPath,
	}, key)

	writeJSON(w, http.StatusOK, setupResponse{Configured: true, Propagation: info})
}

// deploymentsToRoll is the set of Deployments whose pods read OIDC_ISSUER /
// OIDC_AUDIENCE / MEDIA_ROOT from the stube-env ConfigMap or the stream signing
// key from the stube-stream-signing Secret at startup, and therefore must be
// rolled when those change:
//   - chino-api    — envFrom stube-env (OIDC_*) + STREAM_SIGNING_KEY secret.
//   - chino-stream — OIDC_ISSUER / OIDC_AUDIENCE / STREAM_SIGNING_KEY + media root.
//   - katalog-api  — KATALOG_API_OIDC_ISSUER / _AUDIENCE from stube-env.
//   - chino-web    — static SPA; rolled so any baked runtime config is refreshed
//     and to keep the rollout uniform across the product surface.
var deploymentsToRoll = []string{"chino-api", "chino-stream", "katalog-api", "chino-web"}

// propagateSetup pushes the freshly persisted configuration out to the runtime
// objects sibling services consume at startup and rolls the affected
// deployments. It is idempotent: it only patches the ConfigMap / Secret and
// only restarts when a value actually changed versus the pre-save snapshot, so
// re-running setup with the same inputs is a complete no-op (apart from the DB
// upsert). Every step is best-effort — failures are collected into the returned
// propagationInfo rather than aborting, because the DB write is already durable.
func (a *API) propagateSetup(ctx context.Context, prev store.Config, prevKey string, next store.Config, key string) propagationInfo {
	info := propagationInfo{Enabled: a.K8s.Enabled()}

	if !info.Enabled {
		info.Skipped = append(info.Skipped, "kubernetes credentials absent; running outside a cluster")
		return info
	}

	// Decide what changed. OIDC_AUDIENCE is the manager's own configured
	// audience accept-list (the value the platform validates tokens against);
	// it is not part of the first-run form, so it only forces a patch when it
	// differs from what the ConfigMap presumably holds — we always include it
	// alongside an issuer change to keep issuer+audience consistent.
	envChanged := prev.OIDCIssuer != next.OIDCIssuer ||
		prev.LibraryPath != next.LibraryPath ||
		!prev.Configured // first run: the ConfigMap was never populated from setup
	keyChanged := prevKey != key

	if envChanged {
		data := map[string]string{
			"OIDC_ISSUER":   next.OIDCIssuer,
			"OIDC_AUDIENCE": a.Cfg.OIDCAudience,
			"MEDIA_ROOT":    next.LibraryPath,
		}
		if err := a.K8s.PatchConfigMap(ctx, "stube-env", data); err != nil {
			info.Errors = append(info.Errors, "patch configmap stube-env: "+err.Error())
			slog.Error("setup propagation: configmap patch failed", "err", err)
		} else {
			info.Applied = append(info.Applied, "configmap/stube-env")
		}
	} else {
		info.Skipped = append(info.Skipped, "configmap/stube-env unchanged")
	}

	if keyChanged {
		if err := a.K8s.PatchSecret(ctx, "stube-stream-signing", map[string]string{"key": key}); err != nil {
			info.Errors = append(info.Errors, "patch secret stube-stream-signing: "+err.Error())
			slog.Error("setup propagation: secret patch failed", "err", err)
		} else {
			info.Applied = append(info.Applied, "secret/stube-stream-signing")
		}
	} else {
		info.Skipped = append(info.Skipped, "secret/stube-stream-signing unchanged")
	}

	// Only roll deployments when something they read actually changed. If
	// neither the env nor the key moved, there is nothing for the pods to pick
	// up and we skip the restarts entirely (idempotent re-run).
	if !envChanged && !keyChanged {
		info.Skipped = append(info.Skipped, "no config change; deployments not rolled")
		return info
	}

	for _, dep := range deploymentsToRoll {
		if err := a.K8s.RestartDeployment(ctx, dep); err != nil {
			info.Errors = append(info.Errors, "restart deployment "+dep+": "+err.Error())
			slog.Error("setup propagation: deployment restart failed", "deployment", dep, "err", err)
			continue
		}
		info.Restarts = append(info.Restarts, dep)
	}
	return info
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
