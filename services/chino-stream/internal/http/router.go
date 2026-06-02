package http

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/nalet/stube/services/chino-stream/internal/auth"
	"github.com/nalet/stube/services/chino-stream/internal/catalog"
	"github.com/nalet/stube/services/chino-stream/internal/metrics"
	"github.com/nalet/stube/services/chino-stream/internal/play"
)

type Deps struct {
	Catalog         *catalog.Client
	MediaRoot       string
	OIDCIssuer      string
	OIDCAudience    string
	AuthEnabled     bool
	FFmpegBin       string
	FFprobeBin      string
	TranscodePreset string
	// StreamSigningKey shared with chino-api's auth.Signer. Empty falls
	// back to an ephemeral random key (tokens from chino-api won't
	// validate; OIDC bearer still works).
	StreamSigningKey string
	// HLSCacheDir is the on-disk staging area for transcoded HLS init
	// + media segments. Cleared periodically by a sweeper; survives pod
	// restarts only as long as the volume is persistent (currently
	// emptyDir, so segments warm again on each pod start).
	HLSCacheDir string
	// NVENC opt-in. Set when the pod has a nvidia.com/gpu allocated
	// and the runtime image bundles a CUDA-aware ffmpeg.
	UseNVENC    bool
	NVENCPreset string
	NVENCCQ     string
}

func NewRouter(d Deps) (http.Handler, error) {
	verifier, err := auth.New(context.Background(), d.OIDCIssuer, d.OIDCAudience, d.AuthEnabled)
	if err != nil {
		return nil, err
	}
	signer, err := auth.NewSigner(d.StreamSigningKey)
	if err != nil {
		return nil, err
	}
	verifier = verifier.WithStreamSigner(signer)

	playH := &play.Handler{
		Catalog:         d.Catalog,
		MediaRoot:       d.MediaRoot,
		FFmpegBin:       d.FFmpegBin,
		FFprobeBin:      d.FFprobeBin,
		TranscodePreset: d.TranscodePreset,
		UseNVENC:        d.UseNVENC,
		// Share the HLS cache root so subtitle .vtt extracts go
		// through the same disk budget + sweeper as HLS segments.
		CacheDir: d.HLSCacheDir,
	}
	hlsH := &play.HLSHandler{
		Catalog:         d.Catalog,
		MediaRoot:       d.MediaRoot,
		FFmpegBin:       d.FFmpegBin,
		FFprobeBin:      d.FFprobeBin,
		TranscodePreset: d.TranscodePreset,
		CacheDir:        d.HLSCacheDir,
		UseNVENC:        d.UseNVENC,
		NVENCPreset:     d.NVENCPreset,
		NVENCCQ:         d.NVENCCQ,
	}
	// Sweep cached segments older than 2 h every 10 min. Tunable later
	// when usage scales past one user.
	hlsH.StartCacheSweeper(context.Background(), 10*time.Minute, 2*time.Hour)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	// Prometheus scrape target — un-authed because the chino-stream
	// Service is cluster-internal; only hyperv-prometheus (grafana ns)
	// reaches it.
	r.Method("GET", "/metrics", metrics.Handler())

	r.Group(func(r chi.Router) {
		r.Use(verifier.Middleware)
		// Legacy progressive-MP4 endpoint kept around for fallback /
		// quick smoke tests; chino-web no longer uses it on the HLS
		// cutover.
		// Listing of items that have a finished CMAF package on disk.
		// Used by the Zap pager to filter its candidate pool to
		// instant-start items. Registered BEFORE /api/play/{itemId}
		// so chi's trie picks this static segment over treating
		// "packaged-ids" as an itemId parameter (it normally does
		// static-over-wildcard but explicit ordering removes the
		// surprise).
		r.Get("/api/play/packaged-ids", hlsH.PackagedIDs)
		r.Get("/api/play/{itemId}", playH.Play)
		r.Get("/api/play/{itemId}/info", playH.Info)
		// Embedded-subtitle extractor — runs ffmpeg with -map 0:s:N -c:s
		// webvtt so the browser can mount the result as a <track>. Tokens
		// from <track src> can't carry headers, so the caller appends
		// ?token=… (the auth middleware already accepts that form).
		r.Get("/api/play/{itemId}/subtitles/{streamIndex}.vtt", playH.EmbeddedSubtitle)
		// Sidecar subtitles: pre-packaged .vtt files on the packages PVC.
		// The id comes from the item-detail subtitles[] list; this
		// handler resolves it to a path via katalog-api and serves the
		// file with byte-range support.
		r.Get("/api/play/subs/{subID}.vtt", playH.SidecarSubtitle)
		// HLS sub-router: master.m3u8, per-quality playlists, init
		// segment, and on-demand media segments. See HLSHandler.Routes
		// for the URL shape.
		r.Route("/api/play/{itemId}", func(r chi.Router) {
			hlsH.Routes(r)
		})
	})

	return r, nil
}
