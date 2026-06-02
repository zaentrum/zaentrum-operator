package play

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/nalet/stube/services/chino-stream/internal/pkgmanifest"
)

// PackagesRoot is where the packager writes the per-item CMAF trees.
// Mounted read-only into chino-stream pods; the path here must match
// the volume mount in the deployment manifest. Configurable via the
// PACKAGES_ROOT env var, defaulting to /var/lib/stube/packages.
//
// Layout under PackagesRoot is sharded to keep any single directory's
// child count bounded:
//
//	{category}/{shard2}/{itemId}/{manifest.json, .complete, hls/, …}
//
// where category is one of {movies, shows, music, other} (mapped from
// the catalog item type) and shard2 is the first two hex chars of the
// item uuid. At 10k items per category, ~40 per shard — every
// filesystem reads that fast.
var PackagesRoot = packagesRootDefault()

func packagesRootDefault() string {
	if v := os.Getenv("PACKAGES_ROOT"); v != "" {
		return v
	}
	return "/var/lib/stube/packages"
}

// Known top-level category dirs the analyzer writes to. The stream
// side probes these on read to locate a package for a given item id
// (it doesn't know the item type from the URL alone).
var packageCategories = []string{"movies", "shows", "music", "other"}

// itemRootCache memoises the resolved package directory per item id
// so subsequent requests skip the probe loop. Entries are kept until
// pod restart — the only invalidation we'd ever need is when an item
// gets repackaged into a different category, which doesn't happen in
// practice (category is derived from a stable item type).
var itemRootCache sync.Map // map[string]string (itemId -> abs package dir)

// itemRoot returns the on-disk package directory for the given item
// id, probing each category until one is found. Returns "" when the
// item has no package on any category. Result is cached.
func itemRoot(itemID string) string {
	if itemID == "" {
		return ""
	}
	if v, ok := itemRootCache.Load(itemID); ok {
		return v.(string)
	}
	if len(itemID) < 2 {
		return ""
	}
	shard := strings.ToLower(itemID[:2])
	for _, cat := range packageCategories {
		path := filepath.Join(PackagesRoot, cat, shard, itemID)
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			itemRootCache.Store(itemID, path)
			return path
		}
	}
	return ""
}

// packagedIDsCache memoises the directory walk under PackagesRoot so
// the listing endpoint doesn't restat ~40k dirs on every Zap session
// open. 60-second TTL is short enough to pick up new packages within
// a single user session, long enough that the FS walk amortises to
// zero on busy paths.
var (
	packagedIDsCacheMu  sync.Mutex
	packagedIDsCacheAt  time.Time
	packagedIDsCacheVal []string
)

const packagedIDsCacheTTL = 60 * time.Second

// ListCompletedPackageIDs walks PackagesRoot and returns the ids of
// every item with a finished .complete sentinel. The walk is sharded
// (4 categories × 256 shards × ~40 items) so even a fully-populated
// catalogue is sub-second on local disk, and ~1-3s on NFS. The result
// is cached for packagedIDsCacheTTL so back-to-back Zap session opens
// share the same listing.
//
// Used by the /api/play/packaged-ids endpoint that the Zap pager
// consults to filter its candidate pool to instant-start items —
// packaged items skip ffmpeg entirely and serve in tens of ms,
// avoiding the 1-3s cold start that on-demand transcode imposes.
func ListCompletedPackageIDs() []string {
	packagedIDsCacheMu.Lock()
	defer packagedIDsCacheMu.Unlock()
	if time.Since(packagedIDsCacheAt) < packagedIDsCacheTTL && packagedIDsCacheVal != nil {
		return packagedIDsCacheVal
	}
	ids := make([]string, 0, 256)
	for _, cat := range packageCategories {
		catDir := filepath.Join(PackagesRoot, cat)
		shards, err := os.ReadDir(catDir)
		if err != nil {
			continue
		}
		for _, sh := range shards {
			if !sh.IsDir() {
				continue
			}
			shDir := filepath.Join(catDir, sh.Name())
			items, err := os.ReadDir(shDir)
			if err != nil {
				continue
			}
			for _, it := range items {
				if !it.IsDir() {
					continue
				}
				if _, err := os.Stat(filepath.Join(shDir, it.Name(), ".complete")); err == nil {
					ids = append(ids, it.Name())
				}
			}
		}
	}
	packagedIDsCacheAt = time.Now()
	packagedIDsCacheVal = ids
	return ids
}

// HasCompletedPackage reports whether the given item has a finished
// CMAF package on disk. Cheap on a cache hit (single stat); cold-path
// is at most len(packageCategories) stats. Used by the master / init
// / segment handlers to dispatch between "serve static files from
// /media/packages" and "fall through to the legacy on-demand
// transcode".
func HasCompletedPackage(itemID string) bool {
	root := itemRoot(itemID)
	if root == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(root, ".complete"))
	return err == nil && !st.IsDir()
}

// packagePath joins {item-root}/rel safely. The chi URL params are
// constrained by route regex (^v[0-9]+$|^a[0-9]+$ for rendId, digits
// for seg) so a hostile rendId can't traverse out of the item
// directory, but we still filepath.Clean before stat as a belt-and-
// braces measure.
func packagePath(itemID string, rel ...string) string {
	root := itemRoot(itemID)
	if root == "" {
		return ""
	}
	parts := append([]string{root}, rel...)
	return filepath.Clean(filepath.Join(parts...))
}

// PackagedPlayableBy reports whether at least one packaged video
// rendition uses a codec the client says it can decode. The current
// packager only emits a single video rendition (HEVC for everything
// post-2024), so HEVC-only items must fall through to the on-demand
// libx264 transcode path for clients without `hvc` caps (Android
// Chrome, Linux Chrome, anything without a HEVC decoder licence).
// Returns true when the manifest is unreadable so we don't 404 the
// player just because we couldn't introspect renditions.
func PackagedPlayableBy(itemID string, caps Caps) bool {
	mf, err := ReadPackageManifest(itemID)
	if err != nil || mf == nil {
		return true
	}
	if len(mf.Renditions.Video) == 0 {
		return true
	}
	for _, v := range mf.Renditions.Video {
		switch codecFamily(v.Codec) {
		case "h264":
			if caps.Video["h264"] || caps.Video["avc1"] {
				return true
			}
		case "hevc":
			if caps.Video["hevc"] || caps.Video["hvc1"] || caps.Video["h265"] {
				return true
			}
		case "vp9":
			if caps.Video["vp9"] {
				return true
			}
		case "av1":
			if caps.Video["av1"] {
				return true
			}
		default:
			// Unknown codec string — assume playable rather than
			// black-screen the user on a parse miss.
			return true
		}
	}
	return false
}

// codecFamily reduces a master.m3u8-style codec string (avc1.640028,
// hvc1.1.6.L120.B0, hev1.1.6.L120.B0, vp09.00.10.08, av01.0.05M.08)
// to a stable family key matching ParseCaps tokens.
func codecFamily(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case strings.HasPrefix(c, "avc1") || strings.HasPrefix(c, "h264"):
		return "h264"
	case strings.HasPrefix(c, "hvc1") || strings.HasPrefix(c, "hev1") || strings.HasPrefix(c, "hevc") || strings.HasPrefix(c, "h265"):
		return "hevc"
	case strings.HasPrefix(c, "vp09") || strings.HasPrefix(c, "vp9"):
		return "vp9"
	case strings.HasPrefix(c, "av01") || strings.HasPrefix(c, "av1"):
		return "av1"
	}
	return ""
}

// playlistCache memoises the URI-rewritten m3u8 bodies for master and
// per-rendition playlists. The hot path here used to ReadFile + run
// the rewriteM3U8URIs scanner on every request, but the source files
// are static for the life of the package and the only per-request
// variable is the query string (used by rewriteM3U8URIs). Cache by
// (path, query) keyed on file mtime so repackages still get fresh
// content without a pod restart.
type playlistCacheEntry struct {
	body  []byte
	etag  string
	mtime time.Time
}

var playlistCache sync.Map // map[string]*playlistCacheEntry (cacheKey -> entry)

// servePlaylistCached reads + rewrites + caches an m3u8 file, then
// serves it with an ETag and a short max-age so browser revisits
// short-circuit at the cache layer.
func servePlaylistCached(w http.ResponseWriter, r *http.Request, path string) {
	st, err := os.Stat(path)
	if err != nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}
	// Cache key includes the query so different ?stream=…/?caps=…
	// rewrites stay distinct. Cap at the lifetime of the file mtime
	// — different mtime = different entry, so a repackage shows up.
	cacheKey := path + "?" + r.URL.RawQuery
	if v, ok := playlistCache.Load(cacheKey); ok {
		if e := v.(*playlistCacheEntry); e.mtime.Equal(st.ModTime()) {
			if match := r.Header.Get("If-None-Match"); match != "" && match == e.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "private, max-age=60")
			w.Header().Set("ETag", e.etag)
			_, _ = w.Write(e.body)
			return
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}
	out := []byte(rewriteM3U8URIs(string(raw), r.URL.RawQuery))
	sum := sha1.Sum(out)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	playlistCache.Store(cacheKey, &playlistCacheEntry{body: out, etag: etag, mtime: st.ModTime()})
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(out)
}

// PackagedMaster serves the shaka-generated master.m3u8 with every
// rendition URI rewritten to carry the inbound query string. Without
// the rewrite the player would resolve `v0/playlist.m3u8` against the
// master's URL and drop `?stream=…`, leaving subsequent rendition
// fetches unauthenticated.
func (h *HLSHandler) PackagedMaster(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	servePlaylistCached(w, r, packagePath(itemID, "hls", "master.m3u8"))
}

// PackagedRenditionPlaylist serves the per-rendition playlist.m3u8
// (video or audio), again rewriting segment URIs to carry the query
// string. Path is /api/play/{itemId}/{rendId}/playlist.m3u8.
func (h *HLSHandler) PackagedRenditionPlaylist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	servePlaylistCached(w, r, packagePath(itemID, "hls", rendID, "playlist.m3u8"))
}

// PackagedIframesPlaylist serves shaka's I-frame trick-play playlist.
// Same shape as the regular rendition playlist; the player loads it
// when the user scrubs.
func (h *HLSHandler) PackagedIframesPlaylist(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	path := packagePath(itemID, "hls", rendID, "iframes.m3u8")
	body, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "iframes not found", http.StatusNotFound)
		return
	}
	out := rewriteM3U8URIs(string(body), r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(out))
}

// PackagedInitSegment serves a rendition's CMAF init.mp4 from the
// packages PVC. Reads into memory once per (path, mtime) and serves
// subsequent Range requests from the buffer — see servePackagedStatic
// for the rationale (NFS Range-read contention).
func (h *HLSHandler) PackagedInitSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	path := packagePath(itemID, "hls", rendID, "init.mp4")
	servePackagedStatic(w, r, path, "video/mp4", "init not found")
}

// PackagedSegment serves one CMAF media segment from the packages
// PVC. Path: /api/play/{itemId}/{rendId}/seg-{seg}.m4s. The {seg}
// value is matched as 5-digit zero-padded in the route so shaka's
// seg-00001.m4s naming flows through unchanged.
func (h *HLSHandler) PackagedSegment(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	rendID := chi.URLParam(r, "rendId")
	seg := chi.URLParam(r, "seg")
	path := packagePath(itemID, "hls", rendID, "seg-"+seg+".m4s")
	servePackagedStatic(w, r, path, "video/iso.segment", "segment not found")
}

// packagedFileCache is an in-memory LRU-ish cache for packaged
// static files. Keyed by path; the value is the full file contents
// plus mtime + last-touch time.
//
// Why we have it: http.ServeFile uses Range-aware streaming with a
// live file descriptor across the whole response. On networked
// storage, three parallel Range requests for the same segment
// (typical hls.js behaviour during scrubs) can take many seconds each
// because the client serialises reads on the inode. Slurping the file
// once into a []byte and serving subsequent ranges from memory drops
// latency from seconds to sub-ms.
//
// Size budget: chino-stream pod limit is 4 GiB; cap the cache at
// ~512 MiB so transcode buffers + Go heap + page cache still fit.
// Evictions are LRU-ish: when total bytes exceed the cap, walk the
// map and drop entries with oldest lastTouch until under budget.
const packagedCacheBudgetBytes = 512 * 1024 * 1024

type packagedCacheEntry struct {
	bytes     []byte
	mtime     time.Time
	lastTouch atomic.Int64 // unix nanos
}

var (
	packagedCacheMu      sync.Mutex
	packagedCacheLoading sync.Map // map[string]*sync.Mutex — per-path read serialisation
	packagedCache        sync.Map // map[string]*packagedCacheEntry
	packagedCacheBytes   atomic.Int64
)

func packagedLoadLock(path string) *sync.Mutex {
	mu, _ := packagedCacheLoading.LoadOrStore(path, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// servePackagedStatic serves a file from the packages PVC with an
// in-memory cache that survives Range requests + concurrent
// duplicates. Returns 404 with `notFoundMsg` if the file doesn't
// exist; 500 if the read fails.
func servePackagedStatic(w http.ResponseWriter, r *http.Request, path, contentType, notFoundMsg string) {
	st, err := os.Stat(path)
	if err != nil {
		http.Error(w, notFoundMsg, http.StatusNotFound)
		return
	}
	mtime := st.ModTime()

	// Cache hit (matching mtime) → serve from memory.
	if v, ok := packagedCache.Load(path); ok {
		if entry := v.(*packagedCacheEntry); entry.mtime.Equal(mtime) {
			entry.lastTouch.Store(time.Now().UnixNano())
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
			http.ServeContent(w, r, filepath.Base(path), mtime, bytes.NewReader(entry.bytes))
			return
		}
	}

	// Cache miss. Serialise concurrent loaders of the same path so
	// only one goroutine pays the NFS read cost; the others wait
	// on the mutex and then hit the cache.
	mu := packagedLoadLock(path)
	mu.Lock()
	defer mu.Unlock()
	if v, ok := packagedCache.Load(path); ok {
		if entry := v.(*packagedCacheEntry); entry.mtime.Equal(mtime) {
			entry.lastTouch.Store(time.Now().UnixNano())
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
			http.ServeContent(w, r, filepath.Base(path), mtime, bytes.NewReader(entry.bytes))
			return
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "read failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	entry := &packagedCacheEntry{bytes: data, mtime: mtime}
	entry.lastTouch.Store(time.Now().UnixNano())
	if v, loaded := packagedCache.LoadOrStore(path, entry); loaded {
		entry = v.(*packagedCacheEntry)
	} else {
		newTotal := packagedCacheBytes.Add(int64(len(data)))
		if newTotal > packagedCacheBudgetBytes {
			go evictPackagedCache()
		}
	}
	entry.lastTouch.Store(time.Now().UnixNano())

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	http.ServeContent(w, r, filepath.Base(path), mtime, bytes.NewReader(entry.bytes))
}

// evictPackagedCache drops the oldest-touched entries until the
// total cached byte count is back under budget. Single-threaded via
// packagedCacheMu so we don't fight ourselves under load spikes.
func evictPackagedCache() {
	packagedCacheMu.Lock()
	defer packagedCacheMu.Unlock()
	if packagedCacheBytes.Load() <= packagedCacheBudgetBytes {
		return
	}
	type kv struct {
		path  string
		entry *packagedCacheEntry
	}
	all := make([]kv, 0, 256)
	packagedCache.Range(func(k, v any) bool {
		all = append(all, kv{k.(string), v.(*packagedCacheEntry)})
		return true
	})
	sort.Slice(all, func(i, j int) bool {
		return all[i].entry.lastTouch.Load() < all[j].entry.lastTouch.Load()
	})
	for _, kv := range all {
		if packagedCacheBytes.Load() <= packagedCacheBudgetBytes*8/10 {
			break // drop to 80% so we don't immediately re-evict
		}
		if packagedCache.CompareAndDelete(kv.path, kv.entry) {
			packagedCacheBytes.Add(-int64(len(kv.entry.bytes)))
		}
	}
}

// PackagedTrickplayVTT serves the WebVTT cue file that points scrub-
// preview thumbnails to their position inside the sprite sheets.
// Path: /api/play/{itemId}/trickplay/thumbnails.vtt.
func (h *HLSHandler) PackagedTrickplayVTT(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	path := packagePath(itemID, "trickplay", "thumbnails.vtt")
	servePackagedStatic(w, r, path, "text/vtt; charset=utf-8", "trickplay vtt not found")
}

// PackagedTrickplaySprite serves one sprite-sheet JPG. The VTT cues
// reference these by relative name (sprite-NNNN.jpg) so the player's
// resolved URL lands here.
func (h *HLSHandler) PackagedTrickplaySprite(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	n := chi.URLParam(r, "n")
	path := packagePath(itemID, "trickplay", "sprite-"+n+".jpg")
	servePackagedStatic(w, r, path, "image/jpeg", "trickplay sprite not found")
}

// rewriteM3U8URIs walks every line of an m3u8 and appends ?query (or
// merges with an existing ?…) to every line that is a URI. Lines
// starting with '#' are tag lines, except we also have to mutate the
// URI="…" attribute inside #EXT-X-MEDIA tags and the URI="…" inside
// #EXT-X-I-FRAME-STREAM-INF.
//
// We keep the input line endings as-is; shaka emits unix '\n' which is
// what every modern player expects.
func rewriteM3U8URIs(body, query string) string {
	if query == "" {
		return body
	}
	var sb strings.Builder
	sb.Grow(len(body) + 128)
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			sb.WriteByte('\n')
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"), strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(line, "#EXT-X-MAP:"):
			sb.WriteString(rewriteTagURIAttr(line, query))
			sb.WriteByte('\n')
		case strings.HasPrefix(line, "#"):
			sb.WriteString(line)
			sb.WriteByte('\n')
		default:
			sb.WriteString(appendQuery(line, query))
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// rewriteTagURIAttr finds URI="…" inside an HLS tag line and appends
// ?query to the captured value, preserving everything else. Returns
// the line unchanged when no URI attribute is present.
func rewriteTagURIAttr(line, query string) string {
	const marker = `URI="`
	i := strings.Index(line, marker)
	if i < 0 {
		return line
	}
	start := i + len(marker)
	end := strings.IndexByte(line[start:], '"')
	if end < 0 {
		return line
	}
	end += start
	uri := line[start:end]
	return line[:start] + appendQuery(uri, query) + line[end:]
}

// appendQuery returns uri with ?query (or &query) appended.
func appendQuery(uri, query string) string {
	if strings.ContainsRune(uri, '?') {
		return uri + "&" + query
	}
	return uri + "?" + query
}

// manifestCache memoises parsed manifest.json blobs per itemId,
// invalidated by file mtime. The Master / Info / PackagedPlayableBy
// hot path used to ReadFile + Unmarshal on EVERY request — at
// 5-30 ms per call this dominated packaged.m3u8 latency for Zap
// (CDP probe 2026-06-02). Cached lookups are sub-microsecond.
type manifestCacheEntry struct {
	mf    *pkgmanifest.Manifest
	mtime time.Time
}

var manifestCache sync.Map // map[string]*manifestCacheEntry (itemId -> entry)

// ReadPackageManifest parses the manifest.json sitting next to the
// .complete sentinel. Returns nil + an error on read or parse failure
// — the Info handler then logs and falls through to the source-side
// probe so the player at least gets *some* info.
//
// The parsed manifest is cached in-process keyed on the file's mtime,
// so an operator who repackages an item picks up the new manifest on
// the next request without a pod restart.
func ReadPackageManifest(itemID string) (*pkgmanifest.Manifest, error) {
	root := itemRoot(itemID)
	if root == "" {
		return nil, os.ErrNotExist
	}
	mfPath := filepath.Join(root, "manifest.json")
	st, err := os.Stat(mfPath)
	if err != nil {
		return nil, err
	}
	if v, ok := manifestCache.Load(itemID); ok {
		if e := v.(*manifestCacheEntry); e.mtime.Equal(st.ModTime()) {
			return e.mf, nil
		}
	}
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		return nil, err
	}
	var m pkgmanifest.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	manifestCache.Store(itemID, &manifestCacheEntry{mf: &m, mtime: st.ModTime()})
	return &m, nil
}

// writePackagedInfo emits the /play/info JSON shape for a packaged
// item. The fields match what chino-web's PlayerPage panel expects so
// it stops claiming a transcode is happening when none is.
//
// mode="packaged" + reason="…" is the load-bearing change: the player
// branches on mode and stops drawing the "Transcode required" badge
// for that value. Audio tracks are taken from the manifest's actual
// renditions (post-downmix, stereo AAC), not from a fresh probe of
// the source file (which would still report the original surround
// codec).
func writePackagedInfo(w http.ResponseWriter, mf *pkgmanifest.Manifest) {
	video := pkgmanifest.VideoRendition{}
	if len(mf.Renditions.Video) > 0 {
		video = mf.Renditions.Video[0]
	}
	audioTracks := make([]map[string]any, 0, len(mf.Renditions.Audio))
	for i, a := range mf.Renditions.Audio {
		audioTracks = append(audioTracks, map[string]any{
			"index":    i,
			"codec":    a.Codec, // always mp4a in packaged mode
			"language": a.Language,
			"title":    a.Title,
			"default":  a.Default,
			"channels": a.Channels, // always 2 in packaged mode
		})
	}
	// v2 manifests put the title at the top level; v1 manifests had
	// only Source.Path. Prefer Title when present; fall back to the
	// source-path basename for legacy packages.
	filename := mf.Title
	if filename == "" && mf.Source != nil {
		filename = filepath.Base(mf.Source.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"filename":        filename,
		"container":       "cmaf",
		"video_codec":     video.Codec,
		"audio_codec":     "aac",
		"width":           video.Width,
		"height":          video.Height,
		"duration_ms":     mf.EffectiveDurationMs(),
		"mode":            "packaged",
		"reason":          "pre-segmented CMAF on disk; served as static byte-range fetches with no request-time ffmpeg",
		"qualities":       nil,
		"default_quality": video.ID,
		"audio_tracks":    audioTracks,
		"subtitle_tracks": mf.Subtitles,
	})
}
