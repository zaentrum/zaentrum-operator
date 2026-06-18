package http

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/store"
)

// Library returns a paged list of catalog items for the management console.
// Supports `?type=`, `?q=` (title contains), `?limit=`, `?offset=`. Bad
// numeric values are silently clamped rather than rejected — a UI control
// sending garbage shouldn't 400 the whole list.
func (a *API) Library(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := store.ListOpts{
		Type:  q.Get("type"),
		Query: q.Get("q"),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			opts.Offset = n
		}
	}
	res, err := a.Store.ListItems(r.Context(), opts)
	if writeStoreErr(w, r, "library list", err) {
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// UpdateItem applies a partial patch to one item's editable fields and returns
// the updated row. 404 when the id doesn't exist.
func (a *API) UpdateItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var patch store.ItemUpdate
	if !decodeJSON(w, r, &patch) {
		return
	}
	it, err := a.Store.UpdateItem(r.Context(), id, patch)
	if writeStoreErr(w, r, "update item", err) {
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// DeleteItem removes an item (and its composed asset/subtitle/segment rows via
// cascade). 204 on success, 404 when the id doesn't exist. This only removes
// the catalog record; the underlying file the operator owns is left untouched
// on disk.
func (a *API) DeleteItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := a.Store.DeleteItem(r.Context(), id)
	if writeStoreErr(w, r, "delete item", err) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
