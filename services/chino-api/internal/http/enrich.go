package http

import (
	"context"

	"github.com/nalet/stube/services/chino-api/internal/katalog"
	"github.com/nalet/stube/services/chino-api/internal/store"
)

// stampWatched annotates each item with the current user's watched_at
// timestamp (when present in watched_history). Mutates in place; safe
// on nil items / empty slice. Best-effort: a DB error just leaves
// WatchedAt unset rather than failing the whole list response.
func stampWatched(ctx context.Context, st *store.Store, userID string, items []*katalog.Item) {
	if st == nil || userID == "" || len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if it != nil && it.ID != "" {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	watched, err := st.WatchedAtBatch(ctx, userID, ids)
	if err != nil || len(watched) == 0 {
		return
	}
	for _, it := range items {
		if it == nil {
			continue
		}
		if ts, ok := watched[it.ID]; ok {
			t := ts
			it.WatchedAt = &t
		}
	}
}

// stampWatchedSlice is a convenience over stampWatched for a
// concrete-typed slice (listItems gets back a []katalog.Item).
func stampWatchedSlice(ctx context.Context, st *store.Store, userID string, items []katalog.Item) {
	if len(items) == 0 {
		return
	}
	ptrs := make([]*katalog.Item, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	stampWatched(ctx, st, userID, ptrs)
}
