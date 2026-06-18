package http

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/zaentrum/stube/platform/katalog-manager-api/internal/store"
)

// videoExts is the set of container extensions the scan treats as playable
// media. The operator owns these files; the scan only registers what is
// already on disk. It never downloads or fetches anything.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
	".avi": true, ".ts": true, ".webm": true, ".m2ts": true,
}

// episodePattern matches the SxxEyy coordinate in a filename so a scanned
// episode is typed correctly and its season/episode numbers are parsed.
var episodePattern = regexp.MustCompile(`(?i)s(\d{1,2})e(\d{1,3})`)

// yearPattern matches a 4-digit year in parentheses or after a separator,
// used to strip the year out of the derived title.
var yearPattern = regexp.MustCompile(`[\(\.\s_-](19|20)\d{2}[\)\.\s_-]?`)

// scanRequest is the body of POST /api/manage/import/scan.
type scanRequest struct {
	Path string `json:"path"`
}

// ImportScan walks a directory of files the operator already owns and
// registers each new media file as a catalog item with a primary playback
// asset. This is the neutral replacement for the previous acquisition-coupled
// write path: there is no indexer, no download client, and no remote fetch —
// it strictly reflects on-disk reality into the catalog.
//
// The walk is bounded to the configured library path (or below it): a request
// for a path outside the library root is rejected so the endpoint can't be
// pointed at arbitrary host directories.
func (a *API) ImportScan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	scanPath := strings.TrimSpace(req.Path)
	if scanPath == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}

	ctx := r.Context()
	cfg, err := a.Store.LoadConfig(ctx)
	if writeStoreErr(w, r, "load config for scan", err) {
		return
	}
	root := strings.TrimSpace(cfg.LibraryPath)
	if root == "" {
		writeErr(w, http.StatusConflict, "library path not configured; run first-run setup")
		return
	}
	abs, ok := withinRoot(root, scanPath)
	if !ok {
		writeErr(w, http.StatusBadRequest, "path must be within the configured library root")
		return
	}

	info, statErr := os.Stat(abs)
	if statErr != nil || !info.IsDir() {
		writeErr(w, http.StatusBadRequest, "path is not a readable directory")
		return
	}

	result := store.ScanResult{
		JobID:  uuid.NewString(),
		Path:   abs,
		Status: "done",
	}

	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries rather than aborting the whole scan.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !videoExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		result.FilesSeen++

		exists, err := a.Store.AssetPathExists(ctx, p)
		if err != nil {
			return err
		}
		if exists {
			result.ItemsSkipped++
			return nil
		}

		itemType, title := classify(p)
		if _, err := a.Store.RegisterItem(ctx, itemType, title, p); err != nil {
			return err
		}
		result.ItemsInserted++
		return nil
	})
	if walkErr != nil {
		if writeStoreErr(w, r, "import scan walk", walkErr) {
			return
		}
	}

	// Log the scan in the job history so the admin UI's jobs list reflects it.
	_ = a.Store.RecordJob(ctx, result.JobID, "scan", "",
		result.Path+" ("+strconv.Itoa(result.ItemsInserted)+" added, "+
			strconv.Itoa(result.ItemsSkipped)+" skipped)")

	writeJSON(w, http.StatusOK, result)
}

// withinRoot resolves candidate against root and confirms it does not escape
// the root via "..". Returns the cleaned absolute path and whether it is
// contained.
func withinRoot(root, candidate string) (string, bool) {
	root = filepath.Clean(root)
	abs := candidate
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs = filepath.Clean(abs)
	if abs == root {
		return abs, true
	}
	if strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return abs, true
	}
	return abs, false
}

// classify derives an item type and display title from a media file path.
// An SxxEyy coordinate marks an episode; otherwise the file is treated as a
// movie. Title is the cleaned base name with the year and separators removed.
// This mirrors the conventional layout of a personally-organised library and
// makes no network calls — richer metadata is filled in later by the
// enrichment worker after the item is registered.
func classify(path string) (itemType, title string) {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if m := episodePattern.FindStringSubmatch(base); m != nil {
		return "episode", cleanTitle(base)
	}
	return "movie", cleanTitle(base)
}

// cleanTitle turns a raw file base name into a human-readable title: strip the
// SxxEyy episode coordinate, drop a year, replace separators with spaces, and
// collapse whitespace.
func cleanTitle(base string) string {
	t := episodePattern.ReplaceAllString(base, " ")
	t = yearPattern.ReplaceAllString(t, " ")
	t = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(t)
	t = strings.Join(strings.Fields(t), " ")
	t = strings.TrimSpace(t)
	if t == "" {
		return base
	}
	return t
}
