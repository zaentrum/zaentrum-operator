// Command katalog-manager-api is the neutral management plane + first-run
// backend for Stube. It owns the catalog WRITE path (item edits, deletes, and
// the import scan that registers files the operator already owns) and the
// service configuration. Processing work is dispatched asynchronously by
// emitting Kafka task events; nothing here acquires content.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nalet/stube/platform/katalog-manager-api/internal/auth"
	"github.com/nalet/stube/platform/katalog-manager-api/internal/config"
	"github.com/nalet/stube/platform/katalog-manager-api/internal/events"
	managerhttp "github.com/nalet/stube/platform/katalog-manager-api/internal/http"
	"github.com/nalet/stube/platform/katalog-manager-api/internal/k8s"
	"github.com/nalet/stube/platform/katalog-manager-api/internal/store"
)

// version is the reported build version. Overridable at build time via
// -ldflags "-X main.version=...". Defaults to "dev".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()
	slog.Info("starting katalog-manager-api",
		"version", version,
		"addr", cfg.Addr,
		"katalog_api_base_url", cfg.KatalogAPIBaseURL,
		"oidc_issuer", cfg.OIDCIssuer,
		"oidc_audience", cfg.OIDCAudience,
		"pg_set", cfg.PgURL != "",
		"kafka_brokers", cfg.KafkaBrokers,
	)

	ctx := context.Background()

	st, err := store.New(ctx, cfg.PgURL)
	if err != nil {
		slog.Error("store init failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Create our own tables (config + job history) if the pool is present.
	// Best-effort: log and continue so the service still boots in scaffold
	// mode where it can at least answer /healthz and the setup status read.
	if cfg.PgURL != "" {
		if err := st.EnsureSchema(ctx); err != nil {
			slog.Error("ensure config schema failed", "err", err)
		}
		if err := st.EnsureJobsSchema(ctx); err != nil {
			slog.Error("ensure jobs schema failed", "err", err)
		}
	}

	producer, err := events.NewProducer(config.SplitCSV(cfg.KafkaBrokers))
	if err != nil {
		// Don't fatal — the management plane (config, library edits) works
		// without the broker; only processing dispatch needs it.
		slog.Warn("kafka producer init failed; processing dispatch will 502 until brokers are reachable", "err", err)
		producer = &events.Producer{}
	}
	defer func() { _ = producer.Close() }()

	verifier, err := auth.NewVerifier(ctx, cfg.OIDCIssuer, cfg.OIDCAudience)
	if err != nil {
		// Don't fatal — let the service come up so /healthz and the
		// first-run status read answer even if the issuer is unreachable.
		// The auth middleware rejects all protected requests until it works.
		slog.Warn("oidc verifier init failed; protected routes will return 503", "err", err)
	}

	// In-cluster Kubernetes client for first-run config propagation. Never
	// nil: when the ServiceAccount credentials are absent (docker-compose /
	// local), it is a logged no-op so setup still persists to the DB.
	k8sClient := k8s.New()
	slog.Info("k8s config propagation", "enabled", k8sClient.Enabled(), "namespace", k8sClient.Namespace())

	api := &managerhttp.API{
		Cfg:      cfg,
		Store:    st,
		Producer: producer,
		K8s:      k8sClient,
		Version:  version,
	}

	router, err := managerhttp.NewRouter(api, verifier)
	if err != nil {
		slog.Error("router init failed", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	stopCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("http listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-stopCtx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
