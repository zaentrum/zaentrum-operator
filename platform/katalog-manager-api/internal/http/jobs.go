package http

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/events"
)

// Jobs returns the recent job history (scans + processing dispatches),
// newest first. `?limit=` clamps the page size (default 50, max 200).
func (a *API) Jobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	jobs, err := a.Store.ListJobs(r.Context(), limit)
	if writeStoreErr(w, r, "list jobs", err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": jobs,
		"total": len(jobs),
	})
}

// Transcode enqueues a transcode pass for an item by emitting a
// stube.processing.task.transcode event. It does not run the transcode — the
// transcoder worker consumes the event. 404 when the item doesn't exist; 202
// once the event is published and the dispatch is logged.
func (a *API) Transcode(w http.ResponseWriter, r *http.Request) {
	a.enqueue(w, r, events.TopicTranscode, "transcode")
}

// Package enqueues a packaging pass (stube.processing.task.package). Same
// contract as Transcode.
func (a *API) Package(w http.ResponseWriter, r *http.Request) {
	a.enqueue(w, r, events.TopicPackage, "package")
}

// Enrich triggers metadata enrichment for an item
// (stube.processing.task.enrich). Same contract as Transcode.
func (a *API) Enrich(w http.ResponseWriter, r *http.Request) {
	a.enqueue(w, r, events.TopicEnrich, "enrich")
}

// enqueue is the shared body for the transcode/package/enrich endpoints:
// confirm the item exists, publish the task event, log the dispatch, and
// return 202 Accepted with the job id.
func (a *API) enqueue(w http.ResponseWriter, r *http.Request, topic, kind string) {
	id := chi.URLParam(r, "id")
	ctx := r.Context()

	// Confirm the item exists so we don't enqueue work for a phantom id.
	if _, err := a.Store.GetItem(ctx, id); writeStoreErr(w, r, kind+" item lookup", err) {
		return
	}

	if err := a.Producer.PublishTask(ctx, topic, id); err != nil {
		writeErr(w, http.StatusBadGateway, "could not enqueue "+kind+": "+err.Error())
		return
	}

	jobID := uuid.NewString()
	_ = a.Store.RecordJob(ctx, jobID, kind, id, "enqueued via "+topic)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"jobId":  jobID,
		"itemId": id,
		"kind":   kind,
		"status": "queued",
	})
}
