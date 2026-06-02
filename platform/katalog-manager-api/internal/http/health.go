package http

import "net/http"

// Health is the liveness/readiness probe target. It reports ok unconditionally
// so the pod stays in service even when the DB or broker are still warming up —
// the setup status endpoint carries the per-dependency detail.
func (a *API) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
