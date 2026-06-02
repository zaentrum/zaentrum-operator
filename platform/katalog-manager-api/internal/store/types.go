package store

// Config is the persisted, non-secret service configuration. It is the wire
// shape returned by GET /api/manage/config and accepted by PUT
// /api/manage/config. The stream signing key is intentionally NOT a field
// here — it is a secret and is never returned over the config endpoints.
type Config struct {
	Configured   bool   `json:"configured"`
	DisplayName  string `json:"displayName"`
	OIDCIssuer   string `json:"oidcIssuer"`
	OIDCClientID string `json:"oidcClientId"`
	LibraryPath  string `json:"libraryPath"`
}

// Item is the management-plane wire shape of a catalog item. It carries the
// editable core fields the admin UI exposes plus read-only identity columns.
// Field names match the read API's snake_case convention so a single client
// model serves both planes.
type Item struct {
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	SortTitle     string  `json:"sort_title,omitempty"`
	Year          *int    `json:"year,omitempty"`
	Rating        float64 `json:"rating,omitempty"`
	Description   string  `json:"description,omitempty"`
	Tagline       string  `json:"tagline,omitempty"`
	DurationMs    int64   `json:"duration_ms,omitempty"`
	SeasonNumber  *int    `json:"season_number,omitempty"`
	EpisodeNumber *int    `json:"episode_number,omitempty"`
	ParentID      string  `json:"parent_id,omitempty"`
}

// ItemUpdate is the mutable subset of an item accepted by PUT
// /api/manage/items/{id}. All fields are pointers so the handler can apply a
// partial patch: a nil field is left untouched, a non-nil field is written.
type ItemUpdate struct {
	Title       *string  `json:"title,omitempty"`
	SortTitle   *string  `json:"sort_title,omitempty"`
	Year        *int     `json:"year,omitempty"`
	Rating      *float64 `json:"rating,omitempty"`
	Description *string  `json:"description,omitempty"`
	Tagline     *string  `json:"tagline,omitempty"`
}

// ListResult is the paginated envelope every list endpoint returns.
type ListResult struct {
	Items  []Item `json:"items"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// ListOpts is the common list-endpoint parameter set for the management
// library view.
type ListOpts struct {
	Type   string // optional 'movie' | 'series' | 'episode' | 'album' | …
	Query  string // case-insensitive title contains-match
	Limit  int    // clamped to [1, 200], default 50
	Offset int    // clamped to >=0, default 0
}

// ScanResult summarises one import scan over a directory of owned files.
type ScanResult struct {
	JobID         string `json:"jobId"`
	Path          string `json:"path"`
	Status        string `json:"status"`
	FilesSeen     int    `json:"filesSeen"`
	ItemsInserted int    `json:"itemsInserted"`
	ItemsSkipped  int    `json:"itemsSkipped"`
}

// Job is one row of the processing/scan job history surfaced by
// GET /api/manage/jobs.
type Job struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`   // 'scan' | 'transcode' | 'package' | 'enrich'
	Status     string `json:"status"` // 'queued' | 'running' | 'done' | 'failed'
	ItemID     string `json:"itemId,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Detail     string `json:"detail,omitempty"`
}
