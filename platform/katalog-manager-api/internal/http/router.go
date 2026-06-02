package http

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nalet/stube/platform/katalog-manager-api/internal/auth"
)

// NewRouter wires the chi router: the standard middleware stack, the public
// probes, the unauthenticated first-run status read, and the OIDC-gated
// management surface mounted at /api/manage.
//
// Route prefix is /api/manage so the edge proxy can forward the management
// plane and the admin UI (served at /manage) without path rewriting.
func NewRouter(a *API, verifier *auth.Verifier) (http.Handler, error) {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Public probes.
	r.Get("/healthz", a.Health)
	r.Handle("/metrics", promhttp.Handler())

	// First-run status is intentionally reachable WITHOUT a bearer token: the
	// admin UI must render the wizard before any identity provider exists to
	// authenticate against. It exposes only booleans + a version string — no
	// secrets, no catalog data — so leaving it open is safe. Every other
	// management route requires a valid token.
	r.Get("/api/manage/setup/status", a.SetupStatus)

	// Authenticated management surface.
	mgmt := chi.NewRouter()
	mgmt.Use(verifier.Middleware)

	// First-run setup (persist config). Gated like everything else: the UI
	// relays the operator's bearer token. Re-runnable (idempotent upsert).
	mgmt.Post("/setup", a.Setup)

	// Live config.
	mgmt.Get("/config", a.GetConfig)
	mgmt.Put("/config", a.UpdateConfig)

	// Import scan — register files the operator already owns. No acquisition.
	mgmt.Post("/import/scan", a.ImportScan)

	// Catalog library write surface.
	mgmt.Get("/library", a.Library)
	mgmt.Put("/items/{id}", a.UpdateItem)
	mgmt.Delete("/items/{id}", a.DeleteItem)

	// Processing dispatch (emits Kafka task events) + job history.
	mgmt.Get("/jobs", a.Jobs)
	mgmt.Post("/items/{id}/transcode", a.Transcode)
	mgmt.Post("/items/{id}/package", a.Package)
	mgmt.Post("/items/{id}/enrich", a.Enrich)

	r.Mount("/api/manage", mgmt)
	return r, nil
}
