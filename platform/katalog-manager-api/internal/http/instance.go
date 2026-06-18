package http

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/k8s"
)

// instanceView is the response of GET /api/manage/instance and the body
// returned by a successful PATCH. It is the projection of the Stube CR the
// admin UI's update surface renders: the desired-state knobs the operator can
// turn (channel, updateMode), the observed-state mirror the operator's Stage-2
// reconciler maintains (currentVersion, availableUpdate, phase, components) and
// the spec.version tag currently pinned.
//
// The shape is part of the Stage-2 contract shared with the admin UI — keep it
// identical on both sides.
type instanceView struct {
	// Channel is spec.channel: "stable" or "edge".
	Channel string `json:"channel"`
	// Version is spec.version: the image tag pinned on every component
	// ("latest" or a concrete tag).
	Version string `json:"version"`
	// CurrentVersion mirrors status.currentVersion — the tag actually rolled
	// out across the cluster.
	CurrentVersion string `json:"currentVersion"`
	// AvailableUpdate is status.availableUpdate — the newest in-channel tag the
	// operator discovered, if any. Empty when up to date.
	AvailableUpdate string `json:"availableUpdate"`
	// UpdateMode is spec.update.mode: "manual" or "auto".
	UpdateMode string `json:"updateMode"`
	// Phase is status.phase — a coarse human-facing lifecycle string.
	Phase string `json:"phase"`
	// Components mirrors status.components: per-Deployment readiness.
	Components []componentView `json:"components"`
}

// componentView mirrors one entry of status.components[].
type componentView struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	Image string `json:"image"`
}

// Instance returns the projection of the Stube CR the update surface renders.
// It reads the CR (namespace/name from STUBE_CR_NAMESPACE / STUBE_CR_NAME,
// defaulting to stube/stube) via the in-cluster client and maps spec+status
// onto instanceView.
//
// Outside a cluster (no ServiceAccount token) the read has no fallback, so it
// returns 503 — the management plane cannot report instance state when it is
// not the one driving a cluster.
func (a *API) Instance(w http.ResponseWriter, r *http.Request) {
	obj, err := a.K8s.GetStube(r.Context(), a.Cfg.StubeCRNamespace, a.Cfg.StubeCRName)
	if a.writeStubeErr(w, "get stube", err) {
		return
	}
	writeJSON(w, http.StatusOK, viewFromStube(obj))
}

// instancePatch is the body of PATCH /api/manage/instance. Every field is
// optional. channel/updateMode change desired state directly; apply:true is the
// "install the available update" action — it copies status.availableUpdate into
// spec.version, which triggers the operator to roll the new tag. apply with no
// availableUpdate is a 409 (nothing to apply).
type instancePatch struct {
	Channel    *string `json:"channel,omitempty"`
	UpdateMode *string `json:"updateMode,omitempty"`
	Apply      *bool   `json:"apply,omitempty"`
}

// PatchInstance mutates the Stube CR spec per the contract: it sets
// spec.channel and/or spec.update.mode, and when apply:true is set it pins
// spec.version to the currently available update so the operator rolls it. It
// validates enum values and rejects an empty patch, then applies a single
// merge patch and returns the refreshed view.
func (a *API) PatchInstance(w http.ResponseWriter, r *http.Request) {
	var req instancePatch
	if !decodeJSON(w, r, &req) {
		return
	}

	spec := map[string]any{}

	if req.Channel != nil {
		ch := *req.Channel
		if ch != "stable" && ch != "edge" {
			writeErr(w, http.StatusBadRequest, "channel must be 'stable' or 'edge'")
			return
		}
		spec["channel"] = ch
	}

	if req.UpdateMode != nil {
		mode := *req.UpdateMode
		if mode != "manual" && mode != "auto" {
			writeErr(w, http.StatusBadRequest, "updateMode must be 'manual' or 'auto'")
			return
		}
		// spec.update.mode is nested under spec.update.
		spec["update"] = map[string]any{"mode": mode}
	}

	// apply:true installs the available update. We need the current CR to read
	// status.availableUpdate; a GET also lets us 409 cleanly when there is
	// nothing to apply rather than pinning spec.version to an empty string.
	if req.Apply != nil && *req.Apply {
		cur, err := a.K8s.GetStube(r.Context(), a.Cfg.StubeCRNamespace, a.Cfg.StubeCRName)
		if a.writeStubeErr(w, "get stube", err) {
			return
		}
		avail := stringAt(cur, "status", "availableUpdate")
		if avail == "" {
			writeErr(w, http.StatusConflict, "no update available to apply")
			return
		}
		spec["version"] = avail
	}

	if len(spec) == 0 {
		writeErr(w, http.StatusBadRequest, "no changes: set channel, updateMode and/or apply")
		return
	}

	patch := map[string]any{"spec": spec}
	updated, err := a.K8s.PatchStube(r.Context(), a.Cfg.StubeCRNamespace, a.Cfg.StubeCRName, patch)
	if a.writeStubeErr(w, "patch stube", err) {
		return
	}
	writeJSON(w, http.StatusOK, viewFromStube(updated))
}

// writeStubeErr maps a k8s client error onto an HTTP response. ErrDisabled
// (running outside a cluster) -> 503; everything else is logged and surfaced as
// a 502 because the failure is in the manager's call to the API server, not in
// the operator's input. Returns true when it handled the error.
func (a *API) writeStubeErr(w http.ResponseWriter, op string, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, k8s.ErrDisabled) {
		writeErr(w, http.StatusServiceUnavailable,
			"instance management is unavailable: the manager is not running in a cluster")
		return true
	}
	slog.Error("stube CR access failed", "op", op, "err", err)
	writeErr(w, http.StatusBadGateway, "could not reach the Stube custom resource: "+err.Error())
	return true
}

// viewFromStube projects a raw Stube CR (decoded as a generic map) onto the
// instanceView the UI consumes. It tolerates missing fields (a freshly created
// CR with no status yet yields empty observed-state fields and a nil-safe empty
// components slice) so the surface renders before the operator first reconciles.
func viewFromStube(obj map[string]any) instanceView {
	v := instanceView{
		Channel:         stringAt(obj, "spec", "channel"),
		Version:         stringAt(obj, "spec", "version"),
		UpdateMode:      stringAt(obj, "spec", "update", "mode"),
		CurrentVersion:  stringAt(obj, "status", "currentVersion"),
		AvailableUpdate: stringAt(obj, "status", "availableUpdate"),
		Phase:           stringAt(obj, "status", "phase"),
		Components:      componentsFromStatus(obj),
	}
	return v
}

// componentsFromStatus reads status.components[] into a non-nil slice so the
// JSON encodes [] rather than null when the operator hasn't populated it yet.
func componentsFromStatus(obj map[string]any) []componentView {
	out := []componentView{}
	status, ok := obj["status"].(map[string]any)
	if !ok {
		return out
	}
	raw, ok := status["components"].([]any)
	if !ok {
		return out
	}
	for _, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, componentView{
			Name:  asString(m["name"]),
			Ready: asBool(m["ready"]),
			Image: asString(m["image"]),
		})
	}
	return out
}

// stringAt walks a chain of nested map[string]any keys and returns the leaf as
// a string, or "" if any step is missing or not the expected type. Keeps the
// projection free of repeated comma-ok ladders.
func stringAt(obj map[string]any, keys ...string) string {
	cur := obj
	for i, k := range keys {
		if cur == nil {
			return ""
		}
		val, ok := cur[k]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			return asString(val)
		}
		next, ok := val.(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	return ""
}

// asString coerces a JSON-decoded value to a string, returning "" for non-string
// (or nil) values.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// asBool coerces a JSON-decoded value to a bool, returning false for non-bool
// (or nil) values.
func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
